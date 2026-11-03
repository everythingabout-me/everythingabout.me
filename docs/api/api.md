# Unified file API

> Clean-slate specification, 2026-10-30. P2 read resources and same-origin GET/HEAD content are implemented.
> P4 implements PUT, native namespace actions and Operations for explicitly admitted coordinated Local roots.
> Writes default off and require a verified root baseline. [COS/S3 mutation adapters](../design/storage-p4-objects.md)
> additionally require exact-connection native probe evidence and durable single-gateway spool; live provider
> probes were not configured during implementation. P5 implements durable numbered-part uploads on admitted
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
browser route and no file-ID resource. All returned transfer/operation locations are origin-relative below
this same base. In examples, `B` means `/api/files`.

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

### Identity and paths

**The path identifies a file or folder.** `/Photos/2026/trip.jpg` is the same relative tree stored below
the owner's native content root. Public metadata, targets, folder listing, search, rename, move, copy, delete,
content and thumbnails all use paths. There is no fileId, parentId, sourceId or /{fileId} route. Database keys
and provider IDs stay internal and confer no public identity.

Rename/move changes the public path. An old path may stop existing or later refer to another file; applications
must update their references. ETags/revisions are change preconditions, not alternative identity handles.
Upload and operation IDs still identify temporary workflows; they never become file identities.

Paths are absolute within the account file root: `/` is the root, `/Notes/a.md` is a descendant.
A scoped credential is authorized for a declared path subtree; its paths are not silently rebased.
Preserve accepted name spelling. Native case sensitivity, normalization and filename limits are declared by
the connection; reject ambiguous case/Unicode collisions and unsupported names. NFC may be used for lookup
comparison, but must not silently rewrite existing native names. Preserve spaces. Reject empty segments,
`.`, `..`, backslashes, control characters, trailing slashes except `/`, and invalid UTF-8. Never trim or silently rename.
Paths are passed as query values or JSON strings, not mixed with ID route segments. URL-encode query values
with a standard URL builder, including `+` as `%2B`, `#` as `%23`, and `?` as `%3F` when part of a filename.
Decode query encoding exactly once, then validate the resulting logical path; JSON strings are already literal
paths. A literal percent-encoded-looking filename is not decoded a second time. Backend name, segment,
depth and path limits constrain admission; unsupported original names cannot secretly become encoded or
anonymous native files. Existing ambiguous native entries are reported as conflicts, not silently hidden.

