# Storage and the unified file API

> Native file portability is a mandatory, user-confirmed requirement, 2026-10-30. Implementation pending.
>
> **User decision: native files must retain complete folder structure, filenames, and original bytes.**
> Leaving Everything must not require its database, a manifest interpreter, or a proprietary export tool.
> This supersedes the anonymous immutable-content layout in the earlier draft. Contracts:
> [file API](../api/api.md), [storage driver](storage-driver.md). Existing runtime behavior remains in
> [legacy-api.md](../api/legacy-api.md); [the source audit](storage-implementation-review.md) is historical.

## 1. Two layers, one public origin

The API layer owns user authentication, authorization, search/indexing, quota, transfer sessions and operation
coordination. The current delivery targets official COS and self-hosted Local/COS; other storage adapters remain
optional. Clients use one API origin and no provider SDK, signing secret or broad STS credentials. The official
EdgeOne upload mode is an explicit exception to the previous prohibition on client-visible provider parameters:
clients receive a short-lived signed part request under the same origin, containing the required native upload
parameters but no key material. Public file identity remains the native path.

All clients use `/api/files`: Web, iOS, Android, CLI and third parties. There is no browser-only file API.
Explicit Bearer authentication takes precedence; invalid Authorization never falls back to cookies. Without
Authorization, the session cookie may authenticate, with CSRF validation on mutations. Authorization/file
services are shared. Files/folders are addressed only by paths; database/provider IDs remain private.
There is no version path segment. Hosted origin is `https://everythingabout.me`; self-hosted origin
is the user's domain or LAN endpoint. The official service uses `FILE_UPLOAD_INGRESS=edgeone`: the application
checks the user, target and quota, then signs an exact COS UploadPart request. The browser sends that request
to a reserved path on the same public origin. EdgeOne ordinary forwarding removes the fixed route prefix,
sets the COS bucket Host and forwards the unchanged signed query, required headers and body directly to COS.
COS authenticates the request and validates its bound Content-MD5. No edge function, separate upload server,
full-file local assembly or application-server byte relay is involved. Application control endpoints verify
COS part state, persist receipts and retain sole authority to complete/publish and settle the index.

Self-hosting defaults to an embedded receiver in the application process, without a CDN or another service.
It accepts the same upload-session lifecycle and writes to configured Local or object storage. With COS,
parts are uploaded during reception; with Local, bytes remain isolated until native publication. Embedded
clients receive no native handles or signatures. The official signed transport is selected by session metadata;
clients do not select arbitrary storage providers or upload origins.

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
private database keys, permissions, annotations, sessions, operation journals and audit/events. File/folder
identity in the public API is the native path. Rename/move changes that path; clients update references.
Internal keys may help index maintenance, but are never exposed as file IDs or required to access files.

If the database is lost, files remain directly readable. A scan can rebuild paths and basic metadata. It does
not magically recover account credentials, ACLs, annotations, events, prior app IDs, or ambiguous outside
renames. Those application features need separate backup. Owner-to-root configuration/credentials must be
recoverable independently; never infer ownership by exposing all provider data to a new account.

Reconcile/import must index an existing readable tree without rewriting all file bytes. Native IDs can help
recognize external moves; when identity is ambiguous, do not guess from size/name alone. External edits are
supported through explicit scan/change feeds, not instant global linearizability. Operational guidance must
coordinate external writes with active application mutations. A database-only lock does not lock rclone,
Finder or another cloud client.

Keep native names intact. Advertise backend name/case/length limits; reject unsupported names and ambiguous
collisions before writing. Preserve display spelling where native Unicode normalization differs. Do not accept
a logical-only name that must secretly become an unrelated encoded native filename. Cross-backend transfers
must preflight collisions and report unsupported names instead of overwriting or silently escaping them.

## 4. File operations must affect storage

| Action | Native result and performance implications |
|---|---|
| Create folder | Real folder or standard empty-folder marker plus index entry |
| Rename/move | Native rename/move where supported; S3 directory moves require per-key copy/delete |
| Copy | Destination contains its own normal file/tree; reflink/provider-side copy preferred |
| Delete | Remove actual files/folders; database detachment alone does not finish deletion |
| Replace | New bytes become the ordinary file at the same readable path |

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
   logical quota and serialize conflicting file/folder operations.
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

Aborting an open upload can discard temporary data. Once final publication is in flight, cancellation is best
effort and cannot return a definitive aborted state while the native result is unknown. A successful late
publication is reconciled as committed, not hidden only in the database. Temporary cleanup never removes a
committed path merely because its final database result was lost.

## 6. Performance within the portability boundary

Prefer local streaming/range seeks, native rename, server-side copy/reflink, connection reuse, bounded
buffers/backpressure, native multipart and measured fair scheduling across files and parts. Clients share one
session/receipt/commit lifecycle with explicit embedded or same-origin signed transport. Provider request details
remain private except the narrow signed request needed for EO ordinary forwarding.

For COS numbered uploads, durable parts must reach native unpublished multipart storage during the upload
phase. Do not wait until commit to assemble a full local file and send it in a second whole-file PUT. Embedded
reception may buffer one bounded part for server hashing. Official signed mode keeps file bytes entirely off the
application and binds a client digest tuple before issuing authority; COS validates Content-MD5 and the app checks
native size/ETag before a provenance-labeled receipt. Session/native-part metadata live in Postgres, with no
extra receiver machine. Commit verifies coverage and current conditions, completes native multipart once and
settles the index/result; it is not merely a database update or another full-byte transfer.

The browser schedules four files concurrently and up to four numbered parts per file, respecting each session's
maxParallelism and ordering. This permits at most sixteen simultaneous browser part requests. Yield between
bounded batches so selected small files can progress. Transient retries query durable receipts before replaying
an uncertain part; acknowledged content is immutable and refresh/reselection verifies hashes before reuse.

Readable final paths can cost an extra publish copy on S3 or require per-file copies for a folder move. These
are accepted costs when unavoidable; never optimize them by quietly replacing the native tree with anonymous
objects. Benchmark staging-to-final copy, request overhead, gateway bandwidth, retry bytes and directory moves
at large scale. Local/S3 implementations should be validated first, while WebDAV/drive constraints remain
explicit in the driver contract.

Private CDN delivery requires authorization and revision-safe internal caching; otherwise bypass shared
cache. Ordinary CDN service does not automatically accelerate large uploads. All optimization stays behind
the configured origin. Active same-origin content still needs safe MIME/disposition and preview isolation.

## 7. Acceptance and implementation

The [implementation plan](storage-implementation-plan.md) maps this contract to verified source gaps,
dependency-ordered phases and release gates. It does not change runtime behavior.

Before replacing the current implementation:

- Prove direct exit using local tools and rclone/provider CLI with the database unavailable; no special export.
- Scan/index an existing native tree without content rewriting; verify file counts, names, sizes and available
  checksums. Keep old data safe through the transition.
- Test native names/case collisions, empty folders, owner/root isolation, external edits and index repair.
- Test lost responses before/after final native publication, stale requests after lease loss, unknown-outcome
  blocking, conditional replacement, cancellation and partial folder operations.
- Test upload receipts/restart/expiry and logical versus physical budget accounting.
- Keep application metadata backup/restore distinct from ordinary file portability.

The earlier source audit remains useful for COS implementation defects; its recommendations are historical
and must be interpreted against this native-tree decision. This revision changes design documents only;
no runtime data is moved.
