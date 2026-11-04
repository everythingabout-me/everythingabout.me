# File changes, snapshots, audit, and notifications

> Implemented first phase in the shared identity branch, 2026-11-04. Disposable Postgres, full Go/client and
> built-SPA Chrome acceptance passed; targeted race checks passed. This reference follows the
> running handlers and migrations 026–032. It selectively replaces the earlier
> draft: incremental pull, immutable indexed snapshots, durable subscriptions, SSE wakeups and owner audit
> are implemented. External webhooks, asynchronous snapshot construction and automatic retention maintenance
> are deferred. Runtime forms are in the [file API](../api/api.md); identity is in [file-identity.md](file-identity.md).

## 1. Three records with different purposes

| Record | Meaning |
|---|---|
| Operation journal | Admitted intent, native checkpoints, partial effects, guards and recovery |
| File change journal | Confirmed transitions of the indexed file tree |
| File audit | Request admission, authorization/outcome evidence and confirmed worker effects |

Receiving a part, accepting an Operation or timing out a native request is not a successful file change.
Application settlement records the verified index/result transition, change fact, recursive markers and
confirmed effect audit in one Postgres transaction. A durable effect key deduplicates replay, including after
native success followed by database failure. Unknown native effects retain the existing guards and recovery
state. File changes contain metadata and retained ancestry, never historical file bodies. Timeline `events`
remain personal chronicle cards and do not provide these infrastructure contracts.

Each owner has a committed head and root epoch. Producers acquire the short owner-head lock after index/quota
work, allocate a position and hold the lock until commit. Rollback rolls back allocation. Readers cannot skip
an earlier uncommitted change because a later sequence value committed first. No native or network I/O occurs
under that lock. Internal positions, effect keys and actor data are not serialized in the public feed.

## 2. Time, revisions and recursive markers

| Metadata field | Meaning |
|---|---|
| `sourceModifiedAt` | Available native/source time; may be preserved or unknown |
| `updatedAt`, `revision` | Indexed resource metadata and direct folder membership |
| `contentRevision` | Saved native-evidence-backed file content generation; not a folder hash |
| `lastChangedAt` | Service observation time for the latest confirmed direct change; folders also include descendant changes |
| `changeToken` | Opaque equality marker for that effective observed change state |

`lastChangedAt` and `changeToken` are optional when no marker exists or exposing an aggregate could disclose
hidden descendants. Do not treat absence as proof that an entry never changed. The marker token is a random
change UUID, not an encoded global position. Times are clamped against prior marker time; tokens distinguish
changes with equal times. Neither is a native validator, a write precondition, a directory hash or a cursor.

Creation, content replacement, metadata edits, deletion and movement update the affected entry and surviving
old/new ancestors through the account root. Copy affects destination ancestry. Unchanged scans and idempotent
result replay do not emit new changes or advance markers. Folder recursive markers are separate from ancestor
namespace markers: a sibling edit changes its parent aggregate without changing unrelated files. A subtree
move advances effective descendant location markers through ancestry without rewriting every descendant.
Metadata revisions likewise incorporate relevant namespace state; contentRevision survives a proven rename.

A scoped caller sees markers only inside its authorized view. Concealed parent IDs/paths are redacted, and
folder aggregates spanning hidden roots are omitted. Marker freshness is limited by index observation:
external edits do not become instant changes merely because the native tree changed.

## 3. Grants and binding

Owner sessions have change-read and subscription capabilities. Bearer credentials require explicit `changes`
for snapshots/pull and explicit `subscribe` for subscription management/SSE. `changes` requires file `read`;
`subscribe` requires both `changes` and `read`. Timeline `events` is independent. Existing tokens, save links
and newly created Connect apps receive neither new grant implicitly. Connect exposes separate checkboxes;
CLI creation accepts `--changes` and `--subscribe`, both default false.

Cursors, snapshots and subscriptions bind owner, creator principal, native root/configuration, scope, hidden
roots, comparison rules, grant bits and token policy generation. A changed policy or revocation advances
`policyVersion`; restoring a former policy does not revive its old continuations. Renaming a connection and
saving an identical policy preserve the generation. Invalid/mismatched change cursors return `400 invalid_cursor`;
a denied current grant returns `403 forbidden`. An old snapshot/subscription outside the current policy binding
is not found. Expired retained baselines or changed root epochs require a new baseline.

IDs never confer access or retarget path grants. Every page rechecks current authority. An ID watch follows
its entry only while authorized; a path watch stays at its original location.

## 4. Snapshot bootstrap

Create a complete indexed baseline before mirroring current state:

