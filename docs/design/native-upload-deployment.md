# Native uploads and receiver deployment

This implements the user-confirmed upload scope: the official service uses COS; self-hosting uses an embedded
receiver with Local or configured object storage. Every client uses one origin and the same `/api/files`
numbered-part protocol. Other cloud-drive protocols are future work, not a dependency of this delivery.

## Byte and control paths

| Deployment | File-part bytes | Control and completion |
|---|---|---|
| Official / external ingress | Browser -> same-origin front router -> `eam upload-gateway` -> unpublished COS multipart | Browser -> application API -> native completion, index and saved result |
| Self-hosted COS | Browser -> `eam server` embedded receiver -> unpublished COS multipart | The same process performs completion and index settlement |
| Self-hosted Local | Browser -> `eam server` -> private durable local part storage | The same process verifies and publishes through the Local driver |

COS/S3 numbered parts reach native storage before the public receipt is acknowledged. Commit freezes complete
coverage, seals the native manifest without reading the file body, performs native completion once, and settles
the existing Operation/index/result. It does not assemble a whole local file or send a second whole-file PUT.
Commit is not merely a database update: native completion and its uncertain outcomes still require journaling.
Local publication retains its existing durable staging implementation; it is not required to emulate COS RPCs.

An individual native part may be buffered within the assigned part bound to hash/verify its immutable identity
before writing a provider slot. This buffer is not a full-file publication source. Native upload IDs, provider
ETags, credentials and metadata handles remain private. Public receipts contain number, exact size and SHA-256.

## Parallelism and recovery

The browser advances four file tasks and up to four parts per file, capped by the session's maxParallelism and
ordering. This allows at most sixteen simultaneous data requests; browser/network transport may use fewer
connections. Bounded part batches yield to later files, rather than reserving all file slots for an entire large
file. Progress combines unique acknowledged bytes and active part bytes. Both embedded and external receivers
apply a 60-second read-idle deadline, renewed before each body read and cleared after handling; this is not a
60-second total upload limit. Metadata lock admission reserves database connection capacity for receipt commits.
The four-by-four browser limit is a scheduling ceiling; gateway memory, database capacity and native provider
limits may apply backpressure below sixteen active native requests. Size the receiver's connection pool and
bandwidth from measured load rather than assuming sixteen requests guarantee a particular throughput.

Transient part failures receive at most three attempts with 250/500 ms backoff. A lost ACK is resolved by querying
the original session and validating its receipt before replay. Original create idempotency keys and exact session
identities survive refresh; a reselected File must match every acknowledged part hash. Cancellation aborts the
active batch and reconciles/aborts the exact server session. A committing or unknown native effect never permits
a fresh publication merely because a request timed out.

Native allocation identities and immutable part metadata are owner/root/configuration-bound records in Postgres
(migration `025_native_upload_metadata.sql`). Separate application and receiver machines share this metadata,
not a filesystem containing file bytes. Per-session/per-part concurrency coordination applies across receiver
processes. The application's existing publication ledger still requires the admitted durable application-side
metadata/lock namespace; receiver storage is independent. Keep both database and native session evidence for
restore/recovery; a database rollback is not proof that native parts never existed.

## Self-hosted embedded receiver

`FILE_UPLOAD_INGRESS=embedded` is the default. Run `eam server` as usual after migrations, backend configuration,
explicit coordinated-write admission and verified logical/physical budgets. No additional service or CDN is
required. The application receives bytes in this mode. COS/S3 require exact-connection `multipartUploads: true`
evidence before their numbered native multipart route is enabled; adding this field is an operator assertion
based on actual positive and negative provider probes, not a substitute for those probes.

Parts remain 8 MiB, with four requests per session and at most 10000 parts (80000 MiB before lower provider or
budget limits). Native numbered object uploads are independent of `FILE_MAX_SINGLE_PUT_BYTES`. The small direct
content PUT retains its separate configured default 32 MiB / object maximum 5 GiB policy. Local native limits
and available budgets still apply. Capabilities report the actual active limits. Zero-byte sessions have no
parts and use their existing guarded zero-byte native publication.

