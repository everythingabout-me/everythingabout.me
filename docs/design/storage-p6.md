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
Operations. Up to eight files hold open sessions; their parts share one global pool of sixteen byte requests
(a single file may use all sixteen, capped by the session's maxParallelism and ordering). A freed slot goes to the
earliest-started file's next part. Transient part errors allow three total attempts with 250/500 ms backoff;
a receiver-busy `429` is backpressure rather than a failed attempt, so the part waits (backoff up to 2 s, bounded
waits) and retransmits once a slot is free. Each uncertain result queries and verifies durable receipts before any
identical replay. Parallel progress sums
confirmed bytes and current per-part progress without double-counting. Folder selection/drop retains all original paths, parents and explicit empty
folders; existing folders merge, and file overwrite uses a confirmed observed contentRevision. Folder rows
aggregate descendant progress/failure/cancellation. Failed siblings do not block unrelated queue roots.

The browser queue exists only in memory for the current Files page/document. Folder navigation preserves
selected File objects, task/session IDs, original idempotency keys, accepted create intent and Operation waits
within that page. Refreshing or closing the document discards the queue; the next page starts empty, without
restoring failed tasks or asking for File reselection. The queue does not persist upload tasks in localStorage.
On initialization, remove the current owner's legacy `eam.uploads.v1:<owner>` record without restoring or
executing its contents; leave other owners' records untouched.

Register the browser-native `beforeunload` warning while queued, waiting, uploading or finalizing tasks remain,
and remove it when no such tasks remain. Leaving Files for another application route requires confirmation;
navigation within Files keeps the queue and does not prompt. It is a best-effort browser warning, not a cleanup guarantee; do not
rely on an unload-time abort API. Open server sessions retain their normal expiry/cleanup, durable backend
recovery remains active, and already accepted server Operations continue independently of the browser queue.

Within the same page, a lost create ACK replays the same key; a lost part ACK queries receipts; a lost commit
ACK queries the original session/Operation. Committing and outcome_unknown never trigger a new publication.
Every acknowledged receipt is compared with the retained File's part hash before skipping it. Verification is
cached only for the same File object and receipt identity, then released on completion/cancellation. Only
clients that retain session identity and source bytes use the API's resumable recovery across reconnects or
process restarts; that capability does not imply browser queue restoration after reload. Local roots advertise
sourceModifiedAt support; browser timestamps are sent only when explicitly supported.

Official sessions declare cos-signed transport: the shared client lazily computes per-part SHA-256/MD5,
authorizes an exact short-lived request, confirms any earlier native result, sends bytes through the same-origin
EO transfer path and obtains a storage-verified confirm receipt. The COS XML/empty upload reply is not a public
receipt. No application Authorization/CSRF header is attached to the byte PUT; EO strips ambient browser cookies
and provider operation-selector headers. Signed receipt checksumSource identifies client SHA-256 plus
COS-verified MD5, not server-computed SHA-256. Embedded transport retains its existing server hash boundary.

Cancellation aborts the current byte request, then reconciles/aborts the exact server session. Lost allocation
ACKs are recovered before cancellation. Committing work uses Operation cancellation. Already-committed races
retrieve and report the saved result; they cannot hide or delete published content. Cancellation identity is
retained only within the current page. Only authoritative failed/aborted/expired results allow an explicit retry
to allocate a fresh key; a transport error, missing result or unknown effect does not. State callbacks and
polling are fenced after unmount, and request deadlines/abort controllers are cleared.

Before the first upload allocation, each browser task must acquire an ephemeral exclusive Web Lock scoped to
its owner and normalized target path. Hold it while the retained File/task is alive, including failed retryable
tasks, and release it on completion, confirmed cancellation or Files page unmount. Document destruction releases it
through the browser; no heartbeat or persisted upload queue is involved. A new task that cannot acquire the
lock fails inline and remains retryable: "This file is being uploaded in another tab. Try again after it finishes or closes."
There is no popup, and the task never aborts the lock holder's session. If Web Locks are unavailable or ownership
acquisition fails, fail inline and retryably without allocation or automatic abort; there is no unlocked fallback.

An open server session may still guard its target after the prior browser document closes or refreshes. Once
the new selection acquires ownership, a create response of `operation_in_progress` triggers inspection of only
the exact blocking upload, verifying its session/Operation identity and normalized target path. An `open`
session is automatically aborted and allocation retried without a restart confirmation. Never automatically
abort `committing`, `outcome_unknown` or `committed` sessions. Retrying allocation after an authoritative
`aborted`, `expired` or `failed` session additionally requires its exact Operation to be `cancelled`, proving
the guard settled; a failed Operation or unknown cleanup outcome is not sufficient. An unavailable session
status leaves the new task failed and retryable, rather than waiting indefinitely on an unclassified blocker.
Only a definitive non-upload identity falls back to generic Operation waiting.

Web Locks coordinate cooperating tabs in the same browser at the same origin. They are not server authority
and do not prove inactivity of external API clients, other origins or other devices. Existing server guards,
abort rules, exact identities and lost-ACK recovery remain authoritative; no API fields or backend changes are
required for this browser coordination.

Deletion and other admission blockers carry an exact operationId. Independently of upload queue state, the
browser may persist owner-scoped pending delete identities in localStorage. Within the current page it keeps
selected Files waiting before any new bytes at those paths. Queued/running/unknown/failed/cancelled, wrong
identity and unreachable status never mean success. Only that Operation's succeeded result permits retry;
new admission may identify a further blocker. Retained delete identities remain available after refresh for
status reconciliation, without restoring the upload tasks or their selected Files.

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

Web validation must exercise the actual transformed queue/client source. Required coverage includes same-page
lost create/part/commit ACKs, idempotency, immutable receipt/hash verification, owner isolation, bounded fairness,
folder merge/dependencies, exact deletion waits, timeout/unmount, partial failures, terminal retry and
cancellation races. Lifecycle checks must prove queue preservation during folder navigation, an empty queue
after reload without failed-task restoration or File reselection, current-owner legacy upload-record removal
without execution, and independent pending-delete identity retention. Ownership checks must cover acquisition
before allocation, retention through retryable failures, release on completion/cancellation/unmount/document
destruction, same-owner/path tab conflicts without a popup or holder abort, and inline retryable failure when
ownership is unavailable or acquisition fails. Verify automatic exact-open-session recovery after refresh/close
without restart confirmation, terminal-session/Operation proof, lost abort ACK recovery, and no automatic abort
of `committing`, `outcome_unknown` or `committed` work. Verify native beforeunload registration
for queued/waiting/uploading/finalizing tasks and removal when none remain; browser warning display is subject
to browser policy. Path checks cover absolute resources and literal encoded-looking names. Build the production
SPA as part of validation.

The opt-in browser test harness starts a real HTTP service using the built SPA, random disposable PostgreSQL
database, explicit admitted Local roots and genuine Cookie/scoped Bearer credentials. Chromium uses the actual
file picker and network. Browser acceptance must cover special filenames, publication/result settlement,
preview/download, folder navigation with an active queue, unload protection, an empty queue after a confirmed
refresh, pre-commit cancellation, partial failure isolation, exact deletion waits, scoped Bearer isolation and
same-origin traffic. Retain backend receipt/restart/recovery checks separately from browser queue lifetime.
Browser fixtures deliberately interrupt requests; no production bucket, real user root, .env or credentials
are read.

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
