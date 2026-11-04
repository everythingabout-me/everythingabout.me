# P6 backend cutover and deployment gates

P6 retires the HTTP entry points of the legacy file protocols. It does not revoke old native authority,
perform production admission, rewrite native data or prove live COS/S3 guarantees. Backend validation uses
only isolated temporary Local roots, SDK HTTP fixtures and explicitly supplied disposable PostgreSQL.
The unified [file API](../api/api.md), [driver contract](storage-driver.md), [P4 admission](storage-p4.md),
[object admission](storage-p4-objects.md) and [P5 lifecycle](storage-p5.md) still define the write guarantees.

## HTTP and media behavior

- `/api/fs` and `/api/fs/*` return `410 application/problem+json` with code `legacy_file_api_retired`.
  This includes sign/confirm, download, listings, mkdir, rename and old deletion/job status.
- Root `/capabilities`, `/list`, `/search`, `/mkdir`, `/move`, `/copy`, `/delete` and `/upload` routes and
  their previously registered workflow subroutes are retired. They cannot mint PUT, multipart or STS grants,
  redirect downloads, invoke an old native mutation handler or fall through to a successful SPA page.
- `/api/files` remains the exclusive file family. There is no wildcard path PUT compatibility adapter.
  Small uploads use `PUT /api/files/content?path=...`; resumable uploads use numbered same-origin parts,
  explicit commit and durable Operations. Namespace changes use the admitted P4 actions.
- Account/login, Save link management, Connect management, `GET /me`, `PATCH /me`, `/events`, and the
  event stream remain separate APIs. Event authoring changes event metadata and references existing indexed
  files; it does not create native parents, publish bytes, move or remove files. The event queries resolve
  the external token ID to its owner-bound private database UUID before using the event foreign key.
- Event media and historical timeline snapshot covers/grids hydrate to origin-relative
  `/api/files/content?disposition=inline&path=...`. Names are encoded once, including literal `+`, `%`, `?`,
  `#`, spaces and Unicode. Every content request still authenticates and checks file scope. An events-only
  credential does not gain file access through the URL. A browser session can load the same URL directly;
  Bearer clients attach their token. No token appears in a media URL.
- Provider image transforms are no longer signed during event/timeline hydration. P6 uses original authorized
  content or a client placeholder; `/api/files/thumbnail` remains its existing authenticated 501 placeholder.
  It does not add a public thumbnail URL. Safe inline MIME, attachment handling for HTML/SVG, range reads,
  private caching and nosniff remain owned by the existing unified content handler.
- Generated Save/Connect instructions and Save JSON locations use the unified content/session protocol.
  Examples explain explicit parents, create/replace conditions, idempotency, exact part lengths, durable
  receipts, commit, Operation polling and admitted deployment limits. There is no "any size" provider upload.

Admission guard conflicts may carry `Problem.operationId`, identifying an existing owner-scoped workflow.
Clients may poll that exact `/api/files/operations/{id}` and retry admission only after success, before
sending bytes. Failed/cancelled/unknown or unavailable status keeps the local upload waiting or requires
user intervention. An ID does not grant Operation access: creator/scope and explicit owner-management rules
still apply; another app token receives a denial. There is no owner-wide Operations listing workaround.
Multiple blockers may exist; after one settles, a retry can identify the next blocker.

## Code cutover is separate from authority drain

Complete these stages in order for each actual connection/root generation. These are operator instructions,
not actions performed by this change. Preserve evidence outside the application repository and keep secrets
out of reports. Retain application/account metadata backups independently from native-content backups.

1. **Freeze legacy issuance and competing writers.** Deploy the router cutover to every gateway and stop or
   isolate older binaries, scheduled jobs and external native tools that can write the same tree. Verify old
   file endpoints return the retirement Problem, and inspect logs for stale clients. Coordinate the client
   cutover so old browser tabs cannot silently fall back to the old transfer protocol. Cached media/provider
   links already held by clients must be considered separately from new same-origin hydration.
2. **Inventory existing authority and accepted work.** Record issued single PUT/download URLs, STS sessions,
   multipart upload IDs/parts/completion authority, legacy upload/index observations, browser receipts, deletion
   manifests, active jobs, process identities and outstanding provider requests for the exact root binding.
   Preserve journals, manifests, version inventories and spool state. Legacy recovery code remains available
   for already accepted work; absence of a routed handler does not prove those workflows settled.
