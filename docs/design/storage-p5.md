# P5: durable numbered-part upload sessions

> The original P5 byte path and cost model below are historical for object uploads. The current
> [native upload implementation and deployment](native-upload-deployment.md) receives COS/S3 parts into
> unpublished native multipart state during upload, with shared Postgres metadata and no full-file
> assembly/PUT at commit. Local retains durable local staging. Native multipart requires explicit connection
> evidence; its numbered-upload limit is independent of the small single-PUT limit. P6 now uses four file
> slots with up to sixteen concurrent parts per file. The public session/receipt/Operation contract remains.
> Official COS now uses same-origin signed part requests through ordinary EdgeOne forwarding, not an extra
> upload server or edge function. Authorize/confirm supplement the embedded part PUT; signed receipt provenance
> distinguishes client SHA-256 assertions from COS-verified MD5. See the current deployment document above.

P5 implements the shared `/api/files/uploads` lifecycle on explicitly admitted Local/COS/S3 roots.
The existing browser sign/confirm receipt is a separate legacy protocol; switching Web, media and generated
instructions is P6. Native publication still uses P3/P4. Real COS/S3 capability probes, production baselines
and old native authority drain remain unverified deployment gates. No production data or credentials were used.

## Active contract and admission limits

| Method | Resource | Result |
|---|---|---|
| POST | `/api/files/uploads` | Persisted session and quota/guard admission; required Idempotency-Key |
| GET | `/api/files/uploads` | Principal/root/scoped session keyset pages |
| GET | `/api/files/uploads/{id}` | Compact durable counters, state and saved result |
| PUT | `/api/files/uploads/{id}/parts/{number}` | 201 durable SHA-256 receipt, 200 identical replay |
| GET | `/api/files/uploads/{id}/parts` | ACK-order pages with fixed watermark |
| POST | `/api/files/uploads/{id}/commit` | Stable 202 Operation, or 200 saved settled metadata |
| DELETE | `/api/files/uploads/{id}` | Atomic open abort; repeat 204; committing/committed 409 |

All endpoints share the existing Cookie/Bearer boundary, approved cookie Origin and session-bound CSRF.
Explicit Authorization never falls back to cookies. Owner/root/configuration and creator principal are bound
in the journal; owner management may recover retained workflows, another app token may not. Paths never follow
renames. Parent native identity and target conditions are captured at admission and rechecked before publication.
An exact target guard prevents coordinated target ABA and conflicts with ancestor subtree mutations. Other
clients/native tools remain subject to the declared coordinated writer model; database guards do not fence them.
A conflicting same-path session returns operation_in_progress without cancelling the earlier session.

Geometry is fixed at 8 MiB, 1-based part numbers, at most 10000 parts and sixteen parallel byte requests per session.
The last part has the exact remainder. A zero-byte session has no parts and can commit immediately. Whole-file
checksum input is unsupported and rejected; optional part Content-Digest is verified and every part is server
hashed. SHA-256 receipts are lowercase hexadecimal. Sessions/list/status never contain an unbounded part array.
Receipt cursors encrypt/authenticate the ACK keyset, watermark, session, root and principal/scope binding.

Local uploads are limited to 80000 MiB and available logical/physical budgets. COS/S3 use the existing admitted
full-gateway-spool plus one native PUT strategy. Their maximum is the smaller of configured publication policy
(default 32 MiB) and 5 GiB. Oversize session creation returns 413 before bytes/admission. **Native multipart
publication is not implemented or claimed.** Object sourceModifiedAt remains explicitly unsupported. The small
single-PUT content endpoint remains available with its existing deployment limit.

## Persistence and byte identity

Additive, re-runnable migration 024 creates owner-scoped sessions and receipts; bytes never enter Postgres.
Session admission and the P3 operation/guard/logical/physical reservation are one owner-locked transaction.
Client idempotency fingerprints exclude captured native evidence and live in a separate session key namespace
from simple PUT. Replays return the original identity and recheck current caller rights.

Private local `uploadspool` namespaces bind owner, connection, generation and both native roots. Local chunks
live below the admitted owner's system root; object chunks use the admitted durable object spool base. These
bytes are temporary recovery material, never authoritative native file contents. Base directories must exist,
be owned by the service user and not writable by group/other. Children use 0700 and files 0600. Known-local
filesystem admission matches the staged writer. The object strategy still requires one shared durable spool
and OS-lock namespace across all processes serving that connection.

