# Storage and the unified file API

> Native file portability is a mandatory, user-confirmed requirement.
> Stable ID/direct ID-or-path selection, canonical identity, eligible Local native tree moves and first-phase
> [change tracking/audit/SSE](file-changes.md) are implemented in the shared branch, 2026-11-04; final integrated
> verification passed on disposable Postgres/client/built-SPA Chrome fixtures; targeted race checks
> passed. See [identity](file-identity.md).
>
> **User decision: native files must retain complete folder structure, filenames, and original bytes.**
> Leaving Everything must not require its database, a manifest interpreter, or a proprietary export tool.
> This supersedes the anonymous immutable-content layout in the earlier draft. Contracts:
> [file API](../api/api.md), [storage driver](storage-driver.md), [transfer protocol](transfer-protocol.md).

## 1. Two layers, one control origin

The API layer owns user authentication, authorization, search/indexing, quota, transfer sessions and operation
coordination. The current delivery targets official COS and self-hosted Local/COS; other storage adapters remain
optional. Every control request uses one API origin, and clients receive no provider SDK, signing secret or
broad infrastructure credential. Each storage connection selects one [transfer class](transfer-protocol.md):
class A sends bytes between client and object store through presigned part and read URLs, class B between
client and cloud drive through provider sessions and download links, and class C through the application.
Public identity is a stable service UUID; its current native location is
also directly selectable by path.

All clients use `/api/files`: Web, iOS, Android, CLI and third parties. There is no browser-only file API.
Explicit Bearer authentication takes precedence; invalid Authorization never falls back to cookies. Without
Authorization, the session cookie may authenticate, with CSRF validation on mutations. Authorization/file
services are shared. Existing files/folders accept exactly one ID or path at the same endpoint; path callers
need no preliminary ID lookup. Metadata includes id/parentId and path/parentPath with concealed parents
redacted. Native/provider locators remain private outside transfer grants; a service ID is never authorization. Destinations use
`{path}` or `{parent:{id}|{path},name}`. The [API](../api/api.md) lists retained current path shortcuts.
There is no version path segment. Hosted origin is `https://everythingabout.me`; self-hosted origin
is the user's domain or LAN endpoint. The application checks the user, target and quota before issuing any
grant. In classes A and B the client moves bytes into unpublished provider state (a native multipart upload,
a staging file or an open provider session) and the application never carries them; only the application's
commit verifies provider state and publishes at the destination path. In class C the application receives
parts and writes them to Local staging or unpublished native multipart. No class needs an edge function, a
separate upload server or full-file local assembly, and a transfer never falls back to another class. Clients
do not select storage providers or upload origins.

## 2. Native readable layout is a requirement

Given a configured per-user content root, API `/Photos/2026/trip.jpg` is stored as:

```text
<content-root>/
  Photos/
    2026/
      trip.jpg
  Notes/
    diary.md
```

Local disk, WebDAV and NAS contain ordinary files/directories. S3/COS use the same relative key segments;
folder markers preserve empty folders when supported by standard tools. Cloud drives contain native folders
and original filenames, even when their API locates those files through opaque IDs. An owner-specific bucket
prefix/root is acceptable; the entire relative tree below it must match the user-visible tree.

Committed files are individually usable without application decoding. Do not store authoritative content
only as anonymous IDs, hash-addressed objects, chunks, archives, or manifests. Do not replace the native tree
with a continuously exported secondary tree; it is the actual user data. Native reflinks/server-side copies
are allowed if the result remains an independent ordinary file. Shared application references alone are not
an implementation of copying a file to another visible path.

Temporary transfer material, indexes and derived caches may use generated names in a separate system root.
Prefer a sibling/private prefix outside the content root. If the backend only supports a reserved system
folder inside it, disclose the exact reserved name and require users to opt into that root arrangement; do
not take over an existing user folder. Removing temporary/cache material must not damage committed files.