3. **Drain or fence with actual provider evidence.** Revoke/rotate credentials or change provider policy where
   appropriate, but do not assume rotation invalidates every temporary credential or already accepted request.
   Establish the relevant provider semantics and terminate or fence old publishers. Resolve retained multipart
   sessions and uncertain final requests using a reviewed provider-specific procedure. A numbered upload
releases its reservation as soon as its abort gets a definite answer; for an allocation whose abort answer can
never be obtained, `eam storage-resolve-upload --operation-id …` remains the fallback and requires provider
`NoSuchUpload` evidence before it releases retained physical liability. A database completion,
   cancellation, TTL expiry, elapsed wait, process death or successful HEAD does not prove older native requests
   terminal. Download grants can also remain readable until their actual provider expiry/revocation. If native
   quiescence cannot be proved, keep replacement writes disabled. Never label an unknown request aborted merely
   to clear a guard, and never delete recovery rows/spool/lock files as an unblock mechanism.
4. **Settle and compare the native tree.** Finish/reconcile already accepted legacy work under its original
   binding before admitting new writers; inspect failures and retained liabilities. Run read-only reconcile
   dry-run, inspect exact names/kinds/parents/sizes and native versions, import the index, then repeat the
   comparison. Inventory retained object history for physical accounting. Keep owner-to-root mappings and
   configuration generations recoverable independently of the database. Do not infer ownership from arbitrary
   provider keys. Legacy helper tests remain internal recovery coverage, not public route compatibility.
5. **Prepare explicitly admitted deployment configuration.** The application's default COS configuration may
   still have `FILE_COORDINATED_WRITES=false`. With P6 it can read through the unified API but its new writes
   remain disabled; retiring the old routes is not permission to turn that flag on automatically. Before a
   write-serving cutover, an operator must prepare an admitted Local configuration or an exact-connection
   COS/S3 capability report, durable shared spool/lock namespace, version inventory and declared coordinated
   writer model as specified by P4. Real provider positive/negative probes remain a deployment gate. Configure
   `FILE_COOKIE_ORIGINS` explicitly for session mutations. Review advertised limits. The subsequent
   [native upload extension](native-upload-deployment.md) requires multipart capability evidence. Official
   COS uses edgeone mode with narrow per-part signatures, separate UploadPart-only signing credentials and
   native confirm before receipts; no extra receiver process is needed. Prove unsigned action-selector headers
   cannot turn the signing principal's grant into UploadPartCopy/other writes. Drain existing sessions and
   issued authority before changing transport, native strategy or admitted connection identity. The old P5
   whole-file staging/PUT limit is historical.
6. **Initialize the verified root budget before serving writes.** Run `storage-admit --owner-id ...
   --writers-quiesced --evidence /secure/baseline-proof.json --dry-run`, review it and repeat without dry-run.
   Object admission additionally requires `--physical-bytes` covering retained versions and the required
   `retainedVersionInventory` evidence. Follow P4's empty system/spool admission requirements; do not start
   mutation wrappers first. Missing or mismatched baseline must fail closed; the only exception is the
   zero baseline the server establishes for a brand-new owner whose index, accounting and native root are
   all provably empty. Any owner with a prior row still requires the command above.
7. **Verify the new path.** With test data inside the admitted root, confirm cookie Origin/CSRF and Bearer
   isolation, same-origin preview/download with no provider redirect, restart/resume part receipts, commit
   identity, Operation completion, explicit recursion, conditional replacement and native-file portability.
   Compare the actual ordinary tree and bytes using standard native tools. P7 restore/performance/release
   acceptance is separate. Do not declare it complete from unit tests or SDK fixtures.

Do not roll back by restarting a legacy issuer against an admitted coordinated root: that restores writers
outside the new guards. Prefer disabling ingress/writes while preserving reads, journals, spool and unresolved
liabilities, then use a reviewed recovery plan. Any deliberate return to an older writer model needs its own
coordination and drain decision.

## Targeted validation

From the repository's backend module, using an explicitly disposable database and a writable task cache:

```bash
GOCACHE=/path/to/repository/.gocache \
EAM_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:63204/postgres?sslmode=disable' \
  go test ./internal/api ./internal/events ./internal/timeline ./internal/filecontract ./internal/fileops
```

P6-specific runtime tests cover retirement before nil legacy services/SPA, generated transfer instructions,
real P5 ingress/commit, event create/update/list/detach and same-origin media authorization, exact blocking
Operation identity and foreign-creator denial. Timeline hydration tests replace historical provider URLs.
The actual CLI server test reads through unified listings and expects retirement of legacy mutations.
No production environment, real bucket, live provider mutation or real authority revocation is part of this run.
