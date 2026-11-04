# Unified file API

> Source-aligned contract, 2026-11-04. Stable ID/direct ID-or-path selection, canonical parent/name identity,
> eligible Local native tree movement, change facts/markers, indexed snapshots, incremental pull, SSE subscriptions
> and owner audit are implemented and verified with disposable Postgres, client and built-SPA Chrome tests;
> targeted race checks passed. See
> [identity](../design/file-identity.md), [changes/audit](../design/file-changes.md) and the
> [implementation state](../design/storage-implementation-plan.md).
> P2 read resources and same-origin GET/HEAD content are implemented.
> P4 implements PUT, native namespace actions and Operations for explicitly admitted coordinated Local roots.
> Writes default off and require a verified root baseline. [COS/S3 mutation adapters](../design/storage-p4-objects.md)
> additionally require exact-connection native probe evidence and durable single-gateway spool. Limited
> [recorded COS ETag observations](../design/storage-p4-objects.md) do not establish every mutation, native
> completion condition or live deployment. P5 implements durable numbered-part uploads on admitted
> roots; thumbnails remain an authenticated 501 placeholder. See [P5 lifecycle and costs](../design/storage-p5.md),
> [P2 reads](../design/storage-p2.md) and
> [P4 admission, active endpoints and validation](../design/storage-p4.md) for current limits;
> the sections below remain the complete target contract.
>
> This is the target file API, replacing earlier route, envelope, identity, and upload decisions.
> There is no requirement to preserve old clients or provide compatibility adapters. The retired protocols
> are documented separately in [legacy-api.md](legacy-api.md). Account login, Connect and event authoring
> remain separate APIs. P6 switches Web metadata, namespace mutations, resumable uploads and all media bytes
> to this contract. The browser upload queue lives only in memory for the current Files page/document, surviving
> folder navigation but discarded after refresh/close. It restores no failed tasks and requests no File reselection.
> Durable server sessions and accepted Operations retain their recovery/expiry behavior independently.
> `/api/fs/*` and root legacy file routes return 410 and issue no new native authority.
> Read [P6 client acceptance](../design/storage-p6.md) and [authority-drain runbook](../design/storage-p6-backend.md)
> before deployment. Route removal does not revoke previously issued PUT/STS/multipart authority.
> Architecture: [storage.md](../design/storage.md). Driver contract: [storage-driver.md](../design/storage-driver.md).
> **Mandatory:** native storage must retain complete readable folders, original names and raw files. Users
> must be able to leave through ordinary native tools without the database or an Everything export.
> The database is an index/application store; anonymous authoritative content is rejected.

## 1. Contract and addressing

Every metadata request and file byte uses the configured origin: hosted `https://everythingabout.me`,
self-hosted the operator's domain or LAN endpoint. Signing secrets, broad temporary credentials and storage
SDKs never reach clients. The official COS transport returns narrowly scoped signed part requests under this
same origin; their native upload parameters are opaque to the client. This user-confirmed transport supersedes
the former blanket prohibition on client-visible provider signatures/parameters. No provider-origin redirect
or client-selected upload host is allowed.

With `FILE_UPLOAD_INGRESS=edgeone`, control requests stay on the application server. A browser obtains a signed
part request from the authenticated authorize endpoint and PUTs its body to `/api/files/transfer/<object-key>`
with the returned signed query/headers. EdgeOne ordinary forwarding removes `/api/files/transfer`, sets the
COS bucket Host and sends the request directly to COS. No edge function or additional Go upload service is
needed. COS validates the signature and bound Content-MD5; application confirm queries COS and persists a
verified native part receipt. The application refuses transfer traffic and ordinary byte PUTs before reading
their bodies (`503 edgeone_route_required`); maxSinglePutBytes=0 in this mode. Uploads are uncached and the
transfer path must never redirect or fall back to the application. Strip application cookies, Authorization
and X-CSRF-Token on COS-bound transfer requests; signed query parameters remain untouched.

Self-hosted `embedded` mode receives parts in `eam server` with the normal Cookie/Bearer/Origin/CSRF boundary.
Sessions advertise their transport. Logical session/part identity, recovery, cancellation and server-controlled
commit remain shared; only the byte transport differs.

**One base, `/api/files`, serves Web, iOS, Android, CLIs and third-party clients.** There is no separate
browser route or separate ID-only resource family. ID and path select the same resources. All returned
transfer/operation locations are origin-relative below this same base. In examples, `B` means `/api/files`.

Authentication is selected explicitly at the same endpoint:

- If Authorization is present, require a valid supported Bearer token and use only that principal/scope.
  Invalid/expired/unsupported Authorization returns 401; never fall back to an ambient session cookie.
- Otherwise authenticate the httpOnly session cookie. All cookie-authenticated mutations, including raw PUT,
  upload parts and cancellation, require CSRF validation and approved Origin handling. Cookies use Secure
  in production and suitable SameSite policy. Browser media GET can use the same content endpoint.
  A mutation is denied unless the session-bound `X-CSRF-Token` from `GET /api/files/me` validates and its
  Origin matches an explicitly configured `FILE_COOKIE_ORIGINS` entry. Host/forwarding headers do not establish trust.
- Both paths invoke identical authorization/file services. Cookie authentication is not a reason to create
  another API. Native/third-party clients can use Bearer; Web may use cookie or explicit Bearer.

API paths have no version segment. Optional fields may be added; required behavior cannot silently change.
Any future incompatible change needs an explicit contract decision and rollout plan, not an assumed `/v1` path.

### Identity, selectors, and unique paths

**Every file and folder, including the root, has an opaque service-managed `id`.** Metadata returns `id`,
`parentId`, `name`, `path` and `parentPath`; the root has null parent fields. IDs are owner-bound, never reused,
and distinct from provider/native IDs, storage keys, content revisions and temporary upload/operation IDs.
The root ID is a real persisted mapping for the bound owner/root, never a synthetic or per-request value.
They are public identity, not access capabilities. Rename/move within an owner preserves the entry's ID and
all descendant IDs. A service-mediated content replacement preserves file ID even if the native inode/object
changes. Copy creates fresh IDs for every copied entry; deletion retires IDs, and recreation at the same
path has new IDs. Uncertain native identity never permits silently attaching an old ID to a different file.

**ID identifies the resource; path identifies its current location. Both are first-class selectors.** A request
using a path performs resolution server-side in the same call. Clients do not have to retrieve or cache IDs.
An ID request resolves the current authorized location, including after earlier completed rename/move; a path
request selects what exists at that location at admission. Once admitted, mutations bind the selected identity
and location under guards; a concurrent move/recreation cannot silently retarget a queued mutation or upload.

Existing-resource query endpoints accept exactly one nonempty `id` or `path` parameter, never both, even if
both happen to identify the same resource. Reject duplicates, empty selectors, both or required-but-missing
selectors with `400 invalid_selector`. JSON uses `EntrySelector = {"id":"<UUID>"} | {"path":"/Notes/a.md"}`,
with the same exclusivity. Optional-root endpoints explicitly state their default; an empty selector is not
an implicit root. Public IDs are canonical UUID strings, opaque to callers; they are never interpreted as
paths or mixed into path route segments. Malformed UUID selectors return `400 invalid_selector`. Existing node UUIDs provide entry IDs; `file_identity_roots` persists root IDs
separately from nodes. The driver's native ID is never substituted for the service ID.

Destinations do not accept an existing file ID. Use `Destination = {"path":"/Notes/new.md"}` or
`{"parent":{"id":"1f12cfd6-b93b-41ad-ae71-52a4299c3421"},"name":"new.md"}`; the parent may instead use `{"path":"/Notes"}`.
Exactly one destination form is allowed. `name` is one valid native filename segment. Parent selectors must
resolve to an existing authorized folder. New resource creation allocates its ID server-side; clients cannot
pick or resurrect an ID. For raw streaming creation, query `parentId` or `parentPath` plus `name` is the
parent-and-name equivalent; it is exclusive with `id` and full `path`. Reject mixed destination forms,
both parentId and parentPath, or missing/empty/duplicate parent or name fields with `400 invalid_selector`.
A supplied nonempty name that violates filename rules returns `400 invalid_path`, not a selector ambiguity.
See sections 4, 6 and 7 for creation rules.

