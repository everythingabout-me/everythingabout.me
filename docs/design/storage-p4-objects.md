# P4 object-store mutation admission

The remaining P4 object-store code adds a conservative COS/S3 strategy over the existing same-origin
API and finite-manifest workflows. Native final files still use exact owner/path keys and standard
empty-folder markers. Clients receive neither provider credentials nor provider URLs.

**No live COS/S3 capability result is established by this change.** Dedicated provider test variables
were absent. SDK/native fixtures and disposable Postgres test the implementation, not the real
endpoint's guarantees. Writes default off; an explicit operator-owned, exact-connection admission
report is required before the new adapter can be constructed. Local P4 behavior is unchanged.

## Required evidence and configuration

Use an isolated root for positive and negative native probes first. There is one strategy. It requires
exact-connection atomic complete-object publication, native absence conditions (`If-None-Match: *`),
native ETag conditions (`If-Match`) on GET/DELETE and single-attempt response evidence. Live COS
probing (2026-11-03) showed GET and DELETE return 412 on an ETag mismatch, but **PUT ignores `If-Match`**. **Bucket
versioning is neither required nor consulted**: every revision token is the current object's ETag.
Do not infer capabilities from S3 compatibility, accepted headers, size/mtime, or a passing mocked test.

```dotenv
FILE_COORDINATED_WRITES=true
FILE_OBJECT_ADMISSION_FILE=/secure/storage-capabilities.json
FILE_OBJECT_SPOOL_ROOT=/srv/everything/object-spool
FILE_MAX_SINGLE_PUT_BYTES=33554432
```

The spool root must already exist on an admitted local filesystem (the same known-local classes as P3),
be durable across process/gateway restart and database restore, and belong to the operator. Each binding
uses its own private hashed namespace. **Only one gateway or processes sharing the exact same durable
spool/lock namespace may serve that connection.** Identical path strings on different machines are not
shared fencing. A DB lease cannot fence provider requests; do not run this mode as an uncoordinated fleet.

The strict JSON report uses actual connection values and measured probe references:

```json
{
  "connectionId": "default",
  "endpoint": "https://verified-bucket-1234567890.cos.ap-guangzhou.myqcloud.com",
  "bucket": "verified-bucket-1234567890",
  "region": "ap-guangzhou",
  "checkedAt": "2026-11-03T00:00:00Z",
  "probeReference": "/secure/release-evidence/provider-probes.json",
  "atomicPublish": true,
  "conditionalCreate": true,
  "singleGateway": true,
  "multipartUploads": true,
  "signedMultipartParts": false
}
```

`multipartUploads` requires evidence for native unpublished parts and absence enforcement on Complete,
independent of ordinary PUT evidence. No bucket setting or existing object rewrite is required. Upload,
replacement, mkdir, move, copy and delete are all available. Implicit prefix folders (no marker object) are
real native folders: they carry no revision, children may be created below them, and they disappear with
their last child. Reads are validated against the current ETag but never pinned to a retained version, so
`pinnedRead` is false and `sourceModifiedAt` is unsupported.

What ETag revisions do not give: a read may observe a concurrent replacement (detected by size/ETag checks
and reported as `changed`, never silently mixed). Copy and delete are fenced natively by `If-Match`.
Replacement re-checks the current ETag under the gateway's per-intent lock immediately before one PUT; because
COS does not enforce `If-Match` on PUT, an external tool writing the same key inside that short window can be
overwritten. Application writers cannot race it (single gateway), so this only matters for external tools.

The report is trusted deployment evidence, not a request to automatically claim those capabilities.
When admitted COS object writes are enabled, legacy grant/native-action routes are gated before handlers,
legacy browser upload/job/index/timeline writer services are not started, and the legacy storage bridge is
read-only. This stops the same gateway from issuing new effects that bypass the durable guards. In full
mutation mode, previously issued grants and already accepted remote requests still require independent
drain/quiescence evidence.
Startup matches connection ID, endpoint, bucket and region exactly; COS endpoint is its configured
`https://<bucket>.cos.<region>.myqcloud.com`. Root configuration/generation and owner remain bound in
the actual driver identity. Secret rotation is separate from root identity. The file is bounded to 1 MiB,
unknown fields and extra JSON values are rejected. Never store secrets in the report or commit it.

After draining all legacy/native writers and importing the current native
index, initialize each owner:

```bash
cd backend
go run ./cmd/eam storage-admit --owner-id 00000000-0000-0000-0000-000000000001 \
  --writers-quiesced --evidence /secure/baseline-proof.json \
  --physical-bytes 123456789 --dry-run
# Review, then repeat without --dry-run before serving writes.
```

A brand-new owner may instead be admitted automatically on first write (or sign-up) when all of these hold:
the owner has no index, operation, upload, reservation, charge or generation row; a complete scan of the
content root is empty; the binding spool is empty; the exact connection report declares `singleGateway`; and
the driver reports no retained generations (the admitted object write model already assumes none). Anything
else stays on the command above.

Object baseline evidence must contain a nonempty `retainedVersionInventory` reference. The explicit
physical byte amount must include any retained object history and be at least current logical bytes.
Native/index comparison checks exact current names, kinds, size and parents. Read-only HEAD checks require
an ETag on every file and folder marker; prefix-only folders are accepted without a marker.
The exact binding's spool must be empty before and after verification. Dry-run sends only provider reads,
creates no spool state and writes no budget. Existing budgets are not reset. Local admission still measures
its physical bytes and rejects this object-only override.