Each attempt uses a persistent cross-process slot lock and isolated temporary bytes. Files, immutable byte
identity and directories are fsynced before the database receipt callback. One retained identity per part prevents
concurrent, late or ambiguous attempts from replacing acknowledged bytes or accumulating unlimited candidates.
All replays consume and hash the body, including requests without a digest. Different bytes conflict. Generic
receipt persistence errors preserve canonical bytes because COMMIT may have succeeded; only definitive rejected
receipts may remove a newly created candidate. Crash leftovers are reclaimed under their original slot lock.
Symlinks and unsafe entries fail closed. Lock files and terminal tombstones remain to prevent split-lock reuse.

Idle expiry is 24 hours, extended only by new durable progress, capped at seven days. Identical part retries do
not extend it. Status/replay/list can atomically expire a due open session, in addition to the bounded sweeper.
Abort/expiry serialize against receipt acceptance and freeze. Late body completion acquires no receipt. Logical
reservation/guards may be released because open sessions cannot claim a native publication lease; physical
reservation persists until actual cleanup. Terminal identities/results remain at least 24 hours; this version
retains them longer rather than prematurely deleting recovery evidence.

## Commit, recovery and physical accounting

Commit transactionally verifies every unique part, exact geometry and full size, freezes the manifest and marks
committing. The ordinary Operation worker loads bounded receipt pages, orders the finite manifest and streams
verified chunks through the existing Stage/Publish service. The reader verifies each chunk to EOF; corrupt/missing
bytes cannot publish. Native authorization and captured parent/target conditions are rechecked. Existing P3/P4
native-success/index-failure repair, result persistence, logical accounting, retained-version PhysicalDelta and
unknown-outcome guards remain authoritative. There is no second publication implementation.

Open sessions are excluded from worker Pending and cannot Claim. Committing sessions have worker recovery rather
than idle expiry. Restart resumes safe staging/inspection; an uncertain native request is never dispatched again.
An unknown object result stays outcome_unknown even if HEAD shows the file. Repeated commit returns the same
Operation or saved metadata. DELETE of committing work returns commit_in_progress; Operation cancellation stops
further safe steps and does not report a definite abort while native publication is uncertain.

Physical reservation adds durable chunks and four times the maximum actual part length to P4's staging/final
cost. With size S and part size P this is `2*S + 4*min(S,P)` for Local and `3*S + 4*min(S,P)` for version-retaining
objects. Local staged publication reuses its inode; retained object generations are charged in full. Physical
usage is never reduced by a logical replacement/delete on version-retaining roots. The wrapped temporary Discard
must fence/clean chunks and discard the P3/P4 stage before MarkCleaned releases retained physical reservation.
Active receive/read/callback locks make Discard busy and leave the liability for retry. Unknown files and native
content are never removed based on absent index rows. Known terminal cleanup runs through existing bounded workers.
Stage releases its assembled reader before failure cleanup. A crash after chunk tombstoning resumes native
inspection instead of trying to reopen fenced chunks. Cleanup selection persists its last scheduling attempt
and rotates past busy candidates, so stalled late bodies cannot monopolize another owner's cleanup queue.

## Validation and remaining work

Dedicated disposable PostgreSQL 17 applies all 24 migrations twice. Tests cover atomic quota admission, principal/
owner/root/scope isolation, canonical create replay, zero bytes, exact geometry and checksums, dropped ACK replay,
same/different concurrent part attempts, watermarked pagination, server reconstruction, abort during body ingress,
expiry before the sweeper, cancellation, parent replacement/ABA, saved results and permanent object uncertainty.
Spool tests include real subprocess concurrency, restart attempt recovery, corrupt/missing/symlink chunks,
cancellation, ambiguous receipt persistence and cleanup preserving unrecognized files. Object tests use the real
SDK against a disposable HTTP fixture, not a real provider acceptance claim.

```bash
EAM_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:PORT/postgres?sslmode=disable make test
cd backend
EAM_TEST_DATABASE_URL=postgres://postgres@127.0.0.1:PORT/postgres?sslmode=disable \
  go test -race ./internal/api ./internal/fileops ./internal/uploadspool ./internal/localstorage ./internal/objectmutation
```

P0–P4 code foundations and P5 resumable ingress are implemented. Real provider proofs, production baseline and
old-authority drain remain deployment gates. P6 Web/media/prompts/legacy-route cutover and P7 fresh install,
restore/native-exit/performance/release acceptance remain separate work. Removing old routes does not revoke
already issued single PUT URLs, STS or multipart authority.