Authorize normalized native paths by segment boundaries and backend case/normalization semantics, never
raw string prefix (`/Photos` must not authorize `/Photos-old`). A scope follows its configured path, not an
invisible file ID: moving content outside scope removes access; new content at the authorized path falls
under that scope. Moving/renaming a scope root does not automatically retarget that token. Check both source
and destination for move/copy and recheck session/operation rights before effects. Do not follow symlinks
outside the bound root. Missing and concealed out-of-scope resources return 404. Root cannot be moved/deleted.

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
  "checksum": null
}
```

Metadata, folder lists, search and indexed usage are database views. They issue no native Stat/List/Open
requests and retain the indexed path spelling, attributes and timestamps. Native files without index rows
appear after an explicit scan/import or successful application mutation. Missing indexed paths return 404;
an unavailable index returns 503 without a storage fallback. Root metadata is synthetic. Unknown root times
use `0001-01-01T00:00:00Z`; `sourceModifiedAt` and checksum are null when unavailable.

External edits may leave metadata stale until index synchronization. Existing reconcile/import adds or updates
observed entries; it does not prune externally deleted rows. Content requests read actual native bytes and can
return 404 even while an old index entry is visible. Application mutations settle their verified native effects
into the index before reporting success. These indexed-read semantics supersede P2's live native observations.

`revision` describes indexed file/folder metadata; it changes when that indexed representation changes.
Folder revisions also include indexed direct-child membership, not a recursive subtree snapshot. A move changes
returned path/parentPath and metadata revisions, including descendants of a moved ancestor. Metadata revision
preconditions compare the indexed view; unsynchronized external changes are not detected by that comparison.
Mutations separately enforce their admitted native conditions before applying finite effects. Full mutations
capture native versions; restricted `createOnly` writes enforce native absence without a fabricated version.
`contentRevision` is the saved, native-evidence-backed content generation and is null when none is recorded or
on folders; it is not synthesized from index timestamps. Content GET/HEAD and conditional replacements verify
current native evidence before using it. `size/mediaType/checksum` are null on folders. A verified checksum
carries algorithm and value; null means unknown. Application content replacement changes both revisions;
a proven rename preserves the saved content generation while changing metadata revision.
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
| GET | `/capabilities?path=/Photos` | Effective limits/features at an authorized path |
| GET | `?path=/Photos/a.jpg` | Indexed file/folder metadata by path; no native lookup |
| GET | `/list?path=/Photos&limit=&cursor=` | Indexed immediate children with SQL keyset pagination |
| GET | `/search?q=&path=/Photos&kind=&mediaType=&limit=&cursor=` | Indexed search below a path |
| GET, HEAD, PUT | `/content?path=/Photos/a.jpg` | File download/headers/create/conditional replace |
| POST | (base) | Create a folder using `{kind:"folder",path,parents?}` |
| POST | `/move` | Rename/move using `{from,to,ifRevision}` |
| POST | `/copy` | Copy using `{from,to,ifSourceRevision}` |
| DELETE | `?path=/Photos/a.jpg&recursive=true` | Native deletion; explicit recursion for nonempty folders |
| GET | `/thumbnail?path=/Photos/a.jpg&width=&format=` | Authorized image representation |
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

All capabilities are service-level behavior. Clients follow the session's declared embedded or signed transport;
they do not select drivers or arbitrary upload origins. Storage connection administration remains outside this contract.

## 4. Lookup, folders, search, and namespace operations

`GET B?path=/` returns root metadata when authorized; scoped callers use their authorized root path. List and search use opaque keyset cursors, stable sort by
kind (folders first), NFC name bytes, and normalized full-path tie-breaker. Search is case-insensitive substring
matching on name and resolved visible path; `path` defaults to the authorized root and includes descendants.
A cursor binds principal/scope, query and sort. Ordinary concurrent writes do not globally invalidate it.
Interactive keyset listing/search is not a snapshot: concurrent rename/move can cause omissions or repeated
paths; clients may deduplicate/refresh. Backup/import validation uses a coordinated scan, not this pagination.
Default page size is 200; maximum 1000. SQL applies owner, subtree, search filters and the keyset before
returning at most the requested limit plus one row. The API does not scan storage or load the whole indexed
subtree to paginate. Authorization filters apply before pagination and never leak totals.

Create a folder:

```http
POST B
Idempotency-Key: create-notes-001
Content-Type: application/json

{"kind":"folder","path":"/Notes/2026","parents":true}
```

Creation requires `kind: "folder"` and `path`; files are created by content PUT or an upload session.
`parents` defaults false. Missing parents return `409 parent_missing`; a parent file returns `409 not_a_folder`.
Existing folder returns `200` without changing its revision; new folder returns `201`; existing file
returns `409 name_conflict`. `parents:true` creates native parents explicitly and journals progress; multiple
backend mkdir calls are not universally atomic. Failure may leave created empty parents. Return structured
partial-effect details or an Operation when work cannot finish inline.

Rename/move:

```http
POST B/move
Content-Type: application/json

