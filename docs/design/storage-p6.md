# P6: one file client release

P6 switches the browser, media hydration, Save/Connect instructions and routed legacy file protocols together.
P5 was accepted and merged first. Read the [backend cutover and authority-drain runbook](storage-p6-backend.md)
before deployment; this code does not revoke old PUT URLs, STS, native multipart sessions or accepted requests.
Real COS/S3 proofs, production baselines and old-writer quiescence remain separate unverified gates.

## Resources, uploads and recovery

Web consumes the same `/api/files` plain resources and Problem Details as scoped Bearer clients. `Item` aliases
the public FileResource; path is identity. Internal node IDs, legacy `is_dir`/snake-case DTOs and file envelopes
are removed from browser consumers. All file requests pass through the shared API client. Account and event
management retain their intentionally separate envelope APIs. Cookie mutations obtain the session-bound CSRF
token and preserve explicit Origin checks; no invalid Bearer fallback is introduced.

The browser queue uses P5 sessions, exact numbered parts, server receipts, explicit commit and standard
Operations. Four files can advance concurrently and each sends up to four parts per scheduler turn, capped
by the session's maxParallelism and ordering (at most sixteen byte requests). Bounded batches yield so smaller
files can advance beside large files. Transient part errors allow three total attempts with 250/500 ms backoff;
each uncertain result queries and verifies durable receipts before any identical replay. Parallel progress sums
confirmed bytes and current per-part progress without double-counting. Folder selection/drop retains all original paths, parents and explicit empty
folders; existing folders merge, and file overwrite uses a confirmed observed contentRevision. Folder rows
aggregate descendant progress/failure/cancellation. Failed siblings do not block unrelated queue roots.

The queue persists owner-scoped task IDs, session IDs, original idempotency keys, accepted create intent and
Operation waits in localStorage before allocation/effects. It stores no provider credentials, URLs or File bytes.
A lost create ACK replays the same key; a lost part ACK queries receipts; a lost commit ACK queries the original
session/Operation. Committing and outcome_unknown never trigger a new publication. Refresh recovery may ask the
user to select the original File. Every acknowledged receipt is compared with that selected File's part hash
before skipping it. A different same-size File cannot mix into the saved upload. Verification is cached only
for the same File object and receipt identity, then released on completion/cancellation. A queued task with no
original File and no previously captured intent waits for reselection rather than inventing MIME metadata.
Local roots advertise sourceModifiedAt support; browser timestamps are sent only when explicitly supported.

Official sessions declare cos-signed transport: the shared client lazily computes per-part SHA-256/MD5,
authorizes an exact short-lived request, confirms any earlier native result, sends bytes through the same-origin
EO transfer path and obtains a storage-verified confirm receipt. The COS XML/empty upload reply is not a public
receipt. No application Authorization/CSRF header is attached to the byte PUT; EO strips ambient browser cookies
and provider operation-selector headers. Signed receipt checksumSource identifies client SHA-256 plus
COS-verified MD5, not server-computed SHA-256. Embedded transport retains its existing server hash boundary.

Cancellation aborts the current byte request, then reconciles/aborts the exact server session. Lost allocation
ACKs are recovered before cancellation. Committing work uses Operation cancellation. Already-committed races
retrieve and report the saved result; they cannot hide or delete published content. Cancellation identity
survives refresh. Only authoritative failed/aborted/expired results allow an explicit retry to allocate a fresh
key; a transport error, missing result or unknown effect does not. State callbacks and polling are fenced after
unmount, and request deadlines/abort controllers are cleared.

Deletion and other admission blockers carry an exact operationId. The browser persists pending delete identities
and keeps selected Files waiting before any new bytes at those paths. Queued/running/unknown/failed/cancelled,
wrong identity and unreachable status never mean success. Only that Operation's succeeded result permits retry;
new admission may identify a further blocker. Failed/cancelled/unknown cleanup remains visible after refresh.

## Media and retired protocols

Preview, download, images, audio/video and event/timeline media use configured-origin content URLs. Historical
provider URLs are overwritten during hydration; browser consumers ignore supplied media.url fields. Timeline
raster previews verify metadata and use raw content or placeholders. There is no public provider thumbnail URL;
unsupported thumbnail conversion remains the authenticated 501 endpoint. Active HTML/SVG and playlist files use
the download fallback. Authored sandbox HTML restricts media to the configured origin/data/blob and blocks
remote CSS/fonts. Derived content does not gain separate public permissions. Existing safe MIME, nosniff,
disposition, range and private-cache enforcement remain at the shared content boundary.

The browser COS SDK and its exclusive XML dependencies are removed. Legacy `/api/fs/*` and root file actions
return 410 before any old handler or signer. Account/login, Connect and event authoring remain active. Save and
Connect generated instructions call real content/session/parts/commit/Operations resources and describe exact
limits and parent handling. Legacy accepted-work recovery code stays available for controlled drain; deleting
routes alone does not establish stale-writer fencing.

## Validation

All 132 Web tests pass after the signed-transport extension. Web tests exercise the actual transformed queue/client/persistence source, not an independently implemented
model. They cover lost create/part/commit ACKs, immutable receipt verification, refresh/reselection, owner
isolation, bounded fairness, folder merge/dependencies, exact deletion waits, timeout/unmount, partial failures,
terminal retry and cancellation races. Path tests cover absolute resources and literal encoded-looking names.
The production SPA build passes; heavy optional preview dependency chunk warnings remain build warnings.

The opt-in browser test starts a real HTTP service using the built SPA, random disposable PostgreSQL database,
explicit admitted Local roots and genuine Cookie/scoped Bearer credentials. Chromium uses the actual file
picker and network. It covers special filenames, publication/result settlement, preview/download, refresh
between acknowledged parts, reselection without resending confirmed bytes, pre-commit cancellation, partial
failure isolation, exact deletion waits, scoped Bearer isolation and same-origin traffic. Browser
fixtures deliberately interrupt requests; no production bucket, real user root, .env or credentials are read.

```bash
make test
cd backend
EAM_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:PORT/postgres?sslmode=disable' \
  go test -race ./internal/api ./internal/fileops ./internal/uploadspool ./internal/events ./internal/timeline
# Build web/dist first. The driver is installed separately from application dependencies.
EAM_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:PORT/postgres?sslmode=disable' \
EAM_TEST_PLAYWRIGHT_MODULE=/absolute/path/to/playwright-core/index.mjs \
EAM_TEST_CHROME='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome' \
  go test -v ./internal/api -run '^TestFilesBrowserAcceptance$' -count=1
```

P0–P5 code foundations and P6 client/routing cutover are implemented. Live provider evidence, production
admission and old-authority drain remain unverified. P7 fresh-install/upgrade, metadata restore, no-database
native exit and measured deployment performance/release acceptance remain outstanding. The subsequent
[native upload extension](native-upload-deployment.md) replaces object whole-file staging/PUT at commit with
native multipart and signed COS part requests through ordinary EdgeOne forwarding. Small content PUT retains
its separate limit and is disabled in edgeone mode. The superseded standalone upload gateway has been removed;
real provider admission/IAM probes and live EO routing remain distinct from implementation tests.
