# Unified file API: implementation plan

> 2026-10-30. Plan based on source inspection at `ca79fdf`, after committing the approved path API.
> No runtime changes, database migrations, live storage probes, or performance measurements were made during
> this review. Existing `make test` passes; it does not establish the new storage/API guarantees.
>
> Contracts: [file API](../api/api.md), [architecture](storage.md), [driver boundary](storage-driver.md).
> This plan supersedes the earlier [historical audit's](storage-implementation-review.md) migration framing.

## P0 implementation status

P0 foundations are implemented in the contract fixtures, isolated native probes and read-only
`eam storage-baseline` command. See [boundary decisions and cutover runbook](storage-p0.md) and
[capability evidence](storage-p0-capabilities.md). These tests do not activate the replacement API.
Live COS evidence, production baselines and legacy-writer quiescence remain deployment gates;
normal unit-test success does not establish them.

## P1 implementation status

P1 adds read-only native Local/COS/S3 trees, startup backend selection, explicit application/mail
secrets and bounded no-prune index import. See [configuration, migration and validation](storage-p1.md).
Local startup without COS, actual Postgres import/repeat/rollback, native filesystem exit and SDK-backed
COS/S3 read conformance are tested. Public file read/byte routes remain P2; new backend writes remain
P3–P5. Live provider capability and production migration evidence are still deployment gates.

## P2 implementation status

P2 adds the shared authenticated metadata/list/search/me/capabilities routes and same-origin GET/HEAD
content, with explicit auth selection, cookie CSRF/origin handling, Problem responses and bounded scoped
cursors. See [active resources, limits and validation](storage-p2.md). The old file-path wildcard is retired.
Local/Postgres runtime and actual S3 SDK HTTP conformance are exercised; live COS and production cutover
remain unverified deployment gates. P3–P7 remain: durable writes, native actions, resumable ingress,
Web/media migration and release/performance acceptance. No new production write capability is activated.

## P3 implementation status

P3 implements the internal owner/root/principal-bound journal, persistent guards, leases, idempotent results,
logical/physical reservations, proven partial charging and native-success/index-repair coordination. Local
create-only staging/publication/recovery is opt-in for explicitly coordinated writers on admitted local
filesystems. See [native strategy, accounting and validation](storage-p3.md). Public writes remain disabled;
COS/S3 publication, full native namespace actions, resumable ingress and client cutover remain P4–P6.
Actual Postgres/local fault tests and repeatable migrations are exercised; production baselines and provider
quiescence/capability proof are still deployment gates. P4–P7 remain pending.

## P4 implementation status

P4 implements admitted coordinated Local PUT/create/conditional replace, native mkdir and finite-manifest
copy/move/delete, durable Operations and bounded restart/cancellation workers. An explicit `storage-admit`
command validates native/index baselines before budgets can enable write admission; configuration defaults off.
See [P4 admission, semantics and validation](storage-p4.md). Migrations 019/020 persist frozen action bindings
and evidence-bound content generations so proven moves retain contentRevision without database-only file moves.
Local/Postgres HTTP and fault-recovery tests are exercised. [COS/S3 mutation code](storage-p4-objects.md)
adds a separately attested single-gateway spool strategy, one-shot native requests, retained-version accounting
and permanent unknown-outcome guards. Real COS/S3 isolated probes were not configured in this implementation;
new object writes remain default-off and require exact-connection capability evidence plus a verified baseline.
P5–P7 and live production migration/provider acceptance remain pending.

## P5 implementation status

P5 adds owner/principal/root-bound persistent numbered-part sessions and watermarked durable SHA-256 receipts,
private Local chunks, atomic manifest freeze/abort/expiry, and restart workers that reuse P3/P4 Stage/Publish,
guards, accounting and saved results. See [P5 lifecycle, physical costs and validation](storage-p5.md).
Migration 024 is additive and repeatable. Object ingress uses the same durable gateway chunks and explicitly
rejects files beyond the admitted existing single-publication limit; native multipart is not claimed.
Real COS/S3 probes, production admission and old authority drain remain separate unverified gates.
P6 client/media/prompt cutover and P7 release/restore/performance acceptance remain pending.

## P6 implementation status

P6 switches Web resources, namespace actions, P5 uploads with same-page recovery, all media bytes and generated
Save/Connect instructions to the shared file API. Browser tasks remain in memory for the current Files page;
backend sessions and Operations retain durable recovery independently. Legacy file routes return 410; client
provider SDKs are removed. See [client behavior and browser acceptance](storage-p6.md) and the separate
[code-cutover versus authority-drain runbook](storage-p6-backend.md). Built-SPA Chromium acceptance uses
real Cookie/scoped Bearer, disposable Postgres and admitted Local roots. P7 release/restore/performance,
real COS/S3 probes, production baselines and actual old authority drain remain unverified gates.

## 1. Fixed implementation constraints

- One `/api/files` contract for Web, iOS, Android, CLI and third parties. No browser-only file routes, version
  segment, public file IDs, provider URLs, credentials or client storage SDKs in the finished implementation.
- Paths identify files/folders; rename/move changes the path. Cookie/Bearer authentication uses the same
  services, with explicit Authorization precedence, no invalid-header fallback, and CSRF checks for cookie writes.
- Committed bytes retain original accepted native names, complete folders and directly readable contents.
  Existing `drive/<ownerID>/<path>` COS keys are suitable roots; no bulk rename or anonymous-content conversion.
- Postgres remains the index/application/journal store. Native files must be useful without it. Never delete
  native content merely because a restored/missing index does not reference it.
- Native mutations and index updates are a recoverable workflow, not one cross-system transaction. Unknown
  final-path effects block conflicting mutations until safe resolution. Leases alone do not fence native I/O.

## 2. What exists and what must change

| Area | Verified current implementation | Consequence |
|---|---|---|
| Native layout | [Path mapping](../../backend/internal/paths/paths.go#L85) uses owner/path keys and folder markers | Reuse physical tree; treat new API leading slash as a boundary conversion, not a physical leading slash |
| Index and isolation | [Nodes](../../backend/internal/nodes/nodes.go#L81) queries are owner/path scoped; public [DTOs](../../backend/internal/api/api_files.go#L21) already omit internal IDs | Reuse path index, private PKs and owner checks; remove IDs from remaining Web DTOs rather than redesign identity around them |
| Auth | [Authenticate](../../backend/internal/auth/token.go#L488) already checks nonempty Bearer first and otherwise cookie; invalid Bearer does not fall back | Reuse verifier; record credential source, handle present empty/whitespace headers explicitly, centralize cookie-mutation CSRF and test mixed credentials |
| Routes/representations | [Router](../../backend/internal/api/server.go#L104) splits Web `/api/fs/*` and root public paths; wildcard `/api/files/{path...}` conflicts with the target resource family | Replace file route family and DTOs; use shared service calls, plain JSON and Problem Details, retain unrelated account/event APIs |
| CORS/Origin | [CORS](../../backend/internal/api/server.go#L231) has a short public header allowlist; [Origin check](../../backend/internal/httpx/httpx.go#L83) permits absent Origin and trusts forwarded host | Define trusted proxy/canonical-origin policy, explicit cookie CSRF policy, and preflight support for Range/digest/idempotency/revision headers without credentialed wildcard CORS |
| Driver/config | [Storage](../../backend/internal/storage/storage.go#L58) mandates presigning/STS/multipart; [server startup](../../backend/cmd/eam/server.go#L31) always constructs COS | New tree/staged-write capabilities and backend factory; local first, COS adapted, generic S3 next |
| Native reads/writes | [COS Open-equivalent](../../backend/internal/cos/cos.go#L78) has no range/version options; [Copy/Remove](../../backend/internal/cos/cos.go#L86) and [Put](../../backend/internal/cos/cos.go#L121) pass no target conditions | Verify native atomic/conditional support per operation; do not infer guarantees from S3 compatibility or ETags |
| Upload boundary | [Web sign](../../backend/internal/api/files.go#L161) returns COS configuration/STS; [public upload](../../backend/internal/api/api_upload.go#L164) signs final-path URLs | Replace with authenticated same-origin ingress; receiving bytes must not give clients independent write authority to final paths |
| Lifecycle persistence | [Upload schema](../../sql/006_upload_sessions.sql#L7) stores metadata/timestamps and [multipart ID](../../sql/009_upload_multipart.sql#L3); [complete](../../backend/internal/api/api_upload.go#L253) returns 409 on repeats and writes native storage before index/result | Add durable receipts, principal/root binding, immutable intent, result/idempotency records, native checkpoints and explicit unknown outcomes |
| Operations | [Jobs](../../backend/internal/jobs/jobs.go#L81) fail active work on startup, retry by rescanning, and [move](../../backend/internal/jobs/jobs.go#L310) copy/deletes each file immediately | Replace unsafe recovery with per-entry manifests/checkpoints, path guards and operation-specific reconciliation |
| Web/media clients | [Client](../../web/src/api/client.ts#L21) expects internal IDs/envelope and [upload credentials](../../web/src/api/client.ts#L190); [queue](../../web/src/pages/useUploadQueue.ts#L243) uses COS SDK | Migrate DTOs and file calls, retain session recovery within the current Files page and remove SDK; previews/event media must also use same-origin paths |
| Existing scans | [Reconcile](../../backend/cmd/eam/reconcile.go#L40) supports dry-run, native listing and inventory, but is COS-specific | Generalize safe index-only import/scan; fix name handling and derived folders before relying on recovery |
| Local config coupling | [Save-link MAC seed](../../backend/internal/auth/session.go#L26) depends on COS key or DB URL; [SES defaults](../../backend/internal/config/config.go#L120) borrow COS keys | Add a stable application secret and independent mail configuration; backend change must not accidentally alter credential identity |

### Correctness findings to cover before write cutover

These follow from inspected code paths; none was reproduced against a live deployment in this task.

1. **Old signed single PUT can overwrite newer committed content.** [Create](../../backend/internal/api/api_upload.go#L139)
   signs the final key; [supersession](../../backend/internal/api/api_upload.go#L377) only aborts native multipart
   and marks DB rows. The old single PUT remains independently authorized. New ingress plus safe publication
   must replace this behavior. Existing grants need bounded expiry/revocation handling at cutover.
2. **Web uploads bypass account quota.** [Sign/confirm](../../backend/internal/api/files.go#L124) do not call the
   shared quota check, unlike public PUT/session creation. [Quota](../../backend/internal/api/api_files.go#L593)
   calculates `used - old + new` but has no concurrent reservation, so the check alone cannot prevent overbooking.
3. **Copy-overwrite removes the old destination before admission/completion.**
   [Copy handler](../../backend/internal/api/api_actions.go#L259) purges target before quota checking and native
   copy. New contract rejects target collisions for copy; never carry over destructive pre-delete behavior.
4. **Move can report success with old native files left behind.** [moveOne](../../backend/internal/jobs/jobs.go#L319)
   logs source deletion failure and still updates the index. Missing source also allows an index-only move.
   New completion/result accounting must describe actual native outcomes.
5. **Scan is not yet a safe recovery gate.** [Inventory CSV](../../backend/cmd/eam/reconcile.go#L372) trims all
   cells, including keys. [Parent backfill](../../backend/cmd/eam/reconcile.go#L246) runs before absent-key deletion;
   derived parents are not recorded in scan_keys and can qualify for removal despite live children. Use tests
   covering repeated scans and transaction timestamps; preserve exact keys and report ambiguity/collisions.

6. **Search omits read permission enforcement.** [Search handler](../../backend/internal/api/api_search.go#L29)
   authenticates and narrows the prefix but never calls RequireRead before querying metadata. Prefix isolation
   alone does not enforce a read bit; include write-only/events-only token tests in the unified read gate.
7. **Current cursors are not safe to reuse unchanged.** [Cursor encoding](../../backend/internal/api/api_files.go#L137)
   stores original name/private ID without query/scope binding, while [SQL](../../backend/internal/nodes/nodes.go#L315)
   compares lower(name) against the supplied name. Replace with the approved sort representation and scoped
   opaque cursors; test uppercase names and query reuse, not just cursor encode/decode round trips.

## 3. Sequence and completion gates

The phases are dependency-ordered implementation slices. Each slice needs focused tests before merge;
no slice is a reason to activate a half-finished production write path. Local and COS validation can run in
parallel where the shared contract is fixed. Unknown provider capabilities are engineering gates, not user
product decisions to reopen.

### P0 — Freeze boundary cases and prove native safety

Write executable contract fixtures for path-only DTOs/routes, Cookie/Bearer selection, exact name handling,
revision conditions, Operation/Problem responses and native-tree exit. Define root `/` conversion to existing
internal relative paths without rewriting stored names. Resolve cookie CSRF token/origin handling for raw
uploads, including trusted reverse-proxy origin configuration. Snapshot current native paths/counts and index
metadata for migration validation, and back up owner-to-root/configuration mappings independently; do not
mutate customer storage as a capability experiment. Establish logical usage from a complete native/index
comparison, report unresolved names/effects, and preserve existing over-quota data. Restrict new admissions
rather than delete files to satisfy policy. Reservation backfill requires this baseline.

All native safety probes and early P2–P5 acceptance run against isolated roots. New production mutations may
not activate on a root until old single PUT URLs, STS/native sessions and in-flight legacy effects are drained
or safely revoked and reconciled. Those writers bypass new database guards. Strong reads while old writers
exist need real native version pinning or restricted admission, not an assumed application-only writer model.

Probe disposable local and COS fixtures for atomic no-replace/replace, exact range/version reads, conditional
multipart finalization/copy/delete, lost-response recovery and cleanup evidence. Inspect SDK retries and timeouts;
verify that delayed requests cannot mutate a newer final-path generation. Where unsupported, use a declared
coordinated writer model, an alternate safe publication strategy, read-only/limited capability admission, or
persistent unknown-outcome blocking. A Stat-then-write fallback is not a successful safety probe.

**Gate:** documented capability matrix with runnable positive/negative tests; no falsely advertised write or
range guarantee. No use of the application's live data for these probes.

### P1 — Native tree adapters and backend-independent configuration

Implement Tree/Entry/Read/NameRules, root binding and factory. Add local filesystem support and adapt COS's
native tree using existing owner prefixes; generic S3 gets explicit endpoint/region/path-style/auth settings,
not an assumption that COS is identical to AWS. Add native range reads and immediate-child pagination including
implicit S3 prefixes/empty folders. Separate content/staging roots and share bounded transport pools.

Separate immutable root/config identity from credential rotation, and application save-link signing secrets
from storage credentials. Preserve the existing signing seed explicitly when required; no accidental reset
when choosing local storage. Keep independent SES credentials/configuration for production email.

Generalize index-only scan/import with dry-run, exact key spelling, collision/error reporting, implicit-parent
handling and scan-completeness checks. A failed/incomplete scan cannot justify pruning index entries or native
files. Preserve internal node PKs/event relationships where existing paths are unchanged. Private database
IDs may remain; no public ID contract is added.

**Gate:** local works without COS configuration; local/COS tree conformance passes; existing native tree is
indexed without a byte rewrite, and ordinary tools can read it with Postgres unavailable.

### P2 — One authenticated read API and same-origin bytes

Introduce the target `/api/files` route family and authentication middleware. Detect Authorization presence
rather than silently ignoring an empty header; valid Bearer ignores ambient cookies, invalid explicit auth
returns 401. Cookie mutations require centrally enforced CSRF. Reuse existing sessions/tokens/path scopes.

Add plain path metadata/list/search/me/capabilities DTOs and Problem Details without changing unrelated APIs'
envelopes globally. The Web API client must keep separate parsing helpers for existing account/event endpoints
until those are intentionally migrated. Bind cursors to query/scope and stable sort; don't advertise snapshots.

Serve GET/HEAD content as streams, with correct length/type/disposition, Range/If-Range/206/416 and honest
validators. Update middleware ResponseWriter forwarding/Unwrap as needed for flushing, cancellation and efficient
copies; don't turn large file streams into buffered JSON. Preflight allows required public headers; trusted
cookie origin/CORS policy remains distinct from third-party Bearer access. No provider redirect reaches client.

**Gate:** identical HTTP contract works with cookie and scoped Bearer, including invalid-header tests,
owner/root isolation, `+/#/?/%` names, zero files, exact ranges and active-content safety.

### P3 — Durable mutation coordination and storage publication

Extract shared file/transfer services from handlers. Append re-runnable migrations (next available number
at implementation time; currently 015 is latest) for owner/principal/root generation binding, operation intent,
per-entry manifest/checkpoints, idempotency/results, path/subtree guards, upload receipt ownership, native
outcome evidence and logical reservations. Adapt/reuse existing rows carefully; no migration rewrites file bytes.

Implement Stage/Publish/Inspect/Discard and EffectRecovery. Journal before each final-path effect. Persist
unknown outcomes and retain conflicting-path guards across restart/lease loss; do not retry into a mutable
final destination until earlier effects are terminal or fenced. Reconcile native-success/index-failure and
record the same client result. Temporary cleanup is periodic/idempotent and never guesses from a missing index.

Reserve logical bytes consistently: new files reserve full size; replacement growth reserves positive delta
under serialized/version-bound credit, same-size replacement doesn't need a second full logical allocation.
Keep temporary/staging/provider-version usage under a separate physical budget. Settle proven partial native
results and retain unresolved liabilities before releasing reservations. Bind job/session access to creator
principal plus paths; deleting a target must not destroy the evidence needed to read one's operation result.

**Gate:** fault injection proves no stale overwrite, no false abort/success on unknown effects, no double quota
credit, and no native file removal from orphan/index-only assumptions.

### P4 — Safe simple writes and all file/folder mutations

Expose target path PUT through shared Stage/Publish logic with exact length/checksum/zero-byte handling;
return success only after native placement plus result/index settlement. Use native mkdir/rename/copy where
admitted. Replace copy-overwrite pre-delete and implicit unsafe parent writes with target conditions.

Implement finite-manifest copy/move/delete workflows. On non-atomic folder move, copy/verify all captured
destinations before source removal; only remove captured unchanged sources. Never rescan-delete external
additions. Native tree partial effects are explicit in Operation progress/results. Fast actions return resources
or 204; slow work returns 202. Worker claims cannot override native in-flight uncertainty. External editors
need coordinated writer policy/native checks; do not claim that DB locks protect provider UI writes.

**Gate:** native paths match resulting API paths after every success; collisions/ancestor races, partial
copy/delete, lost responses and cancellation preserve recoverable evidence and truthful status.

### P5 — Provider-neutral resumable ingress

Implement create/list/status/part PUT/receipt pagination/commit/abort on `/api/files/uploads`. Use fixed numbered
parts and server hashes, exact geometry, durable acknowledgement sequence, principal/root-bound state and
idempotent committed result. Stop auto-cancelling prior sessions solely because a path matches. Reject stale
target bindings before final effects and fail sessions whose required parent disappeared.

Local uses durable chunk staging/random-access temporary files; COS/S3 uses basic native multipart internally
when P0 verifies publication semantics. A bounded replayable spool is acceptable where necessary, but document
full-file staging/copy costs and enforce admission/disk budgets. Native receipts remain private. Before reusing
any native part slot or temporary-file offset, durably bind its byte identity/hash or isolate attempts;
differing concurrent/late payloads must never overwrite acknowledged bytes. Consume/hash digestless replays,
reconcile ambiguous effects before reuse, and ensure late attempts after abort cannot gain receipts or begin
publication. Define a narrow adapter-specific upload helper first; do not implement speculative drive token/
offset machinery before real providers need it. Public sessions and receipts must survive restart without
volatile ACKs.

**Gate:** lost part/commit ACK, identical/different replay, simultaneous uploads, expiry, restart, abort,
commit uncertainty and delayed differing attempts after acknowledgement/abort pass for local/COS. Huge files
fit both native and ingress/count limits before admission; small requests retain the one-PUT path.

### P6 — Web, media, prompts and cutover as one client release

Move browser file metadata/list/search/mkdir/move/copy/delete and upload queue to the same endpoints external
clients use. Replace Item's exposed ID/envelope assumptions with path metadata/Problem parsing; preserve path
router URLs, progress, cancellation and bounded fairness. Keep the browser queue only in memory for the current
Files page/document, preserving it during folder navigation and discarding it after refresh/close without failed
task restoration or File reselection. Register the browser-native beforeunload warning while queued, waiting,
uploading or finalizing tasks remain; remove it when none remain and do not rely on unload-time abort API calls.
Retain same-page session/create-key recovery, idempotency, receipt/hash verification and cancellation. Remove
only the current owner's legacy `eam.uploads.v1:<owner>` localStorage record without executing it. Pending delete
operation identities may remain owner-scoped in localStorage independently of the queue. Durable backend
sessions, recovery/expiry and accepted Operations continue unchanged; clients retaining session identity and
source bytes can use the resumable API across reconnects/restarts. Remove cos-js-sdk-v5 and legacy upload grants.

Serve preview/download/audio/video/image URLs through content paths, and event/timeline thumbnails through
authorized same-origin endpoints (or supported raw/placeholder behavior until transforms ship). Derived private
images inherit source access; they are not public merely because they're thumbnails. Update generated save/
Connect instructions, download links, browser code and exact prompt tests in the same cutover. Event authoring,
login and account APIs do not need an unrelated wholesale redesign.

Remove legacy file routes/signers after the controlled cutover, including wildcard path routes that can capture
new resource names. Wait out or revoke already issued single PUT/STS/multipart grants and reconcile old sessions
before declaring stale-writer protection complete. API incompatibility is allowed; issued native write authority
and existing data still need explicit handling. Do not keep a permanent second browser or provider fast path.

**Gate:** browser network traffic, including previews and thumbnails, stays on the configured origin and new
route family; curl/Bearer and browser/cookie do the same operations. No exposed file IDs/provider secrets/URLs
or storage SDK in client code, and generated instructions invoke actual new endpoints. Browser acceptance
also proves folder-navigation queue retention, native beforeunload registration for active tasks, an empty
queue after reload without failed-task restoration/reselection, current-owner legacy upload-record removal
without execution, and unchanged same-page retry/cancellation plus independent delete identity tracking.

### P7 — Release acceptance, then performance expansion

Test fresh install and repeated upgrades against disposable Postgres; owner/root/secret configuration survives
restart. Re-index existing COS tree without rekeying, preserving internal event references and native names.
Exercise a backup/restore drill for application metadata separately from native file portability. At the file
exit gate, settle operations, disable DB access and directly inspect/sync the native root with standard tools;
include empty folders, non-ASCII names, copied independence and ordinary raw files. No export interpreter.

Benchmark native versus gateway paths on the same machine/network: tiny batches, one large file, mixed queue,
LAN/high RTT, throttling, disconnect/retry, directory moves and simultaneous users. Record throughput/first byte,
first completed file, request/retry bytes, CPU/memory, staging I/O and egress. Set numeric release budgets from
measurements and deployment resources rather than claim an unmeasured speedup. Build Web assets and verify the
real deployment/proxy URL after refresh as part of runtime cutover; typecheck alone is insufficient.

**Gate:** all fixed requirements and first-release endpoints pass contract/integration checks. Private shared
CDN caching/edge auth, complex drive/WebDAV native resume, multi-mounts, cross-connection copy and deduplication
are later work. Generic S3/COS and local large-file transfer are in scope; performance is not deferred wholesale.

## 4. Suggested implementation units

1. Path/root/credential-source fixtures and native capability probes.
2. Local/COS tree adapters, configuration factory and safe index scan/import.
3. Unified auth + path DTOs/read routes + streamed content.
4. Durable journal/guards/results/quota migrations and staged-write recovery.
5. Simple writes and native file/folder operations.
6. Resumable same-origin transfer service and backend helpers.
7. Browser/media/prompts migration and old grant/route retirement.
8. Integration, upgrade/direct-exit drills and measured performance tuning.

Units 2's local/COS adapter work and unit 3's DTO/middleware work can overlap after unit 1. Mutation and upload
work depend on unit 4, and client write cutover depends on completed safe backend services. First implement the
read-only local/COS vertical slice to make the new boundary concrete, then the durable mutation foundation.
This plan authorizes no runtime actions by itself; implementation begins only on a direct request.