{"from":"/Photos/a.jpg","to":"/Archive/renamed.jpg","ifRevision":"nr_observed"}
```

Both `from` and `to` are full paths; rename is a move within the same parent. `ifRevision` is required
(`428` if absent). No target replacement or
implicit folder merging: a collision returns `409`. Same owner only; moving into oneself/descendants is
invalid. The actual native file/folder must move before success is reported. Prefer native rename; S3
folder moves require per-entry copy/verify/delete. Journal source versions, destination identities and
per-entry outcomes, coordinate conflicting mutations, then reconcile the index. There is no transaction
spanning Postgres and the backend, and no universal atomic whole-folder move.

Copies accept `{from,to,ifSourceRevision}` with full paths. Source revision is required.
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

## 5. Downloads and safe media

GET by path returns `200` bytes, HEAD returns matching representation headers without a body. Root or
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

The path shortcut creates by default. Existing file returns `409 name_conflict`. `If-None-Match: *` explicitly
asserts creation and returns `412` on conflict. Replacement requires `If-Match: "cr_observed"`; no wildcard or
unconditional replacement is provided.
Conditions are checked at the actual native publication effect, not just before receiving the body.
Native conditional support or a declared coordinated-exclusive writer model is required; unsupported
strong conditions are rejected rather than emulated by Stat then an unconditional write. Path writes bind
the observed parent and target file identity; a concurrent replacement file or moved ancestor cannot silently
receive the upload. Changed bindings return `412 target_changed`. Parent folders must already exist.

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
Current geometry is 8 MiB parts, `ordering:any`, at most four parallel parts per file and 10000 parts.
Session lists use opaque UUID keysets; receipt pages use acknowledgement keysets and a fixed `watermark`.
Commit returns a stable Operation until index/result settlement. Same-path sessions are rejected while an
older guard is active; no session is silently cancelled. Admitted native COS/S3 uploads support up to 80000 MiB
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
  "path":"/Videos/a.mp4",
  "size":18234122311,
  "mediaType":"video/mp4",
  "condition":{"mode":"create"},
  "sourceModifiedAt":"2026-10-29T08:00:00Z"
}
```

Creation accepts `path`, the complete target filename, and condition mode create (default) or replace
with contentRevision. Bind owner, normalized native path, internal target/parent evidence, connection generation,
size, metadata and native condition. Recheck the path and parent before the final native effect; moving or
recreating a parent/target invalidates the admission (`412 target_changed`). A session does not follow a
renamed file. Internal target generations/tombstones prevent absent/create/delete/absent races; those values
are not public file IDs. New sessions never silently cancel earlier sessions.

Example response (numbers are illustrative, not universal defaults):

```json
{
  "id":"u_...",
  "state":"open",
  "transport":"embedded",
  "size":18234122311,
  "partSize":33554432,
  "partCount":544,
  "maxParallelism":4,
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

The browser advances at most four files concurrently, with up to four part requests per file (sixteen byte
requests overall), capped by session.maxParallelism and ordering. It yields between bounded batches for
fairness. Transient part failures have bounded backoff; before replaying an uncertain request, query the original
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

An old open session can continue guarding its exact target after browser queue loss. When the user explicitly
selects a new upload and creation returns `operation_in_progress`, the browser inspects only the identified
blocking session, validating the session/Operation identity and target path. If it is `open`, browser confirmation
offers to discard the unfinished upload and start again. Acceptance aborts that exact session before retrying
allocation; declining leaves the new task failed and retryable. This flow never silently aborts another tab's
session and never aborts `committing` or `outcome_unknown` work. For this blocked-create recovery, an
authoritative `aborted`, `expired` or `failed` session allows a new allocation only when its exact Operation is
`cancelled` and the guard has settled; a failed Operation or unknown cleanup outcome does not authorize retry.

For admitted native COS/S3 multipart sessions, each acknowledged service part already resides in unpublished
provider multipart state. Bounded per-part buffering verifies exact length/hash and binds immutable identity;
no full-file local assembly or whole-file PUT follows. Commit performs native completion and result/index
settlement, with provider latency and unknown-outcome protection; it is not a database-only operation.

### COS-signed transport through EdgeOne

When transport is `cos-signed`, use these control endpoints instead of sending bytes to the embedded part URL:

```http
POST B/uploads/u_.../parts/1/authorize
Content-Type: application/json

{"size":8388608,"sha256":"<64 lowercase hexadecimal characters>","md5":"<canonical base64 of 16 bytes>"}
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
never assume a duplicate number means identical content. Concurrent attempts for one slot use isolated native
attempts or serialized provider writes; ambiguous in-flight attempts must be reconciled before reuse. The
service must also fence late native effects: a reused native slot cannot receive different content while an
older request is uncertain. Bind/verify chunk identity before irreversible native writes or isolate attempts,
using bounded spool where necessary. Acknowledgment
requires durable provider/staging bytes plus a durable receipt, not bytes in memory or an SDK progress callback.

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

