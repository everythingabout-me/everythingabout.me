# Current implementation review

> Historical source audit of the COS predecessor. Target contracts are [the file API](../api/api.md) and
> [the driver specification](storage-driver.md); this audit is not a requirement to preserve the old design.


Reviewed against repository baseline `a661b1f` on 2026-10-30. This is a source-level review, not a throughput
benchmark or live COS/Postgres/CDN integration test. Correctness findings below are established by the code
paths but have not been reproduced against a running deployment. Architecture gaps describe migration work,
not production incidents. This documentation change does not fix runtime behavior.

### Correctness findings to address

1. **A superseded single PUT can overwrite a newer completed upload.**
   [Session creation](../../backend/internal/api/api_upload.go#L139) signs a PUT to the final path key.
   [Supersession](../../backend/internal/api/api_upload.go#L377) aborts multipart uploads and cancels database
   rows, but [single PUT signing](../../backend/internal/cos/cos.go#L225) grants storage access independently
   for one hour. Sequence: U1 receives a URL; U2 supersedes U1, writes B and completes; U1's still-valid URL
   writes A to the same key. Rejecting U1's completion does not prevent B's indexed metadata from describing A's
   bytes. Introduce per-session staging/generations and enforce the publication decision against stale writers.
2. **Browser uploads bypass configured account quota.**
   [Web sign/confirm handlers](../../backend/internal/api/files.go#L124) never call `checkQuota`; confirmation
   indexes the object without a durable admitted session or a comparison with the admitted size.
   [Public PUT](../../backend/internal/api/api_files.go#L401) and
   [public upload creation](../../backend/internal/api/api_upload.go#L134) do check quota. Share admission,
   quota reservation, and completion across both adapters; enforce the actual committed byte count.

### Architecture and lifecycle gaps

| Area | Current evidence | Required migration |
|---|---|---|
| Upload origin and provider opacity | [Web sign](../../backend/internal/api/files.go#L161) returns bucket/region/key/STS; [browser queue](../../web/src/pages/useUploadQueue.ts#L243) calls the COS SDK; [public upload](../../backend/internal/api/api_upload.go#L164) returns provider PUT/part URLs | Add authenticated same-origin byte ingress and remove provider SDK/credentials/URLs from clients |
| Downloads and all media | [Public GET](../../backend/internal/api/api_files.go#L299) and [web GET](../../backend/internal/api/files.go#L283) return COS URLs/302; [event thumbnails](../../backend/internal/events/events.go#L406) sign COS processing URLs | Stream files and transformed media through our origin; handle HTTP ranges/version conditions at the API boundary |
| Driver contract | [Storage](../../backend/internal/storage/storage.go#L37) mandates provider URL signing, COS credential shape, multipart, server copy, and markers; read has no range/version options | Introduce basic storage/write lifecycle and optional native capabilities; keep provider processing expressions internal |
| Backend selection | [Startup](../../backend/cmd/eam/server.go#L31) always validates COS and constructs `cos.New` | Backend-specific configuration/factory and a local driver; preserve the existing COS implementation during migration |
| Namespace | [Key translation](../../backend/internal/paths/paths.go#L85) and [node upsert](../../backend/internal/nodes/nodes.go#L181) assume owner/path-derived object keys | Resolve connection plus opaque native reference centrally without changing logical public paths |
| Durable completion | [Complete](../../backend/internal/api/api_upload.go#L253) rejects repeats with 409 and finalizes provider state before index/session writes; [session schema](../../sql/006_upload_sessions.sql#L7) plus [multipart state](../../sql/009_upload_multipart.sql#L3) lack stored result and connection generation | Durable completing/result state, backend binding, idempotent completion, and recovery after provider-success/database-failure |
| Resume, expiry, cancellation | [Session/URL TTL](../../backend/internal/api/api_upload.go#L22) is one hour; [status](../../backend/internal/api/api_upload.go#L501) only lists native parts; [routes](../../backend/internal/api/server.go#L136) have no explicit abort; cleanup runs on new upload creation | Internal credential renewal, provider-neutral progress, explicit abort and scheduled cleanup; browser session recovery |
| Effective limits and scheduling | [Browser queue](../../web/src/pages/useUploadQueue.ts#L16) fixes one file/four parts; [web sign](../../backend/internal/api/files.go#L143) caps at 48 GiB, while [public sessions](../../backend/internal/api/api_upload.go#L112) cap at 5 TiB and default to single PUT through 5 GiB; web signing and public session creation reject zero bytes | One effective policy and fair bounded scheduling based on backend limits and measurements; consistent zero-byte support |
| CDN and revision identity | [Mutable keys](../../backend/internal/paths/paths.go#L85) and [upserts](../../backend/internal/nodes/nodes.go#L181) have no application publication revision; current byte paths redirect | Add authenticated same-origin cache delivery and owner/connection/revision partitioning before enabling private shared caching; no current cache leak was demonstrated |
| Move/copy and workers | [Object jobs](../../backend/internal/jobs/jobs.go#L300) assume copy/delete; [startup recovery](../../backend/internal/jobs/jobs.go#L81) fails all active jobs under an explicit single-instance assumption | Prefer native move/copy; add worker claims/leases before scaling job workers. Multiple byte gateways alone do not require multiple workers |

### Existing foundations and verification gaps

Retain [dependency injection](../../backend/internal/api/server.go#L43),
[owner-scoped index queries](../../backend/internal/nodes/nodes.go#L81),
[NFC/path validation](../../backend/internal/paths/paths.go#L31), the existing
[streaming small-file PUT](../../backend/internal/api/api_files.go#L417), persisted upload/job rows, public
size verification, and the existing async `202 + Location` convention. A substantial amount of API/file
semantics can survive the migration.

Current [API tests](../../backend/internal/api/api_test.go#L20) largely cover routing, helpers, capabilities,
and generated prompts; [job tests](../../backend/internal/jobs/jobs_test.go#L8) cover identifiers/terminal
states; [COS tests](../../backend/internal/cos/cos_test.go#L5) cover STS path escaping. They do not establish
storage-driver conformance or end-to-end byte/lifecycle behavior.

Implementation needs conformance/integration coverage for local and COS drivers, same-origin transfers and
media, absence of provider credentials, owner isolation, GET/HEAD/range behavior, conditional publication,
stale-writer rejection, quota parity, interrupted/repeated completion, abort/expiry/restart recovery, and
private-cache authorization/revision freshness when CDN support is introduced. Performance claims additionally
require the [performance validation in the architecture](storage.md#6-performance-within-the-portability-boundary);
passing the current unit tests is not evidence of transfer throughput.