Exit acceptance: stop/settle outstanding operations, open or sync the native content root with standard tools,
and obtain current file names, folder structure, empty folders where the chosen tools support them, and raw
contents. No Everything program or Postgres is needed to interpret those files. Normal provider access and
provider limitations still apply. Native names that a destination filesystem cannot represent need explicit
collision reporting/conversion by the chosen transfer tool, never silent application renaming.

## 3. Storage facts and database responsibilities

Storage is the authority for actual paths, names, existence and bytes. Postgres holds a searchable index,
stable service IDs, parent/name links, permissions, annotations, sessions, operation journals and audit/events.
Each file/folder and the bound root has a persisted service ID. Root mappings live in `file_identity_roots`;
existing node UUIDs are reused when their lifecycle is valid. Rename/move preserves IDs and descendant IDs;
service content replacement preserves ID. Copy allocates fresh IDs for the whole destination tree; deletion
retires IDs and recreation allocates fresh ones. Public IDs remain distinct from native locators, revisions
and workflow IDs. Direct path selection remains a first-class operation.

**Indexed browsing decision, 2026-11-03:** page metadata, folder listings, search and indexed usage are
served directly from Postgres. Opening or refreshing a directory must not issue synchronous storage Stat,
List or Open calls. Storage authority describes where real files live and how mutations are verified; it does
not require a provider round trip on every page view. SQL applies owner/scope filters and keyset pagination
before returning a bounded page. Folder metadata revisions use indexed direct-child membership.

Successful application mutations settle the native result and index together through the durable journal.
Content reads, mutation capture/validation and explicit scans still access the native backend. A content read
is addressed by the native key the index already holds: one native HEAD proves the representation, conditions
are evaluated, and one ranged GET transfers the bytes; in classes A and B the same index lookup produces a
read grant and the client fetches the bytes from the provider itself. Content delivery never rebuilds the namespace by
listing ancestors; when the index cannot name the file, native path resolution is the fallback and the index
is repaired by reconcile, not by the request. External edits
are visible after index synchronization; metadata can temporarily describe a file whose native content has
changed or disappeared. An unavailable/missing index is reported explicitly, never replaced with an implicit
storage scan in the page request. Existing reconcile/import is additive and does not prune deleted external
paths; stale deletion cleanup requires explicit index repair. This does not change direct file portability.

Files/folders share a unique native sibling namespace under each owner/root/parent, using the connection
comparison rules and original accepted display names. Different IDs cannot make duplicate equivalent native
siblings valid. ID resolution checks owner/current native-aware path scope; moving outside scope removes
access and never retargets a path grant. Migration 027 makes parent/name identity canonical, resolves current
paths through the file_entries ancestry view and retains nullable legacy path/key caches. Versioned NFC/full
Unicode case-fold comparison agrees between Go and Postgres. Eligible Local atomic folder moves update one
root linkage/native effect without mandatory descendant identity rewrites; a destination prefix that grows
in bytes or depth falls back to finite preflight. Dynamic ancestry/search can cost O(candidates × depth), and
cache maintenance or flat object copy/delete moves can scale with subtree size.

If the database is lost, files remain directly readable. A scan can rebuild paths and basic metadata. It does
not magically recover account credentials, ACLs, annotations, events, prior app IDs, or ambiguous outside
renames. Those application features need separate backup. Stable-ID continuity requires backing up the
identity/root mapping; rebuilding without it yields new IDs. Owner-to-root configuration/credentials must be
recoverable independently; never infer ownership by exposing all provider data to a new account.

Reconcile/import must index an existing readable tree without rewriting all file bytes. Native IDs can help
recognize external moves; when identity is ambiguous, do not guess from size/name alone. External edits are
supported through explicit scan/change feeds, not instant global linearizability. Operational guidance must
coordinate external writes with active application mutations according to the admitted strategy. A database-only
lock does not lock rclone, Finder or another cloud client.

