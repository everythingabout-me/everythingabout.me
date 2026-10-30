# P0 native storage capability evidence

These are isolated experiments for [P0](storage-implementation-plan.md#p0--freeze-boundary-cases-and-prove-native-safety),
not runtime drivers or permission to activate production writes. The [driver contract](storage-driver.md) still
requires durable intent/effect recovery and safe admission. No runtime, application configuration, entrypoint,
Makefile, customer data or bucket settings are changed by this package.

## Evidence recorded on 2026-10-30

`go test -count=1 -v ./internal/storageprobe` passed on Darwin arm64. Local probes use `t.TempDir()`; default COS
SDK probes use an HTTP test server or in-process transport. `TestCOSNativeCapabilities` explicitly **skipped**
because dedicated opt-in configuration was absent. No live COS experiment was performed, and no `.env` was
read. **Unverified** means no live evidence, not provider unsupported. **Unsupported** below is reserved for
an operation demonstrated unable to provide the stated guarantee under its tested writer model.

Local observations apply to this run's temporary filesystem, not every Darwin mount or all filesystems:

- Twelve native `link` contenders produced exactly one no-replace publication; a later occupied-destination
  attempt returned existence conflict without changing bytes.
- Same-directory `rename` replacements exposed complete old/new 4 KiB fixtures during concurrent reads.
  This sampled visibility plus native primitive evidence does not prove power-loss durability.
- Ordinary filesystem enumeration retained ` Spelling +#?% 中文.txt `, a nested empty folder and a zero-byte
  file. Exact bounded ranges succeeded; EOF and truncated ranges produced the expected distinct errors.
- Ordinary copied bytes survived in-place source editing and source removal; no mutable hardlink alias was
  used for copying. All inspection/readback used standard filesystem APIs without Postgres or a manifest.
- `Case`/`case` and NFC `café`/NFD `café` collided on the tested filesystem. Enumeration retained `Case` and
  NFC `café` in this run. Names/case/normalization must be checked for the actual configured root; accepting
  two logical names for one native file would violate the contract.
- An open descriptor retained the old inode after pathname replacement, but an in-place edit through that
  descriptor changed the old bytes. A descriptor is not an immutable version against arbitrary writers.
- A deterministic paused older `rename` replaced a newer generation when resumed. A native rename followed
  by an injected lost reply left the new file present while returning an error. These are negative native/
  fault-injection observations, not a production journal/recovery implementation.

## Capability matrix and admission consequences

| Capability | Local evidence | COS evidence in this task | Consequence / remaining gate |
|---|---|---|---|
| Native readable tree / empty folders | Default native tests pass with exact spelling, nested directory, raw and zero bytes | Live exact-key listing/GET and standard trailing-slash empty-marker tests are runnable but **unverified** | COS markers are objects; a chosen sync tool's empty-folder behavior still needs a direct exit drill |
| Exact ranges | Local section reads verify exact bytes, EOF and short-read negative cases | Live asserts `206`, `Content-Range`, `Content-Length`, bytes and `416`; **unverified** | A successful SDK call alone never establishes exact range support; no runtime range capability is enabled |
| Conditional reads / retained versions | Descriptor survives rename; **unsupported as immutable pinning against in-place external edits** | Live matching/stale `If-Match`, `If-None-Match` and captured `versionId` ranges; **unverified** | ETag is opaque, not necessarily a generation/checksum; retained version reads depend on bucket retention and permissions |
| Atomic no-replace publication | Same-filesystem native `link` race/occupied destination pass | Per-operation live `If-None-Match: *` and `x-cos-forbid-overwrite: true` positive/negative probes; **unverified** | Local link is staged-inode publication with later stage unlink, not an independent copy primitive. COS support cannot be inferred from S3 |
| Atomic replacement visibility | Same-filesystem native `rename` observation passes | Small live PUT/Copy/Complete readback is runnable; atomic concurrent visibility still **unverified** | Single-file atomic visibility is distinct from version CAS, crash durability and multi-file transactions |
| Destination version condition | Unconditional rename **does not fence stale generations**; Stat+rename is insufficient | Live matching and captured-stale destination `If-Match` for PUT/Copy/Complete/Delete; **unverified** | Each native operation needs separate admission. Do not borrow GET or copy-source condition support for target mutation |
| Copy source condition | No conditional unlink/source-version primitive implemented | Distinct live `x-cos-copy-source-If-Match` test; **unverified** | Source condition says nothing about destination absence/version or later conditional source removal |
| Independent copy | Ordinary byte-copy edit/delete assertions pass | Live server-side Copy source edit/delete/readback test; **unverified** | No application alias or mutable hardlink representation is acceptable |
| Root confinement | Native `os.Root` positive and symlink/dot-dot rejection; plain joined open demonstrably escapes | Generated reserved-prefix isolation only; production owner/root isolation **unverified** | Static local probe is not a complete symlink-swap/mount-race or mutation adapter audit |
| Delayed stale native effect | Actual paused rename overwrites a newer path: **unsafe unfenced** | SDK transport model reproduces stale seekable PUT replay; no live provider delay experiment | Leases, application fences and client timeout cannot authorize handing an unresolved path to a new mutation |
| Lost replies / recovery | Applied rename with injected error is observable | SDK Complete lost-reply/NoSuchUpload fixture and HTTP timeout followed by modeled late Delete effect; live recovery **unverified** | Error/cancel/404/one successful HEAD cannot prove rollback or quiescence. Persist unknown outcome and guard conflicts |
| Cleanup | `t.TempDir` owns only disposable fixtures | Exact generated key/version and returned upload-ID cleanup code; live cleanup **unverified** | No broad prefix delete, no scan-based user-data GC. Lost allocation ID and delayed effects remain explicit uncertainties |

A test PASS can contain a logged **unsupported** native condition. These integration tests are discovery
probes: successful negative requests are checked for the actual resulting effect and reported as ignored/
unsupported; rejections are checked for unchanged native bytes. HTTP `400`/`501` are reported as unsupported
requests, `412` as observed precondition rejection, and `409` as conflict evidence without assuming which
condition caused it. Other errors fail as infrastructure/unknown outcomes. Match/absent positives distinguish
an enforced condition from a header rejected on every request. Read conditions assert their exact required
status and bytes. These observations do not automatically set a runtime descriptor or establish provider-wide
concurrency guarantees, monotonic generations, ABA protection, or safe cutover.

## SDK retry, timeout and recovery findings

The dependency is `github.com/tencentyun/cos-go-sdk-v5 v0.7.76` (pinned in
[the module file](../../backend/go.mod)). Sources inspected in the Go module cache:
`cos.go` (`NewClient`, `CheckRetrieable`, `doRetry`, `doAPI`), `object.go` (`Put`, `Copy`, `Delete`),
`object_part.go` (`UploadPart`, `completeMultipartUpload`, `CompleteMultipartUploadWithHead`) and `auth.go`.
The default fixture tests assert the important observed behavior so a dependency change requires review.

- SDK `RetryOpt.Count=3` means **three total SDK attempts**, not three retries; interval is zero and automatic
  host switching is false. Generic `doRetry` retries transport errors and HTTP >=500 errors; ordinary errors
  with a response below 500 are terminal. Reader bodies bypass that generic loop.
- `Put` and `UploadPart` have separate replay loops: a successfully seekable input can receive three attempts;
  an input without `io.Seeker` receives one. PUT's loop does not apply `RetryOpt.Interval`. The current runtime
  wraps ordinary PUT input in a non-seekable `sizedReader`, but that is not a general no-retry guarantee for
  other operations. A fixture returns a lost reply after effect, inserts a newer writer, then observes a
  seekable stale replay replacing it. This is SDK/model evidence, not a live COS provider result.
- `Copy` and `Delete` can use generic retries because their request bodies are nil. Default wire fixtures show
  HTTP 412 is terminal for GET/PUT/Copy/Delete. Completion uses its own loop and retries **even HTTP 412 and
  NoSuchUpload**, up to Count. A success HTTP status can carry a completion/copy error in its XML body;
  SDK operation results/errors, not status alone, must be checked.
- A Complete fixture applies the first attempt, loses its reply, and returns NoSuchUpload on subsequent
  attempts. The SDK returns an error while the modeled native object is already completed. This is not a safe
  rollback/abort signal. `CompleteMultipartUploadWithHead` is also not an intent-attribution primitive:
  the default nil CRC fallback can turn HEAD of an unrelated pre-existing object into reported success.
  Even matching CRC proves neither intent identity nor absence of an older delayed final-path request.
- Current runtime `cos.New` sets `http.Client.Timeout=60s`, retains the SDK defaults, and uses
  `AuthorizationTransport` without a custom transport, hence the standard Go HTTP transport for its public
  COS endpoint. That transport uses a 30s dial timeout, 10s TLS handshake timeout, 1s expect-continue timeout,
  90s idle timeout and no response-header timeout. The client timeout includes the response-body read.
  A generic three-attempt call can spend approximately 3 × 60s in HTTP attempts without a tighter context;
  this is a client-side limit, not a bound on provider effects or proof of quiescence. Helper methods may add
  loops (for example SDK `PutFromFile` has three outer attempts); these probes do not validate every SDK helper.
- Live probes deliberately use **Count=1**, no proxy/redirect following, dial 3s, TLS 5s, response-header 10s,
  HTTP client total 15s, run context 3m, and a separate cleanup context 45s. The HTTP fixture sets 100ms and
  demonstrates an accepted server effect can occur after the call times out and after a newer generation is
  installed. Its delayed server action is a model, not proof that COS behaves identically. No timeout here
  proves cancellation of a COS effect. Standard transports can have their own restricted connection retries;
  Count=1 only limits the SDK loop, not provider-side behavior or all possible network replays.

No implemented journal can yet establish terminal native outcomes. Keep persistent unknown-outcome guards
across restart/lease loss; an apparently correct current HEAD is insufficient if an older request may arrive.
Conditional effect support, a declared coordinated writer model, safe publication strategy, restricted/read-only
admission or explicit operator recovery must be selected before write activation. Old signed single PUTs,
STS/native sessions and in-flight legacy writes must be drained/revoked and reconciled separately.

## Running isolated probes

From the worktree's `backend` directory (no application environment is needed):

```bash
go test -count=1 -v ./internal/storageprobe
go test -race -count=1 ./internal/storageprobe
```

Live COS is opt-in, uses only `EAM_PROBE_COS_*`, and never imports the application config loader. Merely
having regular `COS_*` variables or a live `.env` cannot activate it. Missing enablement explicitly skips;
enabling with incomplete/unsafe dedicated settings fails before network access. Use a **disposable dedicated
bucket/credentials**, with List/Get/Head/Put/Copy/Delete, multipart and read-versioning permissions; a versioned
or suspended bucket also needs list/delete versions. The probes read versioning but never modify bucket
versioning/lifecycle. No arbitrary endpoint override is accepted. Explicitly reserve this non-drive root:

```bash
# Supply dedicated credentials securely in this shell; never source a live .env.
export EAM_PROBE_COS_ENABLE=1
export EAM_PROBE_COS_BUCKET='dedicated-probe-bucket-1234567890'
export EAM_PROBE_COS_REGION='ap-guangzhou'
export EAM_PROBE_COS_ROOT='__eam_storage_probe__/'
# EAM_PROBE_COS_SECRET_ID / EAM_PROBE_COS_SECRET_KEY must be dedicated credentials.
# EAM_PROBE_COS_SESSION_TOKEN is optional for dedicated temporary credentials.
go test -count=1 -v -timeout=5m -run '^TestCOSNativeCapabilities$' ./internal/storageprobe
```

Each run uses a fresh cryptographic random 128-bit subprefix below `__eam_storage_probe__/`; `drive/` and all
other roots are rejected. Fixtures are zero/small objects; multipart uses one 1 MiB part. Known generated keys
are recorded **before** effects; returned upload IDs are recorded immediately. Cleanup aborts only captured
upload IDs, removes only exact generated keys (or exact-key listed version/delete-marker IDs on a versioned
bucket), then checks HEAD absence. Prefix listing is never broad deletion authority. Cleanup failures fail the
run and include the isolated prefix for recovery. An initiation lost before returning its upload ID can leave
an untracked allocation; do not claim no leak or guess-delete other uploads. Dedicated bucket lifecycle or
explicit operator inspection must resolve it. A delayed provider effect can also outlive cleanup/HEAD absence;
no fixture destroys the evidence needed to recognize that uncertainty.

The local root test additionally demonstrates why ordinary filepath joining is insufficient: a symlink from
one disposable root to another is readable with a plain open, while `os.Root` rejects escaping opens and
accepts a file inside its own root. It does not establish the safety of a future mutation implementation.

## Remaining release gates

The aggregate [P0 runbook](storage-p0.md) documents the separate isolated baseline command and its scope.
This P0 probe package does not snapshot existing native/index counts, establish quota/reservation baselines, back up
owner-root mappings, implement runtime drivers, or perform a production direct-exit/cutover drill. No live COS
conditional, version, cleanup, delayed-effect or atomicity claim is established by the default tests. Power-loss
fsync ordering, cross-volume moves, source-version deletion, arbitrary external writers, complete root-race
confinement and a durable recovery/unknown-outcome implementation remain separate gates. Never label
Stat-then-write, existence+size, lease expiry, or a clean test run as safe write cutover.