```http
POST /api/files/change-snapshots
Content-Type: application/json

{"target":{"path":"/Notes"},"kind":"folder","recursive":true}
```

`target` is exactly one canonical service UUID or absolute path. The declared kind must be `file` or `folder`;
a file must use `recursive:false`. Both target modes require an existing visible entry at creation. A file
snapshot contains that file. A folder snapshot contains the folder itself, then its immediate children or all
descendants according to recursive, with the remaining entries ordered by canonical path. Hidden roots are
excluded. The declared kind must match, otherwise `409 target_changed`.

The index metadata and committed change head are captured in the same repeatable-read transaction. Metadata
is fully materialized and committed before `201`; no partial ready baseline is returned. The response is:

```json
{
  "id":"b9ca691b-27de-4f86-bd5a-b853efc2e498",
  "state":"ready",
  "filter":{"target":{"path":"/Notes"},"kind":"folder","recursive":true},
  "totalEntries":12,
  "entriesUrl":"/api/files/change-snapshots/b9ca691b-27de-4f86-bd5a-b853efc2e498",
  "changesCursor":"<opaque checkpoint>",
  "createdAt":"2026-11-04T08:00:00Z",
  "expiresAt":"2026-11-05T08:00:00Z"
}
```

`GET entriesUrl?limit=200&cursor=...` returns these snapshot fields plus `items` containing complete file
metadata and `nextCursor` (null on the last page). Saved metadata is immutable across pages; it is not hydrated
from later live rows. Every page still rechecks authorization, current target access, epoch and retention.
The snapshot freezes indexed metadata, not native bytes. Ordinary list/search pagination is not equivalent.

Current bounds are 100,000 entries including the target, 64 MiB of serialized metadata, a 25-second request
window with a 20-second SQL statement timeout, 24-hour lifetime and ten active snapshots per principal.
Oversize materialization returns `503 snapshot_too_large`; timeout/serialization/lock failures return
`503 temporarily_unavailable`. There is no asynchronous `change_snapshot` Operation in this phase.
Overlapping native effects that have started or whose outcome is unknown return `409 operation_in_progress`;
queued unpublished work can settle after the captured head. Admission overflow is `409 snapshot_limit`.

Stage every page locally, then atomically install that view and its changesCursor. Apply subsequent changes
and each nextCursor in the same local transaction. `DELETE /change-snapshots/{id}` returns 204 and releases
retained metadata, including after expiry while the same policy still owns the resource. Creation also
performs bounded cleanup of that principal's expired snapshots.

## 5. Incremental pull and privacy

`GET /api/files/changes?cursor=<checkpoint>&limit=200` accepts only one cursor and limit, with a page size
of 1–1000 (default 200). A missing cursor returns `400 invalid_cursor`; there is no implicit future-only skip.
The filter is sealed into the checkpoint and is not supplied again as query fields.

```json
{
  "items":[],
  "nextCursor":"<opaque next checkpoint>",
  "hasMore":false
}
```

`nextCursor` is always present, including empty pages. `hasMore` means additional authorized matching items
within this pull cycle's captured committed head; it says nothing about future writes. Continue until false,
then retain the returned checkpoint for the next cycle. Snapshot checkpoints retain their original 24-hour
expiry; subscription checkpoints retain their lease expiry. Pulling does not slide either lifetime.

Items have `id`, `type`, `coverage`, `origin`, `observedAt` and optional `entryId`, `before`, `after`, `path`,
`impactPaths`, `affectedFolders` or terminal `reason`. Item IDs are stable change UUIDs, distinct from entry IDs.
The current public types are `upsert`, `remove`, `moved`, `invalidated` and `watch_ended`; coverage is `entry`
or `subtree`. Before/after and affectedFolders carry saved file metadata with concealed parents redacted.

| Projection | Client action |
|---|---|
| `upsert` | Replace/create the local entry from after metadata |
| `remove` | Remove retained entry/location; a live source row is not required |
| `moved` | Apply visible before/after linkage; subtree coverage changes descendant location through ancestry |
| `invalidated` | Refresh the entire watched view using a new snapshot |
| `watch_ended` | Stop the ID watch; reason is deleted or access_lost |

Both move endpoints must be visible to expose a full moved item. A departure becomes remove without its
concealed destination; an arrival becomes upsert or a bounded subtree invalidation. A hidden ancestor effect
is projected at the authorized watched boundary without disclosing the ancestor's location or ID. Scope and
filter must match the same endpoint. Public feed data excludes actor attribution, credentials, native locators,
raw positions and account-wide counts. SQL filters authorized matching facts before the public page limit,
so hidden-only activity does not create sparse hasMore pages. Cursors are authenticated, encrypted, padded
and randomized; a different cursor string is not evidence that a matching change occurred.