**Object writes:** object connections use one mutation strategy whose revision tokens are current-object
ETags with operation-specific native `If-Match` / `If-None-Match` where enforced. COS ignores PUT If-Match;
admitted replacement uses gateway coordination and an immediate ETag recheck, without fencing arbitrary
external writers. Bucket versioning is neither required nor consulted. Upload, replacement, mkdir, move, copy
and delete are available; implicit prefix folders are valid parents without fabricated native version evidence,
and reads are validated but not version-pinned. Indexed folders still have real service IDs. Durable publication evidence
and unknown-outcome guards apply to every effect. Initial native/index comparison is a matched observation;
budget counters retain that baseline plus application journal changes and reservations. Synchronization updates
external metadata in the index but does not update budgets. A separate reviewed budget resynchronization is
not implemented; this mode makes no exact global quota or snapshot claim across external writers.

Keep native names intact. Advertise backend name/case/length limits; reject unsupported names and ambiguous
collisions before writing. Preserve display spelling where native Unicode normalization differs. Do not accept
a logical-only name that must secretly become an unrelated encoded native filename. Cross-backend transfers
must preflight collisions and report unsupported names instead of overwriting or silently escaping them.

## 4. File operations must affect storage

| Action | Native result and performance implications |
|---|---|
| Create folder | Real folder or standard empty-folder marker plus index entry |
| Rename/move | Native effect with stable entry/descendant IDs; S3 directory moves require per-key copy/delete |
| Copy | Independent ordinary destination tree with fresh IDs; reflink/provider-side copy preferred |
| Delete | Remove actual files/folders and retire IDs; database detachment alone is insufficient |
| Replace | New bytes become the ordinary file at the same readable path while retaining its ID |

A folder move cannot be promised as one atomic transaction on every backend. Multi-file copy/move/delete
can be partial after failure and must expose durable progress and affected paths. Checkpoint each safe step;
never delete a source until its destination is proven complete. For a non-atomic folder move, copy and verify
the entire captured destination set before starting source deletion; then delete only captured unchanged
source versions. Retries follow the recorded manifest and
validators rather than blindly rescan/delete a changed source. Cancellation stops further safe steps and
reports what remains; it does not promise to undo completed native effects.

Fast operations may return synchronously. Slow ones use `202 + Operation`. Native atomicity and useful
acceleration remain internal; the client sees the same actions, progress and explicit failure guarantees.
Do not advertise stronger consistency than the selected backend/coordination strategy supplies.

## 5. Write and recovery model

Discard the old claim that a database reference swap alone publishes file bytes. The final path now lives
in an external filesystem/storage system; Postgres and provider operations cannot share one atomic transaction.
Use a durable operation journal and recovery workflow:

1. Bind user/root/config generation, target path/identity, expected version and target generation; reserve
   logical quota and serialize conflicting file/folder operations. Capture selected file/parent ID and ancestor
   namespace state; a moved/recreated target or ancestor invalidates admitted work, including ID-selected uploads.
   New ID requests after a completed move may select its current authorized location.
2. Receive bytes into isolated temporary state or an unpublished native upload. Confirm resumable progress
   only after bytes and receipt survive restart. Failed uploads must not be treated as current files.
3. Verify exact size/integrity and current authorization/conditions. Journal the final native publication
   intent before issuing the path-changing request. Use a backend-proven atomic replace/no-replace primitive
   where required; not a racy Stat followed by an unconditional write.
4. Apply the native change at the readable destination. Record the authoritative outcome, then repair/update
   the database index, settle quota and persist the client result. Native success/index failure needs recovery,
   not a claim that the old native file is still present.

Temporary attempt isolation prevents receiving chunks into live files. It does **not** fence a stale request
that is already publishing to the same final path. Never hand the target lock to a newer mutation while an
older native publication could still arrive, unless native conditional/generation fencing proves it harmless.
A lease timeout, expired process, or one successful HEAD is not that proof. Even after the desired file is
visible, all older final-path requests/retries must be terminal or fenced before the guard can be released.
Persist `outcome_unknown`, block
conflicting mutations, and reconcile; if the provider cannot establish safe quiescence, require explicit
operator recovery rather than risk overwrite. The full driver contract defines these limits.