Paths are absolute within the account file root: `/` is the root, `/Notes/a.md` is a descendant, matching the
readable native tree below the owner's content root. A scoped credential's paths are never silently rebased.
**Each owner has one shared file/folder namespace with exactly one entry per accepted path.** Enforce unique
`(owner, parentId, comparisonName)` using the connection's declared case/Unicode comparison rules, alongside
native conditional admission. Preserve original accepted spelling. A file and folder cannot share the same
name in one parent; distinct parents may contain equal names. Reject equivalent-name collisions rather than
creating duplicates with different IDs. Comparison versions are nfc-sensitive-v1 and nfc-fold-v1; the latter
uses Unicode full case folding with the same generated mapping in Go and Postgres. Original accepted spelling
is preserved. Imports from providers allowing duplicate siblings report conflicts
until resolved explicitly; never hide, silently rename, or ID-only expose an ambiguous native tree.

Preserve spaces. Reject empty segments, `.`, `..`, backslashes, control characters, trailing slashes except
`/`, and invalid UTF-8. NFC may be used for comparison but must not rewrite native names. Paths travel in
query values or JSON strings. URL-encode query values with a standard builder (`+` as `%2B`, `#` as `%23`,
`?` as `%3F` inside filenames). Decode query encoding once; JSON strings are literal paths. A filename that
looks percent-encoded is not decoded again. Enforce backend segment/depth/path limits before effects.

Path-scoped authorization remains bound to configured native-aware path boundaries, not IDs. Resolve either
selector to current ancestry before authorization; knowing an ID never preserves access after a move outside
scope. New content at the authorized path falls under that scope. Moving a scope root does not retarget the
grant. Check both source and destination for move/copy, and recheck queued effects. Do not follow symlinks
outside the bound root. Missing, deleted, foreign-owner and concealed IDs/paths return 404 without disclosing
current location. Root cannot be moved/deleted. Metadata returns parentId and parentPath only for an
accessible parent; both are null for the account root or a concealed parent of a scoped root. The entry's
own absolute path remains visible; redaction never creates a handle into an unauthorized parent.

The canonical index is a parent/name tree with stable IDs. Current paths resolve through ancestry in the
file_entries view; legacy path/key columns are nullable derived caches. Eligible Local native folder moves
update the root linkage with one native effect, without mandatory descendant identity/path rewrites. The fast
path requires a destination prefix no longer in UTF-8 bytes and no deeper in segments than the source; growing
prefixes/depth use the finite manifest to preflight descendant path limits. Generic flat object moves still
perform per-entry copy/verify/delete. Search through dynamic ancestry can cost O(candidates × depth), and derived
cache work can scale with descendants; no universal O(1) rename/search guarantee is made.
Ordinary native files remain readable without IDs or Postgres. Preserving IDs after database loss requires
backing up their mapping; a native scan alone can rebuild files with new IDs, not recover arbitrary old IDs.

## 2. HTTP and representations

JSON successes are the resource directly, without an `ok/data` envelope. Collections use
`{"items":[],"nextCursor":null}`. Errors use `application/problem+json`:

```json
{
  "type": "urn:eam:error:precondition_failed",
  "title": "Precondition failed",
  "status": 412,
  "code": "precondition_failed",
  "detail": "The file changed before publication.",
  "requestId": "req_..."
}
```

`code` is stable; `detail` is an English explanation. Raw content, HEAD, `204`, and `304` are not wrapped in
JSON. Dates are RFC 3339 UTC. Byte counts are integers in `0..9007199254740991`; service/backend limits may
be smaller and are discoverable. No provider error body, secret, or internal native reference is returned.

File metadata JSON:

```json
{
  "id": "f7ca1104-12f0-4bbf-8c77-41d341f7d120",
  "parentId": "1411944e-bcbe-49a2-aa30-7196f3dc09b4",
  "path": "/Photos/2026/a.jpg",
  "parentPath": "/Photos/2026",
  "name": "a.jpg",
  "kind": "file",
  "revision": "nr_...",
  "contentRevision": "cr_...",
  "size": 248123,
  "mediaType": "image/jpeg",
  "sourceModifiedAt": "2026-10-29T08:00:00Z",
  "createdAt": "2026-10-30T04:00:00Z",
  "updatedAt": "2026-10-30T04:00:00Z",
  "lastChangedAt": "2026-11-04T08:00:00Z",
  "changeToken": "74a503df-f004-4cee-ae2c-33da77ab729e",
  "checksum": null
}
```

Metadata, folder lists, search and indexed usage are database views. They issue no native Stat/List/Open
requests and retain the indexed path spelling, attributes and timestamps. Native files without index rows
appear after an explicit scan/import or successful application mutation. Missing indexed paths return 404;
an unavailable index returns 503 without a storage fallback. The root has a persisted stable ID; its metadata
may use synthetic attributes, but its identity is not synthesized. Unknown root times use
`0001-01-01T00:00:00Z`; `sourceModifiedAt` and checksum are null when unavailable.

External edits may leave metadata stale until index synchronization. Existing reconcile/import adds or updates
observed entries; it does not prune externally deleted rows. Content requests read actual native bytes and can
return 404 even while an old index entry is visible. Application mutations settle their verified native effects
into the index before reporting success. These indexed-read semantics supersede P2's live native observations.

`revision` describes indexed file/folder metadata; it changes when that indexed representation changes.
Folder revisions also include indexed direct-child membership, not a recursive subtree snapshot. A move changes
returned path/parentPath and metadata revisions, including descendants of a moved ancestor, while preserving
IDs. Effective revisions may incorporate ancestor namespace markers; contentRevision does not change merely
because an ancestor moved. Metadata revision preconditions compare the indexed view; unsynchronized external changes are not detected by that comparison.
Mutations separately enforce their admitted native conditions before applying finite effects. Object roots
capture the current ETag as native revision evidence and use operation-specific native conditions where
enforced. COS ignores PUT If-Match, so admitted replacement rechecks ETag under gateway coordination; this
does not fence arbitrary external writers. Stable IDs add no native conditional or version-pinning guarantee.
`contentRevision` is the saved, native-evidence-backed content generation and is null when none is recorded or
on folders; it is not synthesized from index timestamps. Content GET/HEAD and conditional replacements verify
current native evidence before using it. `size/mediaType/checksum` are null on folders. A verified checksum
carries algorithm and value; null means unknown. Application content replacement changes both revisions;
a proven rename preserves the saved content generation while changing metadata revision.
Optional lastChangedAt is the service time of the latest confirmed observed change; folders include descendant
changes. Optional changeToken is an opaque equality marker (a random change UUID), not a cursor, native validator
or sequence. Old rows without marker state omit both. Recursive markers advance on surviving old/new ancestors,
including root; effective descendant namespace movement is distinct from sibling content edits. Folder markers
covering hidden descendants are omitted. No-op scans and replay do not advance them. See section 10.

Copy creates an independent ordinary native file/tree at the destination. A provider-side copy or safe
filesystem reflink is allowed; an application-only reference or mutable hardlink is not a copy.

Metadata and lifecycle responses use `Cache-Control: no-store`. They do not use content ETags as metadata revision
validators. Mutations carry explicit metadata revisions as described below. When a trustworthy native
version/snapshot or coordinated writer model supports it, content GET/HEAD uses a strong HTTP ETag equal
to the quoted contentRevision. Otherwise omit the strong validator, disable validator-dependent guarantees,
and reject unsupported conditional writes; an index counter alone is not proof of unchanged native bytes.

## 3. Resource table

All suffixes are below `B`:

| Method | Resource | Behavior |
|---|---|---|
| GET | `/me` | Current principal, authorized path scope, indexed usage and reservations |
| GET | `/capabilities?path=/Photos` or `?id=1f12cfd6-b93b-41ad-ae71-52a4299c3421` | Effective limits/features at an authorized entry |
| GET | `?path=/Photos/a.jpg` or `?id=f7ca1104-12f0-4bbf-8c77-41d341f7d120` | Indexed metadata by either selector; no native lookup |
| GET | `/list?path=/Photos&limit=&cursor=` or `id=1f12cfd6-b93b-41ad-ae71-52a4299c3421` in place of path | Indexed immediate children with SQL keyset pagination |
| GET | `/search?q=&path=/Photos&kind=&mediaType=&limit=&cursor=` or `id=1f12cfd6-b93b-41ad-ae71-52a4299c3421` in place of path | Indexed search below selected folder; authorized-root default |
| GET, HEAD, PUT | `/content?path=/Photos/a.jpg` or `?id=f7ca1104-12f0-4bbf-8c77-41d341f7d120` | Download/headers/conditional replace; creation uses a destination |
| POST | (base) | Create folder using `{kind:"folder",destination:Destination,parents?}`; retained full-path shortcut |
| POST | `/move` | `{from:EntrySelector,to:Destination,ifRevision}`; retained string path shortcuts |
| POST | `/copy` | `{from:EntrySelector,to:Destination,ifSourceRevision}`; retained string path shortcuts |
| DELETE | `?path=/Photos/a.jpg&recursive=true` or `id=f7ca1104-12f0-4bbf-8c77-41d341f7d120` in place of path | Native deletion; explicit recursion for nonempty folders |
| GET | `/thumbnail?path=/Photos/a.jpg&width=&format=` or `id=f7ca1104-12f0-4bbf-8c77-41d341f7d120` in place of path | Authorized representation; current route remains 501 |
| POST, GET | `/uploads` | Create/list this principal's resumable sessions |
| GET | `/uploads/{uploadId}` | Compact state/progress and target path |
| PUT | `/uploads/{uploadId}/parts/{number}` | Embedded immutable numbered part |
| POST | `/uploads/{uploadId}/parts/{number}/authorize` | COS-signed exact part authorization |
| PUT | `/transfer/{encoded-object-key}?…` | Same-origin EO-to-COS byte forwarding; not handled by application |
| POST | `/uploads/{uploadId}/parts/{number}/confirm` | Server checks native part and accepts durable receipt |
| GET | `/uploads/{uploadId}/parts?limit=&cursor=` | Durable part receipts |
| POST | `/uploads/{uploadId}/commit` | Complete at the bound target path |
| DELETE | `/uploads/{uploadId}` | Abort temporary upload |
| GET | `/operations/{operationId}` | Durable workflow state/result |
| POST | `/operations/{operationId}/cancel` | Request stopping further safe steps |
| POST | `/change-snapshots` | Synchronously capture a bounded immutable indexed baseline and checkpoint |
| GET, DELETE | `/change-snapshots/{id}` | Page saved metadata or release the baseline |
| GET | `/changes?cursor=&limit=` | Pull fixed-filter authorized changes; returns items/nextCursor/hasMore |
| POST, GET | `/subscriptions` | Create/list this principal's durable SSE subscriptions |
| GET, DELETE | `/subscriptions/{id}` | Inspect/delete a current-policy subscription |
| POST | `/subscriptions/{id}/renew` | Renew an unexpired seven-day lease |
| GET | `/subscriptions/{id}/events` | Bounded SSE availability wakeup; not consumer acknowledgement |

GET /api/audit is a separate owner-cookie audit collection, outside B, documented in section 12.

All capabilities are service-level behavior. Clients follow the session's declared embedded or signed transport;
they do not select drivers or arbitrary upload origins. Storage connection administration remains outside this contract.

## 4. Lookup, folders, search, and namespace operations

`GET B?path=/` and `GET B?id=73ea6c34-2c46-4349-b3b8-d5fb7be0fb7c` return the same root metadata when authorized; scoped callers use
their authorized root selector. Metadata/list require one selector. Search/capabilities default to the authorized
root only when both selectors are absent. Listing/search use opaque keyset cursors and stable sorting: folders
first, NFC comparison-name bytes, then a deterministic indexed tie-breaker. Search is case-insensitive substring
matching on name and resolved visible path and includes descendants of the selected folder.
A cursor binds principal/scope, selector mode, selected folder, query and sort. An ID-selected folder follows
completed moves only while authorized. A path-selected cursor whose folder moved/recreated returns
`409 cursor_stale`; recheck current ancestry on every page. Ordinary concurrent writes do not globally
invalidate cursors. Interactive listing/search is not a snapshot: concurrent rename/move can cause omissions
or repeats; clients may deduplicate by ID and refresh. Incremental consumers use the immutable snapshot and
checkpoint in section 10. Native backup/import validation still requires a coordinated native observation.
Default page size is 200; maximum 1000. SQL applies owner, subtree, search filters and the keyset before
returning at most the requested limit plus one row. The API does not scan storage or load the whole indexed
subtree to paginate. Authorization filters apply before pagination and never leak totals.

Create a folder:

```http
POST B
Idempotency-Key: create-notes-001
Content-Type: application/json

{"kind":"folder","destination":{"path":"/Notes/2026"},"parents":true}
```

Creation requires `kind:"folder"` and `destination:Destination`; files use content PUT or uploads. The parent
form is `{"kind":"folder","destination":{"parent":{"id":"650ff3fa-efea-4dcb-83ec-733748289eb0"},"name":"2026"}}`; parent may use path.
The existing `{"kind":"folder","path":"/Notes/2026","parents":true}` form is deliberately retained as a
full-path shortcut, exclusive with `destination`. `parents` defaults false; true requires a full-path destination.
A parent selector must resolve to an existing folder. Missing parents return `409 parent_missing`; a parent file
returns `409 not_a_folder`. Existing folder returns `200` with unchanged ID/revision; new folder returns `201`
with a fresh ID; an existing file/equivalent sibling collision returns `409 name_conflict`.
`parents:true` creates native parents with fresh IDs and journals progress; multiple
backend mkdir calls are not universally atomic. Failure may leave created empty parents. Return structured
partial-effect details or an Operation when work cannot finish inline.

Rename/move:

```http
POST B/move
Content-Type: application/json

{"from":{"path":"/Photos/a.jpg"},"to":{"parent":{"id":"651304c3-0a93-4325-8c21-8cc79f479e43"},"name":"renamed.jpg"},"ifRevision":"nr_observed"}
```

`from` is an EntrySelector and `to` is a Destination; `from:{"id":"f7ca1104-12f0-4bbf-8c77-41d341f7d120"}` is equivalent after a
completed move. Retained string `from`/`to` values mean full paths and may independently replace their
object forms. Rename is a move within the same parent. `ifRevision` is required (`428` if absent).
Successful same-owner moves retain the selected ID and all descendant IDs. No target replacement or
implicit folder merging: a collision returns `409`. Same owner only; moving into oneself/descendants is
invalid. The actual native file/folder must move before success is reported. Prefer native rename; S3
folder moves require per-entry copy/verify/delete. Journal source versions, destination identities and
per-entry outcomes, coordinate conflicting mutations, then reconcile the index. There is no transaction
spanning Postgres and the backend, and no universal atomic whole-folder move.

Copies accept `{from:EntrySelector,to:Destination,ifSourceRevision}`, with the same retained string path
shortcuts. Source revision is required. Every copied file/folder receives a fresh ID, including descendants.
Copy is recursive by definition for folders and never merges/replaces an existing target. The service
records a finite source manifest and available native versions after verifying authorization. A consistent
manifest needs coordinated writers or backend snapshots; a folder revision alone is not a subtree snapshot.
Every successful destination is a normal readable file. Prefer provider-side copy/reflink, otherwise stream
bytes. Whole-tree copy can be partially visible and can fail partially; report progress and conflicts. Logical
quota charges the new sizes. No application-only content sharing replaces the destination files.

