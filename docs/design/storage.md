# Storage and the unified file API

> Decision accepted: 2026-10-30. Implementation pending.
>
> This document defines the target architecture and supersedes earlier requirements for client-to-COS
> uploads, provider download redirects, and a separate public data domain. The running implementation still
> uses those legacy paths; [the API reference](../api/api.md) describes them until migration ships.
> The implementation review in §9 records the current gaps. No runtime behavior changes with this document.

## 1. Decision and reasons

There are two conceptual layers:

1. **The API layer** is the only interface consumed by Web, mobile apps, CLIs, agents, and third-party apps.
   It owns file semantics and exposes one configured origin for metadata operations and file bytes.
2. **The storage layer** adapts local filesystems, S3-compatible stores, COS, cloud drives, WebDAV, and NAS
   services. Backend identity, credentials, references, and wire protocols stay behind the API layer.

Hosted clients use `https://everythingabout.me`; self-hosted clients use the operator's domain or LAN
endpoint. Origin means scheme, host, and port, not merely matching hostnames. Clients configure only the API
endpoint and application credential. They do not configure a bucket, region, provider SDK, or data domain.
All supported application upload/download flows, including previews and thumbnails, remain on that origin.
The API does not redirect file requests to provider origins or return provider upload addresses/credentials.

This boundary keeps downstream applications independent of storage integrations, makes hosted and local
installations interchangeable from a client's perspective, and moves authentication, retry, and protocol
translation into one maintained implementation. Adding a backend must not require downstream adaptations.

The previous design let clients upload directly to COS and follow download redirects to COS. That saves
service bandwidth but makes provider protocols part of the client contract and fits private NAS/local disk
poorly. We accept the bandwidth, gateway capacity, and operational cost of serving bytes through our origin
in exchange for a stable interface. Provider presigning may still be used internally between services.

```text
Web / mobile / CLI / agent / third-party app
                    |
          one configured API origin
                    |
API layer: authorization, namespace, index, search, quota, file operations,
           streaming/range responses, durable uploads, transfer scheduling
                    |
             storage driver contract
                    |
      local disk / S3 / COS / cloud drive / WebDAV / NAS
```

The API layer may contain file-service and transfer modules. CDN, data gateways, and NAS connectors are
internal deployment components, not additional public layers or storage protocols exposed to clients.

## 2. Responsibilities and file semantics

| Concern | API layer | Storage layer |
|---|---|---|
| Identity | Owner-scoped, NFC-normalized logical paths | Backend-native references |
| Authorization | Cookie/Bearer policy, scopes, write conditions | Provider authentication and access |
| Metadata | Directory index, search, attribution, logical revision | Native size, timestamps, validators |
| Bytes | Same-origin read/write endpoints, backpressure, resume | Native reads/writes and upload state |
| Directories | Consistent directory semantics, explicit recursive delete | Native directories or object markers |
| Move/copy | Consistent result and async lifecycle | Native operation when available |
| Performance | Request scheduling, caching, resource budgets | Optional native optimizations and limits |

Preserve path-based public identity, permanent deletion, explicit recursive directory deletion, and explicit
overwrite intent. File revisions used for concurrency/cache validation do not imply user-visible version
history. Keep Bearer-only public routes separate from cookie-only web routes; both adapters should invoke
the same internal file/upload services. A unified API contract does not merge the two authentication systems.

An operation must not silently weaken its promised semantics for a particular backend. For example, if the
caller requires a conditional overwrite, use a provider precondition or an API-managed publication mechanism
that can enforce it. If neither works, reject the operation explicitly. An API-only lock cannot prevent
concurrent writes made outside the application; describe the supported consistency boundary accurately.

The database remains an index and lifecycle store. File bytes never enter Postgres. External backend changes
are discovered through backend scans/change feeds and reconciled with the index; direct provider tools remain
operator import/export tools, not a protocol that downstream applications must implement.

## 3. Namespace and backend contract

Resolve `(owner_id, logical_path)` to a storage connection and native reference inside the service. S3/COS
keys, cloud-drive file IDs, WebDAV paths, and local relative paths are valid reference forms. Existing
`drive/<ownerID>/<path>` COS keys remain readable and do not need a bulk rewrite merely to add this boundary.
Centralize translation; handlers and analyzers must not construct provider keys independently.

A first implementation can have one default connection per installation or user. Retain connection identity
internally so later mounts can be introduced without changing public path operations. Mount management and
multiple writable replicas are not prerequisites for this work.

The following is a contract outline, not a finalized Go interface:

| Basic responsibility | Required behavior |
|---|---|
| Stat and enumerate | Native metadata, paginated listing, explicit absence/error behavior |
| Open a read | Streaming body; requested range and version condition when supported |
| Begin/write/finish/abort | A write lifecycle that can represent a stream or native upload session |
| Create directory | Real directory or backend-appropriate representation |
| Remove | Explicit file/directory scope; no implicit recursive deletion |