One-file overwrite should preserve old complete bytes until an atomic native switch. Drivers unable to meet
a required create/replace condition must reject that write capability instead of silently falling back to
in-place partial overwrite. Conditions against arbitrary external writers need native support or coordinated
exclusive access; API locks guarantee only the stated application-controlled writer model.

Aborting an open upload can discard temporary data. A definite abort answer is terminal for the whole
allocation: the provider deletes the upload session and every part stored under it, so a part receipt that no
reply ever confirmed must not keep its reservation once that answer exists. Retain liability only while the
abort outcome is unknown, and retry until the provider gives an answer; ordinary cancellation must never need
an operator to unstick it. Once final publication is in flight, cancellation is best
effort and cannot return a definitive aborted state while the native result is unknown. A successful late
publication is reconciled as committed, not hidden only in the database. Temporary cleanup never removes a
committed path merely because its final database result was lost.

## 6. Performance within the portability boundary

Prefer local streaming/range seeks, native rename, server-side copy/reflink, connection reuse, bounded
buffers/backpressure, native multipart and measured fair scheduling across files and parts. Clients share one
session/commit lifecycle; the connection's transfer class decides only how bytes travel.

Parts must reach unpublished native storage during the upload phase. Do not wait until commit to assemble a
full local file and send it in a second whole-file PUT. A class-C receiver streams each part straight to the
provider while hashing it, so no payload is retained and the client's send rate follows the provider's. A
class-A session signs part URLs in windows, binding exact length but no content digest, so the client never
hashes before sending; a class-B session hands the client one provider session. Session metadata lives in
Postgres, with no extra receiver machine. Commit verifies provider state and current conditions, completes
the native upload or provider commit once and settles the index/result; it is not merely a database update or
another full-byte transfer.

The browser runs up to eight queue tasks; failed tasks can retain open sessions, so eight is not a hard cap
on server sessions. Running tasks send numbered parts through one global pool
of sixteen part requests with no extra per-file cap, respecting each session's maxParallelism. A
freed slot immediately goes to the earliest-started file's next part, so files finish in order and one slow part
never idles a connection. After an uncertain part request the client asks the session what the provider or
relay holds before resending; class C receipt hashes are verified against the retained File before reuse. The browser queue exists only in memory for the current Files page/document and survives folder
navigation. Refresh/close discards it without restoring failed tasks or requesting File reselection. Register the
browser-native beforeunload warning while queued, waiting, uploading or finalizing tasks remain; do not rely on
unload-time abort requests. Same-page retry, idempotency and cancellation retain their exact session/Operation
identities. Remove the current owner's old `eam.uploads.v1:<owner>` localStorage record without executing it;
owner-scoped pending delete identities may remain stored independently. Durable server sessions, expiry,
recovery and already accepted Operations are unchanged. Clients retaining session identity and source bytes
can use the resumable API across reconnects/restarts; the browser does not restore its queue after reload.