DELETE optionally accepts `If-Revision: nr_...` to guard the selected file or folder; recursive deletion additionally
serializes against subtree mutations. Without it deletion explicitly applies to the current accessible subtree.
No trash or restore API. Success requires actual native deletion plus index settlement; removing only index
rows is not deletion. Recursive deletion can partially succeed and must journal each captured entry; never
rescan and remove new external additions. Provider backup/version retention is separate and secure erasure is
not promised. Temporary transfer cleanup is background work, not deletion of authoritative user content.

Completed rename/move returns `200` file/folder metadata; copy returns `201`; deletion returns `204`. Slow
work returns `202 + Operation + Location`. The public action is the same even when backend work differs.
Cancellation/failure of multi-file work can leave native partial effects; the Operation records them.
A non-atomic move must never expose one ID at both source and independent destination. Reserve source IDs
under guards until coherent settlement. If terminal partial failure leaves both copies, source retains its ID
and destination gets a fresh ID; if only a proven moved destination survives, the original ID follows it.
Uncertain effects retain guards. See [identity settlement](../design/file-identity.md#native-effects-and-identity-settlement).

Mutations fence only the native paths they observe or mutate, so an open upload session elsewhere in the
drive never blocks an unrelated rename, copy or delete. A genuinely overlapping path returns
`409 operation_in_progress` with the blocking `operationId`; wait for that exact Operation, or abort the
identified open upload session when the conflict is a resumable upload of the same target.

## 5. Downloads and safe media

GET by ID or path returns `200` bytes, HEAD returns matching representation headers without a body. Root or
folder content returns `409 not_a_file`. Content responses include Content-Type, correct length,
`X-Content-Type-Options: nosniff`, suitable Content-Disposition and a strong ETag only when §2's native
version guarantees are proven. Default is attachment; `?disposition=inline`
is honored only for a safe media allowlist. HTML/SVG and other active content are not served as executable
same-origin documents; preview conversion/sandboxing must preserve this boundary.

Support single HTTP byte ranges (including open-ended and suffix ranges), `If-Match`, `If-None-Match`, and
`If-Range` with strong content ETags. Evaluate conditions before ranges. A satisfiable range returns `206`
and Content-Range; an unsatisfiable range returns `416` with `Content-Range: bytes */N`. Unsupported multi-range
requests are ignored and return a full `200`, as HTTP permits. HEAD ignores Range. `If-Range` mismatch returns
the full representation; a resuming client must detect that and restart. Never mix revisions.

Content is addressed by the native key the owner index recorded for the path. The endpoint observes the
representation with one native HEAD, evaluates conditions, and issues at most one ranged GET; a `304` or a
`HEAD` therefore costs one native request and opens no body. A read must not enumerate ancestor directories to
re-prove the path: when the index cannot name the file, native path resolution is the explicit fallback, and
index repair stays a reconcile concern rather than part of the request.

Select a proven native content version/snapshot for the response, or coordinate all application writes
against the read. External in-place changes cannot be frozen by an application lock or an open FD alone.
Do not claim strong ETags/pinning from size and mtime alone; use trustworthy native evidence or verification.
Only advertise the read guarantees the effective route can enforce. `Accept-Ranges: bytes` is sent only when the effective
route supports correct ranges. Backends without native ranges may use bounded managed cache/staging; otherwise
ignore Range, return `200`, and advertise range support false. A backend failure after response headers closes
the stream; do not append a JSON error to file bytes. Avoid compression that changes byte-range offsets.

Default client cache policy is `private, no-cache`; disable shared private caching unless current
authorization and a proven native content generation can be enforced on every cache hit and `304`. Native
paths can change externally, so an old index revision is not by itself proof of unchanged bytes. Cache keys
include owner/connection/content generation/representation. Never forward app credentials to storage.

Thumbnails use a bounded width/format allowlist from capabilities, contentRevision-based cache keys, and the
same authorization as the source. They may use native provider processing internally or application workers.
A pending conversion returns `202` with a same-origin operation Location; invalid types return `415` and
unsupported representation options return `422`. Raw file content GET never responds with a JSON job body.

## 6. Simple streaming writes

```http
PUT B/content?path=/Notes/a.md
Content-Type: text/markdown
Content-Length: 12

hello world
```

Full-path or parent-and-name PUT creates by default. `PUT B/content?parentId=650ff3fa-efea-4dcb-83ec-733748289eb0&name=a.md`
is equivalent to the path example; `parentPath=/Notes` can replace parentId. Parent-and-name is creation only,
exclusive with id/full path. Existing entry returns `409 name_conflict`; `If-None-Match:*` explicitly asserts
creation and returns `412` on conflict. Creation never accepts id. `PUT B/content?id=f7ca1104-12f0-4bbf-8c77-41d341f7d120` selects an existing
file and requires `If-Match:"cr_observed"` (`428` if absent); path replacement uses the same precondition.
Unknown/retired ID returns 404 and never creates. No wildcard or unconditional replacement is provided.

Conditions are enforced at actual native publication. Native conditional support or a declared coordinated-
exclusive writer model is required; reject unsupported conditions rather than emulate them with Stat followed
by unconditional write. Bind selected file ID, parent ID, native path and ancestor namespace state for both
selectors. A concurrent move or recreation of the target/ancestor returns `412 target_changed`; admitted
writes do not follow later moves. A new request after a completed move can use the ID at its current authorized
path. Successful replacement preserves file ID and changes contentRevision. Parents must already exist.

Content-Length is required (`411`); zero is valid. Unknown-length streams use an application-specific future
extension, not silent unlimited buffering. Above `maxSinglePutBytes` returns `413` and points to uploads.
The limit is deployment policy, not a hard-coded COS limit. Optional `Content-Digest: sha-256=:base64:` verifies
the request while streaming. MIME defaults to application/octet-stream; source modification time may be set
with `X-Source-Modified-At` (RFC 3339).

Return `201` file metadata for creation or `200` for replacement only after the normal native destination
contains complete bytes and the journal/index/result are settled. The content ETag is separate. Failed body
reception leaves the old file intact when the admitted staged-write strategy supplies that guarantee. Final
native publication can succeed before database settlement; recover it rather than claiming rollback.
A timeout after final publication starts is outcome_unknown until proven otherwise. Block conflicting
mutations while old requests can still apply; database leases alone cannot fence final-path writes.
Before issuing final-path I/O, store a durable recovery identity. An unresolved request returns a Problem with
same-origin operation Location/operationId (or uses its saved Idempotency-Key to recover that identity), not a
false rollback/success. Replays wait for/retrieve the recorded outcome rather than start another publication.

An optional Idempotency-Key plus Content-Digest makes an ambiguous small PUT retry safe without a preliminary
metadata request; absent a digest, use GET/HEAD to resolve uncertain success rather than blindly overwriting.
A retry body is still consumed/validated before claiming it matches a stored result. Large writes use sessions.

## 7. One resumable upload protocol

Clients use numbered service parts and the same create/status/receipt/commit lifecycle. Sessions explicitly
advertise transport `embedded` or `cos-signed`; they never let clients choose an arbitrary origin or provider.
Current geometry is 32 MiB parts, `ordering:any`, at most sixteen parallel parts per file and 10000 parts.
Session lists use opaque UUID keysets; receipt pages use acknowledgement keysets and a fixed `watermark`.
Commit returns a stable Operation until index/result settlement. Same-path sessions are rejected while an
older guard is active; the server does not cancel it on create. Admitted native COS/S3 uploads support up to 320000 MiB
subject to budgets, independently of the small content-PUT policy. Native multipart completion is implemented.
EdgeOne signed transport is COS-only and requires additional exact-endpoint signature/MD5/listing evidence.
Whole-file checksum input remains unsupported. Embedded parts optionally accept Content-Digest; signed parts
require per-part MD5 and a separately identified client SHA-256 assertion.
Create with an Idempotency-Key:

```http
POST B/uploads
Content-Type: application/json
Idempotency-Key: mobile-upload-001

{
  "destination":{"path":"/Videos/a.mp4"},
  "size":18234122311,
  "mediaType":"video/mp4",
  "condition":{"mode":"create"},
  "sourceModifiedAt":"2026-10-29T08:00:00Z"
}
```

Create mode (default) accepts `destination:Destination` and forbids `target`. For example,
`destination:{"parent":{"id":"14baf74e-feb7-47b4-acf1-c4d3146eea91"},"name":"a.mp4"}` creates under an existing folder.
Replace mode requires `target:EntrySelector`, `condition:{"mode":"replace","contentRevision":"cr_observed"}`,
and forbids destination. Both `target:{"id":"1384edac-0543-4c86-a185-5b9b3ae829b3"}` and `target:{"path":"/Videos/a.mp4"}` select an
existing file; an unknown ID returns 404. Replacement preserves ID; creation publishes a fresh ID.
Session IDs and any reserved future entry ID are separate; an open upload is not a live file.

The existing top-level `path` form is deliberately retained for current clients. With create/default condition
it is shorthand for `destination:{path}`; with replace condition it is shorthand for `target:{path}`. It is
exclusive with both destination and target; mixed forms return `400 invalid_selector`. This shortcut retains
the same condition, collision, authorization and binding rules and does not introduce an ID-shaped path.

Bind owner, selected file/parent ID, native path/name, ancestor namespace state, connection generation, size,
metadata and native condition. Recheck binding and current rights before final effects. A moved/recreated
parent, ancestor or target invalidates admission (`412 target_changed`) even for an ID-selected session; an
upload does not follow later moves. Internal publication generations/tombstones prevent absent/create/delete/
absent races and remain separate from public IDs. Creating a session never cancels an earlier session as a
server-side effect. Repeated commit returns the same file ID, result or Operation.

Example response (numbers are illustrative, not universal defaults):

```json
{
  "id":"u_...",
  "state":"open",
  "transport":"embedded",
  "size":18234122311,
  "partSize":33554432,
  "partCount":544,
  "maxParallelism":16,
  "ordering":"any",
  "nextPart":null,
  "receivedParts":0,
  "receivedBytes":0,
  "expiresAt":"2026-10-31T04:00:00Z",
  "partsUrl":"/api/files/uploads/u_.../parts",
  "commitUrl":"/api/files/uploads/u_.../commit"
}
```

The service selects a fixed partSize for the session. Part numbers start at 1; offset is `(number-1)*partSize`;
all but the final part have exactly partSize bytes. `partCount=ceil(size/partSize)`; a zero-byte session has
zero parts and can commit immediately. GET session returns compact authoritative counters, not an unbounded
part array. GET uploads lists accessible sessions with normal pagination for clients retaining session/source
state or performing authorized recovery. This API capability does not restore the browser upload queue after reload.

`ordering` is `any` or `ascending`. In ascending mode `maxParallelism=1`, `nextPart` identifies the next new
part; replay of earlier acknowledged parts remains allowed. An out-of-order new part returns
`409 part_out_of_order`, with nextPart. This is a service scheduling rule; clients implement one part protocol
and scheduling rule, not separate provider protocols. The service may use durable staging to support more
parallelism when its quotas and benchmarks justify the cost. `maxParallelism` is an upper bound, not a target.

The browser runs up to eight queue tasks sharing one global pool of sixteen byte requests; failed tasks may
retain open server sessions, so eight is not a hard cap on server-side sessions. Part requests have no extra
per-file cap, limited only by session.maxParallelism and ordering. A freed slot goes to the
earliest-started file's next part. Transient part failures have bounded backoff; a `429 rate_limited` answer means every
receive slot is busy and this part must simply wait, so it retries the same part without spending a failed attempt or
aborting the file. Before replaying an uncertain request, query the original
session and verify its durable part receipt against the retained File's part hash before skipping bytes or
replaying. The browser queue lives only in memory for the current Files page/document and survives folder
navigation. Refresh/close discards it; the next page starts empty, without restoring failed tasks, session waits
or File reselection. Same-page retry, idempotency, receipt verification and cancellation retain the exact
session/Operation identity. Register the browser-native beforeunload warning while queued, waiting, uploading
or finalizing tasks remain, and remove it when none remain. Warning display is subject to browser policy;
server cleanup does not rely on unload-time abort API calls.

The browser removes the current owner's old `eam.uploads.v1:<owner>` localStorage record without restoring or
executing it. Pending delete Operation identities may still be stored in owner-scoped localStorage independently
of upload queue state. Discarding browser tasks does not abort or fail durable server sessions or accepted
Operations: open sessions retain their normal expiry/cleanup, and accepted work continues under the existing
recovery contract. Clients retaining session identity and source bytes can use resumable API recovery across
reconnects/restarts; this is distinct from the browser's page-memory queue.

Browser tasks acquire ephemeral ownership for the owner and normalized target path before the first upload
allocation, using the [Web Locks lifecycle and scope](../design/storage-p6.md#resources-uploads-and-recovery).
A conflicting tab produces a retryable inline error, "This file is being uploaded in another tab. Try again after
it finishes.", without a popup or aborting the holder's session. Unavailable or failed ownership also fails inline
and retryably; the browser must not proceed unlocked or automatically abort a session without ownership.

An old open session can continue guarding its exact target after browser queue loss. After ownership is
acquired, a new selection whose creation returns `operation_in_progress` inspects only the identified blocking
session, validating the session/Operation identity and normalized target path. If it is `open`, the browser
automatically aborts that exact session and retries allocation without a restart confirmation. This flow never
automatically aborts `committing`, `outcome_unknown` or `committed` work. An authoritative `aborted`, `expired`
or `failed` session allows a new allocation only when its exact Operation is `cancelled` and the guard has
settled; a failed Operation or unknown cleanup outcome does not authorize retry. Unavailable session status
leaves the new task failed and retryable; definitive non-upload blockers keep their existing Operation waits.
This coordination applies only to cooperating same-origin tabs in one browser; it neither supplies server
authority nor proves inactivity of external API clients, other origins or devices. Server admission/abort
semantics and API fields are unchanged.

For admitted native COS/S3 multipart sessions, each acknowledged service part already resides in unpublished
provider multipart state. Streamed per-part reception verifies exact length and content under the slot lock and
records the resulting identity; no full-file local assembly or whole-file PUT follows. Commit performs native
completion and result/index settlement, with provider latency and unknown-outcome protection; it is not a
database-only operation.

### COS-signed transport through EdgeOne

When transport is `cos-signed`, use these control endpoints instead of sending bytes to the embedded part URL:

```http
POST B/uploads/u_.../parts/1/authorize
Content-Type: application/json

{"size":33554432,"sha256":"<64 lowercase hexadecimal characters>","md5":"<canonical base64 of 16 bytes>"}
```

Success returns `{method:"PUT",url,headers,expiresAt}`. The URL is origin-relative below
`/api/files/transfer/`, contains a five-minute exact COS part signature, and must not be changed by the client.
The only returned request headers are Content-MD5 and Content-Type=application/octet-stream. The signature also
binds Content-Length, final COS Host/path, PUT, uploadId and partNumber. No secret/STS key or Complete authority
is returned. The native parameters in this short-lived request are the explicit official-transport exception
to the earlier no-provider-parameters rule. Grant responses use no-store caching.

The application persists the immutable digest tuple before issuing authority. A different tuple for the same
part returns part_conflict; renewal cannot retarget bytes. After authorization, POST
`B/uploads/u_.../parts/1/confirm` (no browser ETag) first recovers a previously stored native part. On
`409 part_pending`, PUT the exact Blob to the signed URL, then confirm again, including after a lost PUT reply.
EO forwards bytes directly to COS using the deployment rules, with no edge function. App credentials/CSRF are
used only on control endpoints, never added to the transfer. Same-origin cookies must be stripped at EO.

Confirm lists recorded native COS parts and checks exact size plus ETag against the bound MD5 under admitted
ETag=MD5 semantics. It returns `{number,size,sha256,checksumSource:"client-sha256-cos-md5"}` only after durable
receipt acceptance; repeated confirmation reuses that receipt. The SHA-256 is a client assertion bound to the
grant, not a server-computed or COS-verified SHA-256. COS validates Content-MD5; this is not SHA-256 collision
resistance. Commit relies on these storage-confirmed receipts, not a browser success response. Missing/wrong
native parts get no receipt. All control actions recheck live authentication, creator/scope and session state.

Issued grants remain usable for their exact native part until expiry and accepted provider requests may outlive
expiry; logout/abort is not immediate signature revocation. Only the server can Complete. Cleanup retains
unresolved native liability and never uses grant expiry alone to release an uncertain final-path guard.
See [EO routing and admission](../design/native-upload-deployment.md) for header/path preservation and byte isolation.

### Embedded part acknowledgement and retries

```http
PUT B/uploads/u_.../parts/1
Content-Length: 33554432
Content-Digest: sha-256=:base64-value:

<bytes>
```

Content-Length must match the assigned part length. Content-Digest is optional; the service always computes
SHA-256 while receiving and returns `{number,size,sha256}` in a durable receipt (`201`, or `200` for identical
replay). Client hashing is per-part and optional, never a mandatory full-file pass before upload. Reject a
provided digest mismatch with `422 checksum_mismatch`; there is no receipt for a failed part.

A part slot is immutable once acknowledged. An identical replay returns its existing receipt; different bytes
return `409 part_conflict` and cannot replace it. Without a supplied digest, consume/hash the replay to compare;
never assume a duplicate number means identical content. Concurrent attempts for one slot are serialized by the
slot lock, and the receipt is written before that lock is released, so a confirmed slot is never written again
and a late attempt cannot change bytes an existing receipt already describes. An attempt that fails before its
receipt is confirmed leaves the slot reserved by number, size, and session but not bound by content; a later
replay may replace those bytes, and the receipt recorded afterwards always describes what the provider now
holds. Nothing unconfirmed is acknowledged or assembled. A part may therefore be streamed to the provider in
one pass; buffering or spooling is required only when mismatched bytes must be rejected before they reach the
provider. Acknowledgment requires durable provider/staging bytes plus a durable receipt, not bytes in memory or
an SDK progress callback.

Status counters count unique acknowledged parts only. GET parts returns receipts in acknowledgement order,
with a cursor binding session and a captured acknowledgement watermark. Later receipts do not shift those
pages. Re-list from the first page to see new receipts; pagination returns the watermark explicitly. Complete
coverage checking happens server-side and does not rely on the client's receipt list. A full-file checksum may
be supplied at creation, but must be verified if accepted; part hashes alone do not equal a whole-file hash.
Capabilities disclose supported checksum algorithms and any additional verification work.

### Commit, abort, and expiry

```text
open ──commit──> committing ──confirmed native effect + index settlement──> committed
  │                 ├──unresolved native effect──> outcome_unknown ──reconcile──> committed/failed/aborted
  │                 ├──definite failure──> failed
  │                 └──cancel before final effect──> aborted
  ├──abort──> aborted
  └──timeout──> expired
```

Commit atomically verifies exact part coverage, freezes the manifest, and transitions to committing; incomplete
coverage returns `409 upload_incomplete` and stays open. From then on reject new part writes. It returns
file metadata with `200` if settled, or `202 + Operation + Location + Retry-After` otherwise. Repeated commit
returns the same settled result or operation identity. Retry only safe native steps; ambiguous final effects
enter outcome_unknown and block
conflicting mutations until resolved. Neither an application revision nor a final-path HEAD alone proves
that older remote requests cannot still arrive.

DELETE upload aborts open sessions atomically (`204`, repeat `204`). It returns `409 commit_in_progress` for
committing sessions and `409 already_committed` after publication; use operation cancellation for a committing
upload. Before final native publication starts, cancellation can fence temporary writes. Once a final-path
effect is in flight, cancellation is best effort: the result may be committed or outcome_unknown. Never
report a definite abort while the native write could still succeed. Temporary bytes finishing after an open
abort acquire no receipt and cannot begin publication; clean them by their recorded temporary identity. Failed/aborted/expired session IDs remain tombstones
for a retention window and are never reused. Their content writes/commit return `410`.

Default idle expiry is 24 hours, extended only by accepted durable progress, with a hard seven-day maximum;
capabilities may advertise different policy. Native upload-session retention additionally caps the advertised
expiry/extensions unless durable staging can recreate the acknowledged bytes. Credential renewal does not
extend a provider session's lifetime. Expiry applies only to open sessions. Committing work has worker
leases/recovery rather than expiring because a request/token timed out. An expired provider credential is
renewed internally; an unrecoverably lost native session fails explicitly rather than pretending bytes survived.
Keep terminal results/idempotency records at least 24 hours after termination, and advertise exact retention.
Revocation prevents new client actions; queued work rechecks authorization before publishing. First-party owner
management can recover sessions, but another app token does not gain access merely by sharing the owner.

## 8. Operations, concurrency, and quota

Operation JSON has `{id,kind,state,progress,result,error,cancelRequested,createdAt,finishedAt}`. States are
`queued/running/outcome_unknown/succeeded/failed/cancelled`; progress is `{completed,total,unit}` with nullable
total. Also record per-entry effects, conflicts and remaining work for partial tree operations.
Kinds are bounded file actions (`mkdir/move/copy/delete/upload_commit/thumbnail`), not user-submitted arbitrary tasks.
Result is the file/folder metadata for mkdir/move/copy/upload_commit, `{deleted:true}` for delete, or
`{contentUrl,mediaType,size,contentRevision}` for thumbnail; contentUrl is a same-origin representation endpoint.
The committed upload also stores its file metadata result in session status. Errors use the same Problem structure.

Operation/session visibility uses owner, creator principal and captured authorization anchors. Before creation,
the anchor is the destination parent, not the nonexistent output file or folder. While source/destination files or folders still exist,
recheck current ancestry. After successful delete, retained tombstone provenance anchors the terminal operation
result, so authorized polling/idempotency replay can still confirm deletion. It grants no file-content access.
Token validity and action/scope rights still apply; another app cannot read results solely because it shares
the owner. Never require a deleted source to have live ancestry to retrieve its own deletion result.
Polling returns `200` with the state. Synchronously completed actions return their ordinary resource/status;
accepted unfinished actions return `202 + Operation + Location` with Retry-After. `Prefer: respond-async` requests immediate
acceptance; no header promises completion before timeout. Streaming PUT only returns after commit and does
not detach the caller's unfinished request body into a background operation.

Cancel stops further safe steps, not already completed native effects. Before final publication starts,
a winning upload cancellation sets session aborted and operation cancelled. After the native effect starts,
resolve its real outcome; committed returns `409 already_committed`, unresolved remains outcome_unknown.
Repeat commit on a definitely aborted session returns `410 upload_aborted`.
A running commit must seal/migrate its native data before its provider retention deadline, retain a replayable
spool, or fail explicitly if the provider expires it; removing the public idle timer does not extend native
retention. Advertised resume durability covers the admitted open-session interval, not unlimited native life.
Failed/cancelled tree operations may have partial native results; report captured per-entry progress.
Recovery follows the saved manifest and native validators, never blindly rescan-and-delete a changed tree.
A replacement worker cannot issue conflicting final effects merely because its predecessor lease expired.
An unresolved operation retains its path/subtree guard until native quiescence/fencing is established.

Reserve quota transactionally before accepting bytes, consistently for every client and authentication method. Charge
logical file sizes (including copies), track reservations independently from staging/physical usage, recheck
replacement conditions at commit. On failure/abort/expiry, release only unused reservations after indexing
and charging all proven native partial results; retain liability for unresolved effects. Concurrent sessions
must not overbook the account. Replacements reserve worst-case additional liability without allowing multiple
writers to spend the same old file's size credit. Temporary upload data and retained provider versions use
a separate physical budget. Missing index entries alone never make ordinary native files garbage.

Idempotency keys are scoped by owner, principal, route/action and canonical request fingerprint. Same key with
different intent returns `409 idempotency_conflict`. Store the durable operation/session/result with the key;
retry after a lost response returns that identity. A replay never bypasses current authorization. Keys cannot
provide exactly-once effects at arbitrary third parties; journals, native conditions and explicit uncertain
outcome handling define the actual guarantee. Retention is explicit; retrying a key after expiry is a new request.

## 9. Capabilities and failures

Capabilities is authenticated and accepts either selector (default visible root only when both are absent),
returning effective `read/write`,
`range`, `thumbnails`, supported checksums, maximum file/single-PUT/part sizes, pagination limits, session
expiry/retention policy, `sourceModifiedAt` support, and quota/reservation information appropriate to the caller. Numbers are authoritative
for admission but may change before a later request; new sessions bind their accepted limits. Upload responses
provide their exact chunk layout. Never expose provider names or assume every backend has the same limits.

Capabilities also always include identity: {id:true,path:true,uniquePaths:true} and changeTracking with
available, read, subscribe, delivery, retentionSeconds, subscriptionLifetimeSeconds, snapshotLifetimeSeconds,
snapshotMaxEntries, snapshotMaxBytes, observation and externalChanges. The current values are:

```json
{
  "identity":{"id":true,"path":true,"uniquePaths":true},
  "changeTracking":{
    "available":true,
    "read":true,
    "subscribe":true,
    "delivery":["sse"],
    "retentionSeconds":7776000,
    "subscriptionLifetimeSeconds":604800,
    "snapshotLifetimeSeconds":86400,
    "snapshotMaxEntries":100000,
    "snapshotMaxBytes":67108864,
    "observation":"indexed",
    "externalChanges":"explicit-reconcile"
  }
}
```

Read/subscribe are effective grants for this principal; an old read-capable token sees both false. Availability
and advertised transport describe implemented service support, not automatic native watching or retention
maintenance. GET B/me also returns changes and subscribe booleans alongside existing file read/write/scope,
usage and cookie csrfToken fields. Owner sessions have both new grants; token grants remain explicit.

Object roots use current ETags as native revisions: `pinnedRead`, `strongContentValidators` and
`sourceModifiedAt` are false, while upload, replacement, folder creation, move, copy and deletion are
available whenever `write` is true and the requested action is admitted. Native condition enforcement is
operation-specific; COS PUT ignores If-Match, so replacement uses gateway coordination and an immediate
ETag recheck. Outside writers are not globally fenced; object reads are validated but not version-pinned.
Scans synchronize observed index metadata; budget counters retain the initial compared baseline plus
application journal changes/reservations, not exact quota state across unsynchronized outside writers.

| Status | Representative code |
|---|---|
| 400 | invalid_request, invalid_path, invalid_selector, invalid_cursor |
| 401 | unauthenticated |
| 403 | forbidden (known visible resource, insufficient action permission) |
| 404 | not_found (also outside visible scope) |
| 409 | name_conflict, cursor_stale, upload_incomplete, part_conflict, part_out_of_order, baseline_required, watch_deleted, watch_access_lost, snapshot_limit, subscription_limit |
| 410 | upload_expired, upload_aborted, result_expired, cursor_expired, snapshot_expired, reset_required |
| 411 | length_required |
| 412 | precondition_failed, target_changed |
| 413 | request_too_large, file_too_large |
| 415 / 422 | unsupported_media_type / checksum_mismatch, unsupported_option |
| 416 | range_not_satisfiable |
| 428 | precondition_required |
| 429 | rate_limited (Retry-After) |
| 502 / 503 / 504 | storage_failure / temporarily_unavailable, snapshot_too_large, snapshot_incomplete, audit_unavailable, change_root_reset_required / storage_timeout |
| 507 | quota_exceeded, staging_full |

Retry transient requests with backoff/jitter and Retry-After. Resolve ambiguous mutations through their
session/operation/idempotency identity rather than assuming a timeout means nothing happened.

## 10. Indexed change snapshots and incremental pull

These resources require current file read plus explicit changes permission for Bearer tokens. Owner sessions
have the capability by default. They inherit the same cookie Origin/CSRF mutation policy, plain resources and
Problem Details. They do not return historical file bodies. Ordinary list/search is not a consistent baseline.

```http
POST B/change-snapshots
Content-Type: application/json

{"target":{"path":"/Notes"},"kind":"folder","recursive":true}
```

Target is an EntrySelector. File and folder targets must exist and match the declared kind; a file cannot be
recursive. An ID target follows identity while authorized; a path target observes its fixed location. Folder
snapshots include the selected folder plus direct children or all descendants. A file snapshot includes itself.
The index, effective markers and committed feed head are captured in one repeatable-read view, and saved
metadata is fully materialized before 201. Location identifies the snapshot entries resource.

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

GET entriesUrl accepts only cursor/limit, default 200 and maximum 1000, and returns the snapshot resource
fields plus items (complete file metadata) and nextCursor (null when complete). Metadata pages remain immutable
while current authority, target access, root epoch and retained head are rechecked. Stage every page, then
atomically install the view and changesCursor. DELETE the snapshot returns 204 and releases retained metadata,
including after expiry when the same policy still owns it.

Current creation is synchronous and bounded to 100,000 entries including the target, 64 MiB serialized metadata,
25 seconds per request with a 20-second SQL statement timeout, ten active snapshots per principal and a 24-hour
lifetime. There is no asynchronous snapshot Operation. Oversize returns 503 snapshot_too_large; timeout,
serialization and lock failures return 503 temporarily_unavailable. Admission overflow is 409 snapshot_limit.
Started/unknown overlapping native work returns 409 operation_in_progress; an incomplete saved baseline returns
503 snapshot_incomplete. Expiry returns 410 snapshot_expired. Native bytes are not frozen by this index snapshot.

Continue with GET B/changes?cursor=<changesCursor>&limit=200. Only one cursor and limit are accepted;
missing/malformed/mismatched cursor is 400 invalid_cursor. Filter, selector mode and stream boundary are sealed
in the cursor. A response is {items,nextCursor,hasMore}; nextCursor is always present, even for an empty page.
HasMore means more authorized matching facts within the cycle's captured committed head, not future activity.
Drain until false and persist each page's effects plus its nextCursor atomically. Pull does not extend expiry.

Change items have id (change UUID), type, coverage (entry/subtree), origin, observedAt and optional entryId,
before/after (complete retained file metadata), path, impactPaths, affectedFolders or terminal reason. Public
types are upsert, remove, moved, invalidated and watch_ended. Creation/update produces upsert with after;
delete produces remove with authorized retained before/location. A fully visible move carries before and after.
Apply authorized affectedFolders patches with the entry effect so direct revisions and recursive markers stay
current without extra logical changes per ancestor. Nonrecursive filters do not receive filtered-out deep edits.

A complete visible ID/parent tree can apply an atomic subtree moved item as one parent/name update and lazily
invalidate descendant paths. A narrower view or client lacking membership/endpoints takes a new snapshot for
its entire watched view. Invalidated is an explicit refresh instruction, not a proven content edit. An ID
continuation requiring refresh returns 409 baseline_required. Targeted snapshot/cursor splicing is deferred.

Projection authorizes old/new endpoints independently and matches scope and filter on the same endpoint.
A departure becomes remove without its concealed destination; an arrival becomes upsert or bounded subtree
invalidation. Hidden ancestor effects are reduced to the watched boundary. No raw sequence/count, actor,
concealed endpoint/parent ID, credential or native locator appears. Authorized SQL filtering precedes page
limits; hidden-only changes do not create sparse progress pages. Change cursors are authenticated, encrypted,
padded and randomized, so different strings do not prove matching activity.

ID deletion/access loss yields authorized cleanup remove and watch_ended with reason deleted/access_lost.
Continuing the terminal checkpoint returns 409 watch_deleted/watch_access_lost; returning later does not revive
that watch. Path watches stay at their location and can observe replacement, although first-phase baseline
creation requires a currently existing target. Snapshots grant no access to a target now outside current scope.

The policy binding includes owner/principal, root/configuration, scope, hidden roots, comparison rules, grants
and monotonic policyVersion. Changed rights/scope invalidate old continuations even after later restoration;
name-only edits preserve generation. Expired cursors, retained-floor loss and epoch mismatch return
410 cursor_expired and require a new baseline. Snapshot checkpoints expire after 24 hours; subscription
checkpoints expire with their lease. Change rows default to 90-day expiry. The administrative
`eam file-changes-prune --owner-id UUID [--dry-run]` command removes only an expired contiguous prefix and
retains effect/deduplication receipts. There is no automatic scheduler or HTTP prune endpoint.

Migration 032 durably binds the owner stream to its current native root/configuration on first use. Later
mismatches are rejected, not silently rebound: root admission/pull can report 410 reset_required and snapshot
creation 503 change_root_reset_required. An old differently bound cursor can fail invalid_cursor first. After
DB restore or every root rebind, including A → B → A, the operator must stop application/external writers,
settle accepted Operations and run `eam file-changes-reset --owner-id UUID --writers-quiesced [--dry-run]`
against the intended current configuration. It refuses remaining guards/nonterminal Operations. Reset rotates
epoch, sets retained floor=head, expires subscriptions/snapshots and clears markers while retaining monotonic
positions, file IDs, effect receipts, audit and native bytes. Even same-namespace restores need a reset;
restored secrets/epochs must not revive old cursors. Clients then bootstrap new snapshots. Dry-run rolls back.
See the [root lifecycle procedure](../design/file-changes.md#durable-root-binding-and-restorerebind-procedure).

## 11. Durable file subscriptions and SSE

Subscription routes require file read, changes and subscribe. Create with:

```json
{"target":{"id":"1411944e-bcbe-49a2-aa30-7196f3dc09b4"},"kind":"folder","recursive":true,"delivery":{"type":"sse"}}
```

POST B/subscriptions returns 201 + Location and {id,filter,state,expiresAt,changesCursor,eventsUrl}. State is
active or ended. The filter retains the selected ID/path mode. An ID target must exist and be visible at
creation. A missing non-root path can be watched when its immediate authorized parent exists and is a folder;
this supports future arrival without inventing an empty snapshot baseline. A file matches itself, a nonrecursive
folder itself/direct children, and a recursive folder all descendants. ID mode follows identity only while
authorized; path mode remains at its location.

The returned changesCursor starts at the committed head: it enables future-only watching, not a current-state
baseline. A complete mirror uses its snapshot checkpoint instead. GET B/subscriptions returns {items} for this
principal/root/current policy, with no query filters or pagination. At most 100 active resources per principal
are admitted (409 subscription_limit). GET/DELETE B/subscriptions/{id} inspects/deletes; DELETE returns 204.
POST B/subscriptions/{id}/renew extends a valid unexpired lease by seven days after epoch/retention checks.
Expired leases/checkpoints return 410 cursor_expired. Policy-bound resources cannot be accessed by another token.

GET eventsUrl serves text/event-stream with event name changes and data:

```json
{"subscriptionId":"<subscription UUID>","type":"changes_available","notificationId":"<notification UUID>"}
```

Streams last at most 25 seconds, poll approximately every second and close after one coalesced notification.
They disable proxy buffering and recheck token expiry/revoke/policy on each poll. SSE id/Last-Event-ID identify
a durable notification, never a change cursor or consumer acknowledgement. Matcher checkpoints/notification
IDs survive restart and are independent of pull progress. Always pull on reconnect/app resume and periodically
even without a notification. Use streaming HTTP for Bearer headers and same-origin cookie connections for
browser sessions; never put tokens in event URLs.

Only delivery.type=sse is supported. External webhooks, receiver-control challenges, delivery signing/rotation,
outbound callbacks and webhook retries are deferred. No notification grants content access. See the
[change design](../design/file-changes.md) for matching, projection and observation limits.

## 12. Owner audit and Connect permission management

GET /api/audit?limit=50&cursor=... is outside B. It returns the plain collection {items,nextCursor} and Problem
Details, requires an owner cookie session, and rejects explicit Authorization instead of falling back to a
cookie. File/app permissions do not imply audit access. Only cursor and limit (1–100, default 50) are accepted;
app/path/action/time filters and export are not implemented. The reverse observed-time/UUID keyset is a recent
management view, not the change feed's commit-safe replay cursor.

Audit rows contain id, action, outcome, nullable allowed, status, origin, observedAt and actor; optional fields
are bytesWritten, reasonCode, entryId, path, requestId and operationId. Actor snapshots contain kind and optional
principalId/appId/grantId/label/scope/canRead/canWrite/canChanges/canSubscribe/canEvents/policyGeneration. False
actor bits may be omitted. Labels are self-reported connection names, not verified publisher identities.
Captured attribution survives credential rename/revocation. There is no separate verified app registry yet.

Authenticated third-party file GET/HEAD admission is durable before releasing metadata/bytes; persistence
failure returns 503 audit_unavailable. Owner reads and completion telemetry may have explicitly incomplete audit
when persistence fails. Confirmed mutation effects record audit transactionally with settlement; recover native
success when database settlement fails. Completion bytesWritten counts bytes accepted by the server writer,
not proof of delivery. A crash can leave admission without completion. Known-owner denials are recorded;
unknown-owner authentication failures go to platform logs. Never store secrets, bodies, raw search text, signed
URLs or raw provider errors. Audit history currently remains indefinitely; no 365-day deletion scheduler, audit-retention deletion SLA
or tamper-proof compliance claim is provided.

Connect/account/event APIs retain their existing envelopes and session-only management boundary. POST
/api/connect/tokens and PATCH /api/connect/tokens/{id} accept explicit changes/subscribe booleans alongside
prefix/read/write/events/expiresIn. PATCH is a complete policy replacement, so omitted new grants are false.
Changes requires read; subscribe requires changes and read, with English 400 validation errors. Old tokens,
save links and newly minted apps receive neither implicitly; owner sessions have both capabilities.

Token resources and GET /me token snapshots include changes, subscribe and policyVersion. Actual changes to
prefix/permissions/expiry advance the generation; revoke advances it too. Renaming and identical saves do not.
PATCH /me remains name-only and returns the current policy. QR JSON remains credential-only; live /me and /s/
instructions show current grants rather than freezing mutable rights into a QR. CLI token create accepts
--changes and --subscribe, default false. These rights never expand native path scope. The older audit_log
account activity path remains separate from the new file_audit owner collection.

## 13. Design choices and acceptance

- One API accepts stable service IDs or direct paths for every client. Native/provider IDs and unrelated
  database keys stay private. Same-owner moves and content replacement retain IDs; copies/recreation allocate
  fresh IDs. Native paths remain unique and readable; S3 moves may copy bytes. IDs never bypass authorization.
- One service part protocol supports both independent and ordered storage; native protocols stay internal.
  Plain streaming PUT avoids multipart overhead for small files. tus is not the public protocol: its core
  offset stream does not by itself provide the independent part slots/replay receipts chosen here.
- Native readable folders/names/raw bytes are mandatory. Leaving Everything requires ordinary native tools,
  not its database or export. Temporary staging is allowed; permanent anonymous content is not.
- Native mutations plus a durable journal/index repair replace the old database-only publication model.
  There is no universal atomic folder operation or transaction spanning provider and Postgres.
- Plain JSON/Problem responses and one operation resource remove legacy envelope and per-action polling
  exceptions. Paths have no version segment.
- Retained current path forms are explicit shortcuts listed above; no retired file routes or envelopes return.
  Existing data needs deliberate backup/import and stable-ID mapping preservation; redesign does not authorize
  deleting content or reusing a retired ID. Sections 10–12 describe first-phase changes/audit;
  webhooks, asynchronous snapshots and automatic retention scheduling remain deferred.

Before shipping: exercise the same API conformance suite against local disk and COS/S3, then plain WebDAV
and a sequential-upload drive; cover create/replace races, parent moves, immutable part replay, lost responses,
commit/abort/restart/revocation, quota reservations, path-scope authorization after moves, range/HEAD correctness,
media safety, stale native effect blocking, partial tree recovery, readable no-database exit and scan/index repair. Measure tiny/large/mixed transfers,
LAN/high RTT/throttling/interruptions, memory/staging, request overhead, and egress cost. This document specifies
behavior; it does not claim the existing implementation or tests already meet it.