Keep optional accelerators separate: native move/copy, independent multipart writes, contiguous-offset
writes, conditional writes, atomic publication, batch operations, range reads, and change feeds. Describe
semantics and limits, not only booleans: chunk alignment, part count, size limits, allowed parallelism,
validator strength, checksum algorithm/scope, and publication guarantees.

A driver does not have to emulate STS or S3 multipart to be usable. An adapter can report read-only access
when writes are unavailable. API capabilities describe effective service behavior; provider capabilities are
internal inputs. Do not advertise native range or resume globally when the chosen route cannot deliver it.
Local drivers must enforce their configured root, including symlink handling, and keep owners isolated.

## 4. Public transfer contract

Ordinary clients use familiar HTTP operations at their configured origin:

```http
PUT /api/files/Notes/a.md
GET /api/files/Notes/a.md
HEAD /api/files/Notes/a.md
GET /api/files/Videos/a.mp4
Range: bytes=1048576-
```

The target GET returns file bytes (`200` or `206`), not a provider redirect. Retain a simple streaming PUT
for scripts and agents. Publish effective request limits; do not require multipart for every small file.
Zero-byte files are valid. JSON and error envelopes stay consistent with the application's existing APIs;
file content and HEAD responses use HTTP body/header semantics.

Larger or interruption-prone uploads use a durable, provider-neutral session. The following route shape is
illustrative; exact fields/routes must be finalized with the implementation and migration of existing clients:

```http
POST /upload                       # path, size, MIME, overwrite/precondition, idempotency key
PUT  /upload/{id}/parts/{number}    # bytes sent to our origin
GET  /upload/{id}                   # authoritative acknowledged progress and state
POST /upload/{id}/complete
POST /upload/{id}/abort
```

The current `POST /upload/complete` may be retained as a compatibility adapter. All returned transfer
locations must be relative or on the configured origin. The client follows our upload rules only; it never
negotiates a cloud provider's protocol. Sessions may expose an effective concurrency limit or acknowledged
offset when needed, without exposing why a backend imposes it. Gateway adaptation can parallelize reception
with durable staging when useful, but is not required to accept unlimited out-of-order data.

Required lifecycle properties:

- Bind owner, logical target, connection/configuration generation, native target, size, metadata, write
  conditions, and opaque provider session state at creation. Changing a mount must not reroute an upload.
- Persist acknowledged progress, expiry, publication state, and the committed result. Confirm progress only
  after the data is durable at the provider or in explicitly managed staging.
- Separate service-session lifetime from internal provider credential lifetime. Refresh credentials internally
  so an otherwise valid upload need not restart because a provider token expired.
- Make creation retries safe with an idempotency key and completion retries return the same committed result.
  Persist a completing state and recover provider-success/index-failure cases; blindly repeating provider
  finalize is not sufficient.
- Validate size and integrity using supported checksums. Provider ETags are opaque validators, not universally
  MD5 hashes. Avoid a mandatory full-file hashing pass before transmission.
- Protect readers and overwrite conditions during publication. Stage when necessary; use native atomic
  rename/conditional publication where available. Record the extra copy and weaker guarantees of backends
  that cannot publish atomically. Cancellation must stop future writes/publication through the API, with
  in-flight cleanup reconciled explicitly.
- Resume downloads against a single revision. Implement `Range`, `If-Range`, `Content-Range`, `206`, and `416`
  accurately for the ranges the API supports. Do not concatenate bytes from different object revisions.

On browser reload, a durable server session can survive while access to the local File does not. Persist
session identity and file matching information; request file reselection when browser permissions require it.

## 5. Deployment and acceleration

| Deployment | Internal byte path |
|---|---|
| Self-hosted, local disk | Same-origin handler streams to/from local files |
| Self-hosted, LAN NAS | Same-origin service or LAN gateway reads/writes the NAS |
| Hosted, object store | CDN/regional data gateway serves the origin and accesses storage internally |
| Hosted, cloud drive/WebDAV | Data gateway translates credentials, uploads, and reads |
| Hosted, private NAS | Connector near the NAS establishes an authenticated reachable service path |

One origin does not require one application process. Route metadata and byte endpoints to different internal
services when capacity justifies it. A reverse-proxied storage origin is acceptable only when our authorization,
HTTP semantics, and response handling remain enforced; provider redirects must not escape to clients.
Private NAS throughput still depends on the connector's uplink and network path. Plain self-hosted deployment
must remain useful without a CDN, edge worker, or distributed gateway fleet.

### Download caching