## Official / external receiver

1. Apply all migrations, including native upload metadata. Establish the exact root's admission and native
   multipart evidence, retained-version inventory and legacy-writer drain before serving new writes.
2. Configure the application with `FILE_UPLOAD_INGRESS=external`. It still serves authentication, create/list/
   status/receipts/commit/abort and Operations. Part PUT and simple content PUT return a Problem before reading
   their bodies; maxSinglePutBytes is zero. External clients must use sessions.
3. Run `eam upload-gateway` on a separate host with its own listen address, the same database, stable APP_SECRET,
   approved public FILE_COOKIE_ORIGINS, storage connection/generation/owner-root mapping, provider credentials
   and multipart admission evidence. It serves only exact numbered-part PUT routes, with the same live auth,
   scope, Origin and CSRF validation. It does not serve login, the SPA, upload control endpoints or workers.
4. At the one public HTTPS origin, route `PUT /api/files/uploads/{id}/parts/{number}` to the gateway. Route all
   control requests to the application. Do not configure a fallback that forwards a failed upload to the
   application. Do not redirect the browser to a bucket URL or another origin.
5. Disable caching for upload/control endpoints. Preserve the method, full path, Content-Length, content type,
   Origin, Cookie, Authorization, X-CSRF-Token and optional Content-Digest. Set body limits/timeouts for the
   advertised part size; disable proxy full-request buffering when streaming ingress is required. Keep private
   service origins inaccessible to unauthorized clients except through the same authenticated boundaries.
6. Verify that a part causes native UploadPart before its 201 receipt, and that commit sends native completion
   but no object-body PUT/download/assembly on the application host. Test owner isolation, dropped receipts,
   receiver restart, cross-host commit, abort races and native unknown outcomes before enabling real traffic.

A CDN/load balancer provides same-origin routing. It must support the required methods, request bodies and
timeouts. Ordinary download cache support is insufficient. This repository does not provision a CDN account,
DNS, TLS or a cloud receiver host; those remain deployment configuration.

## Publication and cleanup guarantees

Native multipart stays unpublished until commit at the original readable path. The captured target/parent and
current authorization are checked before completion, with admitted native conditions. Reusing a part number
with different bytes is rejected even after a failed or ambiguous earlier attempt. Successful native response
plus a durable service receipt is required before acknowledging that part.

Publication guards also serialize commit against receiving parts. Native physical reservation is
`2*S + 4*min(S, 8 MiB)` for file size S, covering native multipart/final coexistence and bounded replay buffers;
Local retains its existing conservative staging reservation. A lost native part response can leave an older
request uncertain even after a same-byte retry succeeds. If such a session is aborted before definitive native
completion, cleanup retains physical liability for provider/operator reconciliation rather than pretending
Abort alone proves all older native requests terminal.

Final completion is sent once per guarded publication attempt. Ambiguous final effects retain the existing
outcome_unknown state and path guard; HEAD alone does not prove old effects terminal. Cancellation does not
remove a committed file. Cleanup aborts only the recorded unpublished upload and never deletes the final path.
Lost allocation replies still require provider multipart lifecycle/recovery evidence; do not claim exactly-once
allocation or blindly expire guards for unknown final effects. Provider multipart lifecycle rules must retain
acknowledged parts for at least the advertised session lifetime (up to seven days). A lost native upload is never
silently replaced underneath acknowledged receipts. Unit and SDK-fixture integration tests do not constitute
positive/negative capability evidence for a real COS bucket.

Existing P4 object single-PUT sessions/metadata must be drained or explicitly migrated before changing storage
admission on a live connection. Route removal and configuration changes do not revoke old native authority.
See [the authority-drain runbook](storage-p6-backend.md) and [driver guarantees](storage-driver.md).