Before allocation, browser tasks acquire an ephemeral Web Lock per owner and normalized target path, retained
through retryable failures until completion, cancellation or page unmount. Browser destruction releases it.
A new selection can automatically replace an abandoned `open` session without another confirmation once it
owns the lock; an actual tab conflict fails inline. Missing/failed ownership never permits an unlocked abort.
This coordinates cooperating same-origin tabs in one browser, not external clients, origins or devices, and
supplies no server authority. Exact session/Operation checks and publication guards remain authoritative;
see [transfer protocol](transfer-protocol.md#3-upload-lifecycle) for the full lifecycle.

Readable final paths can cost an extra publish copy on S3 or require per-file copies for a folder move. These
are accepted costs when unavoidable; never optimize them by quietly replacing the native tree with anonymous
objects. Benchmark staging-to-final copy, request overhead, gateway bandwidth, retry bytes and directory moves
at large scale. Local/S3 implementations should be validated first, while WebDAV/drive constraints remain
explicit in the driver contract.

Class A and B reads come straight from the provider through renewable grants; class A rounds grant expiry to
fixed windows so repeated grants stay browser-cacheable. Application-served bytes (class C content, thumbnails
and converted previews) use private caching only with authorization and revision-safe keys; otherwise bypass
shared cache. Active same-origin content still needs safe MIME/disposition and preview isolation.

## 7. Changes, snapshots and audit

Confirmed native effects settle index/result, durable owner-scoped change facts, recursive markers and effect
audit together. Short owner-head locking gives commit-safe ordered progress; effect receipts deduplicate
recovery. No provider I/O occurs under the head lock. Metadata may expose lastChangedAt/changeToken;
namespace markers propagate subtree movement without rewriting every descendant, while sibling edits do not
advance unrelated file markers. Concealed aggregate coverage omits markers. This observes indexed state,
not historical bytes or instant external-writer state.

Immutable repeatable-read snapshots capture indexed metadata and the committed head. Current construction is
synchronous: at most 100,000 entries/64 MiB, 25-second request window, 24-hour lifetime and ten active resources
per principal. Oversize/timeouts fail explicitly with 503; there is no asynchronous snapshot Operation.
Pull uses policy-bound private cursors and projected retained endpoints. Owner sessions have access; ordinary
tokens need explicit changes/read, plus subscribe for durable subscriptions/SSE. Old tokens and save links
receive neither new right implicitly. Monotonic policy generations prevent old cursors reviving after policy
expansion and restoration.

SSE subscriptions persist matching state and coalesced notification IDs, with seven-day renewable leases.
Wakeups only announce availability; independent pull cursors provide recovery. Streams are bounded to 25
seconds and recheck grants during polling. Webhooks and outbound delivery are deferred. GET /api/audit is a
separate owner-cookie collection with captured actor/policy and request/effect outcomes, distinct from timeline
cards and the older account activity log. Third-party read admission fails closed if durable audit cannot be
recorded; incomplete completion telemetry is explicit and never proof of client delivery.

Rows have a 90-day change expiry and an explicit file-changes-prune CLI that removes only an expired
contiguous feed prefix while retaining effect receipts. No automatic retention scheduler is wired. Audit
history currently remains indefinitely; no 365-day deletion policy is implemented. Additive scan/import records external creations/updates, not outside deletions or guessed
rename continuity; unchanged scans emit no facts. Audit export/automatic retention, external deletion repair,
native watcher health and external-writer budget resynchronization remain separate work. Full contracts and
observation limits are in [file changes](file-changes.md) and the [API](../api/api.md).

Migration 032 durably binds the change stream to the current configured root namespace on first use. Root
mismatches reject access instead of silently rebinding. Producers take shared root protection before index
locks and the short ordering head afterward; repeatable-read consumers take session root protection before
their MVCC snapshot. Reset takes exclusive root protection. After database restore/mapping rebuild or every
rebind, including A → B → A, stop writers, settle accepted work and explicitly run file-changes-reset with
--writers-quiesced. It refuses pending guards/nonterminal Operations, rotates epoch, sets floor=head, expires
baselines/subscriptions and clears markers while preserving positions, receipts, IDs, audit and native bytes.
A same-namespace restore still requires reset. See the [root runbook](file-changes.md#durable-root-binding-and-restorerebind-procedure).

## 8. Acceptance and implementation

This contract is verified by conformance tests plus explicit deployment gates. Neither test suites nor code
presence attest a production root admission, a live provider configuration or a performance SLA.

Before replacing the current implementation:

- Prove direct exit using local tools and rclone/provider CLI with the database unavailable; no special export.
- Scan/index an existing native tree without content rewriting; verify file counts, names, sizes and available
  checksums. Keep old data safe through the transition.
- Test native names/case collisions, empty folders, owner/root isolation, external edits and index repair.
- Test lost responses before/after final native publication, stale requests after lease loss, unknown-outcome
  blocking, conditional replacement, cancellation and partial folder operations.
- Test upload receipts/restart/expiry and logical versus physical budget accounting.
- Keep application metadata backup/restore distinct from ordinary file portability.

Integrated disposable tests do not move runtime data, and a passing suite is not evidence that a specific
deployment is admitted. This documentation does not move runtime data.