Use CDN caching for suitable reads, with streaming cache misses and measured range-cache behavior. Private
files require authorization before every cache hit. Cache identity must include owner, storage connection,
and immutable content revision (plus representation parameters), and honor response disposition as needed.
A logical path alone is insufficient. Never cache private content publicly merely because the request used
Bearer authentication or a cookie. If the CDN cannot safely authorize hits and resolve the current revision,
bypass shared caching and use the authenticated gateway.

Overwrites/deletes must update revision resolution or invalidate relevant entries. Do not cache provider
redirects or authentication errors as file content. Strip application cookies/Bearer tokens before any upstream
provider request, inject provider credentials internally, and avoid logging secrets. Same-origin untrusted
HTML/SVG content requires safe MIME/disposition, `nosniff`, and isolated preview policy; the former cross-origin
COS boundary must not be lost accidentally during migration.

### Upload ingress

An ordinary download CDN does not automatically accelerate uploads. Verify PUT/PATCH/POST support, request
body limits, buffering, timeouts, retry behavior, and streaming support. Route the same public upload paths to
suitable regional ingress or bypass incompatible CDN handling while preserving the public origin. Multipart
requests help fit ingress limits and reduce retries; they do not remove the gateway-to-storage bandwidth cost.

Stream with bounded buffers and backpressure; do not buffer a whole large request in the application. Align
service chunks with native provider chunks where possible. Stage only for an actual protocol or durability
need, with disk quotas, expiry, and cleanup. Plain WebDAV staging can preserve client progress while its
final upstream PUT still restarts on failure. Efficient range reads likewise need native access or a managed
cache; a gateway cannot invent them without extra I/O and storage cost.

## 6. Performance policy and validation

Optimize within the same-origin contract. Do not reintroduce provider addresses as an opt-in fast path.

- Small files: retain one-request PUT, reduce metadata round trips, batch control operations where useful,
  reuse connections, and allow modest cross-file concurrency.
- Large files: use native multipart/offset sessions behind the gateway, recover acknowledged progress, and
  avoid a mandatory extra full-file read or staging copy.
- Mixed workloads: schedule across files and parts so a large upload does not block every small one. Bound
  active requests, buffers, staging, and aggregate load per user/connection/backend.
- Adapt concurrency using observed throughput, latency, failures, throttling, and `Retry-After`. Respect backend
  limits. Fixed four-part or six-connection assumptions are not universal across HTTP versions and networks.
- Downloads: stream immediately when possible; support native media seeking. Parallel ranges are an internal
  or client-library optimization only when benchmarks justify their request and reassembly overhead.
- Move/copy: prefer native operations in one connection; use durable async copy/verify/delete otherwise.

Compare service behavior with the backend's native client on the same network and hardware. Measure total
throughput, time to first byte, time to first completed file, retry bytes, request count, CPU/memory, staging
I/O, and gateway egress cost. Include many tiny files, a large file, a mixed queue, high RTT, interruptions,
backend throttling, LAN disk/NAS, and authenticated CDN hits/misses/ranges. Record topology and resource
budgets; no protocol can guarantee a universal optimum or bypass a backend's own limits.

## 7. Compatibility and implementation sequence

1. Define the minimal storage/write lifecycle and same-origin streaming GET/PUT behavior. Keep the current COS
   driver working and add local disk to validate that the boundary has no object-store requirements.
2. Unify web and public upload services, add durable session/commit recovery, and switch clients to same-origin
   transfers. Preserve auth separation and migrate generated save-link/Connect instructions and media URLs.
3. Add capability-aware native acceleration and test with plain WebDAV plus a cloud drive that uses file IDs
   and sequential uploads. These catch abstractions that only happen to work for local disk and S3.
4. Optimize measured bottlenecks, add CDN/regional ingress where useful, and support private NAS connectors
   when needed. Multi-mount management can follow independently.

Existing presigned-upload clients require a deliberate compatibility/sunset window; do not silently change a
response from provider-authenticated to application-authenticated requests without updating clients. The
single-origin rule describes the final contract; legacy endpoints are transitional exceptions, not a second
permanent public transport. Keep current deployment instructions until their implementation is replaced.

## 8. Scope of this decision

Accepted now: two layers, one public origin for control and bytes, backend-independent clients, internal
acceleration, durable transfer semantics, and the performance constraints above. Storage SDKs and provider
credentials remain server-side. The old prohibition on streaming/proxying bytes is superseded.

Exact Go signatures, upload payloads, deployment products, numerical tuning defaults, and migrations are to be
specified and verified during implementation. The examples above do not register routes or change today's
limits. No new storage driver, CDN integration, or transfer endpoint is shipped by this documentation change.

## 9. Current implementation review

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
require the workload matrix in §6; passing the current unit tests is not evidence of transfer throughput.