## Remote-effect protocol

Ingress is fully verified into a durable private gateway file before final native publication. Exact length
and SHA-256 are enforced, including zero-byte input and replay identity. Temporary identities/checkpoints
are durable before allocation. Data/intent files and directories are fsynced, and permanent per-intent flock
files and terminal tombstones prevent concurrent/restarted callers from dispatching the same intent again.
The spool is recovery evidence and temporary bytes, not authoritative committed user data.

A final operation writes durable `publishing` evidence before issuing exactly one SDK mutation request.
The write clients are separate from read/legacy clients, disable SDK retries and redirects, and suppress
replayable request-body factories. No high-level uploader, CompleteWithHead fallback or helper retry loop
is used. Conditional create uses native absence headers. Replacement checks the captured native version
and parent, then sends one native condition/atomic publication under the declared coordinated writer model;
`NativeConditionalMutation` does not falsely advertise general provider CAS.

A successful sole native response is recorded durably before application settlement. Only that saved terminal
response can prove Applied automatically. A timeout, connection loss, truncated response, 5xx, generic 409,
process death after dispatch, or uncertain response remains `outcome_unknown` across restart. **HEAD showing
the desired file or absence does not prove old requests terminal.** No polling, lease expiry, elapsed timeout,
matching checksum or current object version releases that guard. A definite completed HTTP 412 is a
precondition rejection. COS HTTP 304 is also a precondition rejection for absence-conditioned PUT or multipart
Complete requests that sent `If-None-Match: *`; a 304 for replacement or deletion remains unknown.
Other errors do not become a safe rollback by guesswork.

Retained receipts are monotonic evidence. If a database checkpoint says a dispatch started but the spool
record is missing or restored to an older ready phase, recovery must remain unknown rather than use the
older state to retry/discard. Permanent unknown effects require explicit operator/provider quiescence
resolution; this release does not automatically resolve them from object inspection. Preserve spool and
journal backups together. Do not delete a spool/lock directory to unblock a path.

For confirmed success, read-only metadata refresh is tied to the acknowledged ETag.
Refresh cannot attribute an earlier ambiguous write. Public checksum is null when HEAD/read metadata cannot prove a
verified digest, even though ingress replay still uses the durable server hash. This keeps the settled
resource and later native-authoritative GET consistent without trusting arbitrary user metadata or ETags.
`X-Source-Modified-At` is rejected for this initial object strategy: providers generally cannot set native
Last-Modified, and the adapter does not silently pretend that it did.

## Namespace and version restrictions

Files and explicit standard folder markers are identified by their current ETag. ETags stay private
inside revision tokens; the wrapper provides the P4 bound native tree and verifies `If-Match` content reads.
The root is the configured owner binding, never an arbitrary provider prefix inferred to belong to a new owner.

Mkdir creates a real zero-byte marker. Copy reads the captured source ETag (`If-Match`) into verified gateway
staging and writes an independent ordinary target object; it is bounded by the admitted single-publication
limit. Folder copy/move use saved finite manifests. Native Move optimization is unsupported; the service
copies/verifies all captured targets before captured unchanged source removals. Existing implicit folders
are never silently assigned marker identities or created as a side effect of metadata admission.

Delete removes the **current key** with `If-Match` on the captured ETag. If the bucket happens to have
versioning enabled the provider keeps history on its own; the application never relies on it. Empty-folder removal checks
captured marker identity and immediate children. All remote mutation uncertainty uses the same permanent
guard rules. Discard removes only exact recorded gateway staging files, never authoritative provider keys
or their history. Unknown allocations are retained for reviewed reconciliation.

## Physical budgets and remaining gates

Object roots persist `RetainsVersions=false` in their immutable budget identity: replacement and deletion
credit the old logical and physical size. Provider-side history (if the operator enabled versioning) is
outside application accounting. Copy/delete move ends at zero net logical growth.
Gateway spool plus final object reserve two times admitted new bytes. Known partial charges consume only
their proven portion; unknown liability remains reserved, and terminal spool cleanup releases staging only.
The adapter never invents provider-history garbage collection or releases bytes because an index row vanished.

SDK fixtures cover actual S3/COS authentication, exact key encoding, conditions/metadata, If-Match
reads, one-attempt error/timeout behavior and refused redirects. Engine tests cover durable dispatch, response
loss, late calls, permanent unknown guards, missing/restored evidence, terminal response recovery, cancellation
and temporary-only cleanup. Real S3 SDK HTTP + disposable Postgres tests exercise the unified API, retained
history accounting, finite copy/move/delete, current-key markers and unknown results across new server/factory
instances. Baseline CLI tests use provider reads only and repeat the full migration set.

Progress: P0–P3 implemented; P4 Local and conservatively admitted COS/S3 code are implemented with explicit
limits. **Real COS/S3 positive/negative probes and deployment admission remain unverified.** P5 resumable
ingress/native multipart, P6 Web/media/old-grant cutover and P7 production restore/performance acceptance
remain. A simple SDK fixture PASS is not completion of live provider or release acceptance.