Capabilities is authenticated and path-scoped (default visible root), returning effective `read/write`,
`createOnly`, `range`, `thumbnails`, supported checksums, maximum file/single-PUT/part sizes, pagination limits, session
expiry/retention policy, `sourceModifiedAt` support, and quota/reservation information appropriate to the caller. Numbers are authoritative
for admission but may change before a later request; new sessions bind their accepted limits. Upload responses
provide their exact chunk layout. Never expose provider names or assume every backend has the same limits.

`createOnly` is a boolean describing the root's restricted write strategy. When true, `write:true` permits
new file content only, through content PUT or create-mode upload sessions, using native atomic absence
conditions at publication. It does not permit replacement, folder creation, move, copy or path deletion;
those options return `422 unsupported_option`. Upload abort still cleans up only temporary state. Existing
prefix folders may be destination parents without a retained marker version; this does not promise their
identity or existence is frozen against external tools. `contentRevision` is null, content responses omit
strong ETags, and `pinnedRead`, `strongContentValidators` and `sourceModifiedAt` are false. No index revision,
ETag or timestamp is promoted into a native version. `createOnly:false` alone does not grant any write;
clients must also check `write`, `uploads` and the other effective capabilities.

External tools may continue changing a create-only root. Native absence conditions protect an occupied target
from application overwrite at publication; they do not prevent later external replacement or deletion. Scans
synchronize the observed index metadata. Budget counters retain the initial compared baseline plus application
journal changes; reconcile does not update them. External additions/deletions need separate reviewed budget
resynchronization, which is not implemented. Admission limits therefore do not enforce an exact global quota
across external writers.

| Status | Representative code |
|---|---|
| 400 | invalid_request, invalid_path |
| 401 | unauthenticated |
| 403 | forbidden (known visible resource, insufficient action permission) |
| 404 | not_found (also outside visible scope) |
| 409 | name_conflict, cursor_stale, upload_incomplete, part_conflict, part_out_of_order |
| 410 | upload_expired, upload_aborted, result_expired |
| 411 | length_required |
| 412 | precondition_failed, target_changed |
| 413 | request_too_large, file_too_large |
| 415 / 422 | unsupported_media_type / checksum_mismatch, unsupported_option |
| 416 | range_not_satisfiable |
| 428 | precondition_required |
| 429 | rate_limited (Retry-After) |
| 502 / 503 / 504 | storage_failure / temporarily_unavailable / storage_timeout |
| 507 | quota_exceeded, staging_full |

Retry transient requests with backoff/jitter and Retry-After. Resolve ambiguous mutations through their
session/operation/idempotency identity rather than assuming a timeout means nothing happened.

## 10. Design choices and acceptance

- One path-based API serves every client. Native/provider IDs and database keys stay private. Rename/move
  changes the public path, and S3 moves may copy bytes. Authentication does not change endpoint shape.
- One service part protocol supports both independent and ordered storage; native protocols stay internal.
  Plain streaming PUT avoids multipart overhead for small files. tus is not the public protocol: its core
  offset stream does not by itself provide the independent part slots/replay receipts chosen here.
- Native readable folders/names/raw bytes are mandatory. Leaving Everything requires ordinary native tools,
  not its database or export. Temporary staging is allowed; permanent anonymous content is not.
- Native mutations plus a durable journal/index repair replace the old database-only publication model.
  There is no universal atomic folder operation or transaction spanning provider and Postgres.
- Plain JSON/Problem responses and one operation resource remove legacy envelope and per-action polling
  exceptions. Paths have no version segment.
- No compatibility adapter or old client behavior constrains this specification. Existing data needs a
  deliberate backup/import decision at implementation time; freedom to redesign does not authorize deleting it.

Before shipping: exercise the same API conformance suite against local disk and COS/S3, then plain WebDAV
and a sequential-upload drive; cover create/replace races, parent moves, immutable part replay, lost responses,
commit/abort/restart/revocation, quota reservations, path-scope authorization after moves, range/HEAD correctness,
media safety, stale native effect blocking, partial tree recovery, readable no-database exit and scan/index repair. Measure tiny/large/mixed transfers,
LAN/high RTT/throttling/interruptions, memory/staging, request overhead, and egress cost. This document specifies
behavior; it does not claim the existing implementation or tests already meet it.