Authorized affectedFolders patches update parent/direct revisions and recursive markers in the same local
transaction as the entry effect. Only matching visible folders belong in these patches. A nonrecursive watch
is not a source of complete recursive aggregate updates for filtered-out deep edits; use recursive coverage or
refresh metadata when that aggregate matters.

A complete ID/parent tree can apply a fully visible atomic subtree move as one root parent/name update and
invalidate derived paths locally. Clients lacking the required membership or endpoints refresh their entire
watched view. Narrower ID views receiving an invalidation must bootstrap again; continuing that cursor returns
`409 baseline_required`. Targeted subtree snapshot splicing has no server barrier protocol and is deferred.

An ID deletion or loss of current path access produces an authorized cleanup remove followed by watch_ended.
Its terminal checkpoint then returns `409 watch_deleted` or `409 watch_access_lost`. It does not revive if an
ID returns later. A fixed-path watch stays at the location and can observe later replacement, but first-phase
baseline creation still requires an existing entry. Old snapshots do not grant access to a now-concealed target.

## 6. Durable subscriptions and SSE

```json
{"target":{"id":"1411944e-bcbe-49a2-aa30-7196f3dc09b4"},"kind":"folder","recursive":true,"delivery":{"type":"sse"}}
```

`POST /api/files/subscriptions` returns 201 with `{id,filter,state,expiresAt,changesCursor,eventsUrl}` and an
origin-relative Location. The checkpoint starts at the committed head, so creation provides future-only
watching without a baseline. It does not certify current file state. Use the baseline's checkpoint when
building a complete local mirror. File watches match their entry; folder watches include themselves and direct
children, or descendants when recursive. ID mode follows identity; path mode follows location. An ID target
must exist and be visible. A non-root missing path may be watched when its immediate authorized parent exists
as a folder. This future-only path watch does not create a snapshot baseline for an absent entry.

`GET /subscriptions` returns `{items:[...]}` for this principal/root/current policy, at most 100 resources.
No filter or pagination query is accepted. GET/DELETE `/subscriptions/{id}` inspects/deletes a resource;
delete returns 204. POST `/subscriptions/{id}/renew` extends an unexpired valid lease by seven days after
retention/epoch checks. Limits are 100 active subscriptions per principal and a seven-day lease. Expired leases
and checkpoints return `410 cursor_expired`; recreate and bootstrap deliberately rather than skip history.

GET eventsUrl serves SSE. Each stream lasts at most 25 seconds, polls durable matcher state approximately
once per second, disables proxy buffering, and returns after sending one coalesced notification. Reconnect
and pull even without a notification. The event name is changes and its data is only:

```json
{"subscriptionId":"<subscription UUID>","type":"changes_available","notificationId":"<notification UUID>"}
```

The SSE id and Last-Event-ID identify the durable notification, not consumer progress. Matcher state,
coalesced notification ID and terminal reason survive process restart; concurrent pollers retain the same
notification for the same committed head. Consumer change cursors are independent and never advanced by a
wakeup. Streams recheck token expiry/revocation/policy on each poll. Header-authenticated clients use streaming
HTTP; same-origin cookie clients can connect without putting secrets in URLs. Reconnect/app resume should
always pull, and consumers should periodically pull to recover from missed wakeups.

Only delivery type sse is accepted. This phase performs no outbound webhook requests, receiver validation,
secret issuance, signing or retry delivery. Those features require a separate reviewed implementation.

## 7. Owner audit

`GET /api/audit?limit=50&cursor=...` is a separate plain JSON collection `{items,nextCursor}` with Problem
Details errors. It requires an owner cookie session; explicit Authorization is rejected rather than falling
back to that cookie. App/file-read grants do not authorize audit access. Only limit (1–100, default 50) and
cursor are accepted; app/path/action/time filters and export are not implemented. Results use reverse
observed-time/UUID keysets. This recent-management view is not a commit-safe incremental feed.

Rows expose action, outcome, allowed (nullable), status, optional bytesWritten/reasonCode/entryId/path,
actor, requestId/operationId when known, origin and observedAt. Actor snapshots retain principal/grant ID,
self-reported label, scope and captured grants/policy generation where available. They survive credential
rename/revocation; labels are not verified publisher identity, and no separate app registry exists yet.

File request admission/completion and confirmed settlement effects are distinct records. Authenticated
third-party GET/HEAD admission must persist before releasing metadata/bytes; a persistence failure returns
`503 audit_unavailable`. Owner reads and request telemetry can log incomplete audit when persistence fails.
Confirmed mutation effects record audit transactionally with settlement and recover native success if that
transaction fails. Completion records bytes accepted by the server writer, including zero, not proof of client
delivery. Crashes can leave admission without completion. Known-owner denials are recorded; unknown-owner
authentication failures use bounded platform logs rather than fabricated account rows.

No tokens, cookies, passwords, file bodies, raw search text, signed transfer URLs, request headers or raw
provider error payloads belong in audit records. The older audit_log/account activity path still handles
Connect/account/event activity; it is separate from the new file_audit owner collection. There is no claimed
365-day audit deletion scheduler, tamper-proof compliance, publisher verification or complete audit export.

## 8. Persistence, observation and remaining work

| Migration | Delivered responsibility |
|---|---|
| 026 | Stable root/node identities, parent links and versioned sibling comparison |
| 027 | Canonical parent/name paths, derived path/key caches, membership revisions and identity-bound content generations |
| 028 | Commit-safe owner heads, durable change/effect receipts, markers and file audit |
| 029 | Explicit token rights/policy generation; durable subscriptions and matching state |
| 030 | Immutable materialized snapshots, page rows and admission bounds |
| 031 | Separate ancestor namespace markers for effective descendant change state |
| 032 | Durable stream/root namespace binding and explicit reset protection |

Change rows have a default 90-day expires_at. The explicit command removes only an expired contiguous feed
prefix and advances the retained floor, retaining effect/deduplication receipts and all file bytes:

```bash
eam file-changes-prune --owner-id UUID --dry-run
eam file-changes-prune --owner-id UUID
```

Dry-run rolls back the maintenance transaction. There is no automatic maintenance scheduler; operators must
schedule/monitor the explicit command. Ninety days is the row lifetime, not an automatic disk-reclamation SLA.
Pruning does not delete audit history, reset identity or remove unexpired feed rows. File audit currently
retains history indefinitely; no 365-day retention/deletion guarantee is implemented. Feed resumes below the
retained floor or with an expired epoch return 410 cursor_expired. Snapshot expiry is 410 snapshot_expired.

### Durable root binding and restore/rebind procedure

Migration 032 persists the change head's root namespace, derived from owner, connection ID, configuration
generation and content/system roots. First use binds an empty namespace once. A later mismatch is rejected;
normal endpoints and repeated migrations do not rebind or rotate a healthy stream automatically. Pull/root
admission can return 410 reset_required; snapshot creation reports 503 change_root_reset_required. A malformed
or differently bound old cursor can fail as invalid_cursor before the durable root check.

After database restore, mapping rebuild or any root/configuration rebind, stop all application and external
writers, settle accepted work, then inspect and apply an explicit reset against the intended current config:

```bash
eam file-changes-reset --owner-id UUID --writers-quiesced --dry-run
eam file-changes-reset --owner-id UUID --writers-quiesced
```

Reset refuses any remaining mutation guard or nonterminal Operation. The writers-quiesced flag confirms an
operator action; database checks cannot fence an external provider request or rclone process. The command
binds the current configured namespace, rotates the root epoch, sets the retained floor to the existing head,
expires all subscriptions/snapshots and clears recursive/namespace markers. It preserves positions, effect
receipts, file IDs, audit and file bytes; it does not scan, move or delete native content. Clients must create
new baselines. Reset even when the restored namespace/APP_SECRET/epoch appear unchanged, and for each rebind
including A → B → A: returning to an old configuration must not revive old checkpoints.

Producers acquire shared root advisory protection through BindRoot before index write locks, then acquire
the short ordering-head lock after index/quota work. Reset acquires exclusive root protection before the head.
Repeatable-read snapshot/feed readers acquire session-level shared root protection before beginning their
MVCC transaction and verify the durable namespace inside it; a snapshot cannot predate a reset it waited for.
No native I/O occurs under these database locks. See [root implementation](../../backend/internal/filechanges/root.go)
and [maintenance commands](../../backend/cmd/eam/file_changes.go).

Explicit additive scan/import emits observed creation/update facts, with external attribution and no invented
native author. An unchanged import emits nothing. It does not prune external deletions, infer an outside
rename from similar bytes, track exact outside-writer quota changes or detect all same-size preserved-time
edits. Native watcher/feed overflow, automatic reconciliation, complete outside-deletion repair, release-scale
retention operation and webhook delivery remain separate work. The implementation preserves current COS ETag,
EO signed-part, native upload, admitted writer and unknown-outcome limits in the [storage contract](storage.md).

Final integration evidence belongs in the [implementation plan](storage-implementation-plan.md). Code presence
and disposable tests do not attest a production root admission, live EO/IAM/COS configuration or performance SLA.
