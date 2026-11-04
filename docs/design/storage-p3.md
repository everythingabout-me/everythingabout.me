# P3: durable native mutation coordination

P3 implements the internal mutation foundation from the [implementation plan](storage-implementation-plan.md).
It does not enable the P4/P5 HTTP write routes: the unified file API continues to advertise writes, uploads
and thumbnails unavailable. Existing COS legacy writers are not silently moved under the new guards.
Native capability validation, old-grant draining and a verified owner/root baseline remain admission gates.

## Durable boundary

The new `fileops` service separates Postgres intent/accounting from native publication. The application
journals an immutable owner/principal/root-bound operation, guards the captured paths, reserves liability,
and only then receives staged bytes or starts a native effect. Public file identity remains the path.
Database keys, provider locators, temporary handles and per-entry native evidence are private.

The additive migrations are `016_file_operations.sql` (operations, persistent guards and checkpoint history)
and `017_file_budgets.sql` (explicit baselines, reservations, captured replacement credit and partial charges).
They use `create table/index if not exists`; all 18 migrations are exercised twice on disposable Postgres.
No migration writes file bytes, rewrites native keys, derives usage from an index SUM, or changes old
upload/job rows. Old sessions are not converted into new durable receipts by assumption; P5 will create
the public resumable-session/receipt lifecycle on this foundation.

Each operation captures owner UUID, stable creator principal, scope-checked guard paths, connection ID,
configuration generation, content/system roots, immutable intent and finite manifest. Requests with the same
owner/principal/action/idempotency key return the same saved identity/result. A different canonical intent
conflicts. JSON object key order is canonicalized; results remain byte-stable across JSONB round trips.
Replay rechecks the recovery actor's rights/scope. Another app token sharing an owner does not gain access;
explicit first-party owner management can recover retained results, including deletion provenance after the
original target disappears. Credential authentication/revocation and current ancestry remain the caller's
responsibility before admitting a new native effect.

Owner-level transaction locks serialize admissions and accounting. Guards compare exact path segments with
NFC and the configured case rules, cover exact/subtree/ancestor conflicts, and are not tied to worker lease
expiry. P3 conservatively keeps the owner's budget root binding immutable, including after settlement;
connection/root replacement needs an explicit future migration, not a new generation bypass.

Action guards name the native paths the action observes or mutates: the deletion target (subtree only when
recursive), both endpoints of a move or copy, and the mkdir target. A long-lived guard elsewhere — an open or
abandoned upload session, for instance — therefore cannot turn an unrelated mutation into
`operation_in_progress`, while overlapping paths still conflict. `mkdir` with `parents` is the exception and
keeps the actor-scope fence, because its missing ancestors are discovered only after admission and the frozen
manifest must stay covered.

Worker claims use DB time, random tokens and increasing epochs. Every checkpoint/state update validates
its lease fence. An expired lease may allow inspection by a successor, but never permits a second final
publication while the first is uncertain. Releasing a finished worker lease does not release guards or
reservations. Checkpoint history is append-only; recovery observations are stored separately so a lock-busy
inspection with no new evidence cannot erase a known native allocation identity.

## Staging, publication and repair

`storage.StagedWriter` defines Stage/Publish/Inspect/Discard; `EffectRecovery` also covers finite manifest
mkdir/move/copy/delete effects. Root-bound extensions expose non-secret immutable construction identity.
The service compares the actual writer/recovery owner, connection, generation, roots and naming rules before
any native I/O. A writer accidentally constructed for another owner is rejected even if its relative path
matches. Injected native action and SQL callbacks remain trusted internal dependencies, never user-submitted
tasks; P4 must obtain them from the same admitted root-bound executor.

The single-file coordinator supports a typed `upload_commit` intent (also usable by P4's simple PUT):

1. Read the admitted immutable intent and current authorization; claim a lease and renew it while working.
2. Receive exact bytes into isolated staging. SHA-256 is always recorded for durable replay; a declared
   `sha-256` digest must match. A terminal retry still consumes/validates its body before returning success.
3. Persist the stage; recheck cancellation/current publication authorization. Journal `effect_started` before
   the final native call. A failed final call is inspected rather than declared rolled back.
4. Save proven native success, then update the index, usage and durable response in one Postgres transaction.
   If that transaction fails, the real native file remains; recovery repairs it without another Publish.

The `nodes.CommitStagedFile` callback constructs the private owner/path key mapping, retains existing node
IDs/application metadata, requires known parents, and saves a path resource as the operation result. It does
not create native parents or claim a database reference swap published bytes. P4 will wire mutation
representation/generation anchors into the live HTTP revision model; this internal callback is not a new
strong validator or public write endpoint.

Recovery inspection does not issue a new final effect. Applied native results must be charged/indexed even
when the original creator has been revoked after publication; internal repair and terminal temporary cleanup
therefore do not repeat the publication-authorization callback. Current caller access still goes through the
journal and upstream credential verification. A revoked client cannot use this distinction to publish new
bytes; explicit owner/system recovery is responsible for finishing already-known effects.

`outcome_unknown` remains nonterminal and guarded, including when the expected file is visible but an old
request might still apply. A NotApplied result needs durable native evidence and quiescence/fencing. Receiving
crashes are handled separately from final effects: confirmed unused staging is discarded before pre-effect
termination; uncertain temporary inspection retains the receiving phase, guard and reservations for retry.
Cancellation before publication can win; after publication starts it is best effort until real outcome proof.

Per-entry coordination persists BeginEntry before each admitted manifest effect, durable started checkpoints,
private native results and explicit applied/not_applied/unknown states. A started/unknown entry always goes
through InspectEffect after restart/lease takeover. It never blindly rescans or repeats final I/O, even for
NoFinalEffect errors. Definitively not-applied entries stop for higher-level disposition; automatic full-tree
move/copy/delete workflows, captured-source removal order and HTTP Operations remain P4 work.

## Logical and physical accounting

Initialization requires an explicit complete native/index comparison, old-writer drain assertion and saved
nonempty operator evidence for the exact root. The service never assumes missing index rows mean garbage
or infers an empty account. Existing over-quota content remains present and charged. Zero-increment
admissions can proceed without worsening an existing over-limit budget; positive growth is rejected.

New files reserve their full logical size; replacement growth reserves `max(new-old,0)` only with captured
target path/version and an exclusive credit claim. The same native revision's credit cannot be spent by
concurrent or later writers twice. P3 only admits this old-size credit for typed upload replacements; generic
tree/source credit requires P4's finite-manifest accounting model. The local writer below does not yet
implement replacement; testing quota credit does not advertise a native replacement capability.

Physical bytes and reservations are independent from logical size. Admission covers staging/native copy or
retained-version liability separately; the generic service requires an explicit physical-cost policy for any
replacement strategy. Known partial positive entry charges are idempotent by operation/entry number, consume
matching reservations and retain every unknown liability/guard. Final cumulative settlement subtracts already
charged entries, avoiding double billing. P3 defers negative partial corrections to an uncharged final lifecycle.
An unresolved entry cannot be terminal-settled merely to release quota; unused reservations are released only
following proven no-effect cleanup or known final settlement.

The periodic cleanup primitive selects terminal upload operations with recorded temporary identities only.
It calls the bound driver's Discard, then releases retained physical reservation and records cleanup. It never
removes logical liability, operation/tombstone provenance or authoritative targets because an index row is
missing. Unknown/in-flight outcomes are excluded. Generic action cleanup requires a registered known-identity
handler in P4; unrelated terminal kinds cannot starve the P3 staging cleanup queue. No automatic production
worker is started before write admission/cutover.

## Local publication strategy and explicit limits

`localstorage.NewStagedWriter` requires `WriteOptions{CoordinatedExclusiveWriter:true}`. This is an operator
assertion that application/native external writers are coordinated, not a DB lock on Finder/rclone. Existing
read drivers still advertise mutation flags false; this separate opt-in helper is not activated by startup.

P3 admits create-only `Absent:true` publication. Expected-version replacement is rejected before allocation,
because P1 Local reads expose no trustworthy version pin and Stat+rename is not a conditional replacement.
Final bytes occupy their accepted ordinary native path; no anonymous authoritative representation is added.

Content and system roots and the selected destination parent must share an admitted local filesystem.
Supported admission is Darwin local APFS/HFS or Linux ext2/3/4, XFS, Btrfs, F2FS, tmpfs/ramfs. Network,
FUSE, overlay/unknown filesystems and other OSes fail closed. A Linux container's overlay-backed temporary
root is intentionally unsupported; native tests need an admitted TMPDIR such as a verified tmpfs.

Private 0700 system namespaces bind owner/root physical identity. A random temporary attempt and persistent
intent record are checkpointed before allocation. File/directory fsync and atomic record replacement provide
durable staging. Per-intent OS flock remains held through native Linkat and the outcome checkpoint. The lock
file is never deleted, avoiding split-lock races. Exact byte length/digest, root/parent identity, no-follow paths
and stage integrity are checked again after delayed callbacks.

Native Linkat provides atomic no-replace between pinned directory descriptors. EEXIST is a definite collision;
other native errors retain a publishing record. Recovery after acquiring the lock requires quiescence plus the
retained staged inode and verified SHA-256 to prove Applied; existence/size alone is insufficient. A native link
that applies then returns EIO is recovered as applied. An absent, quiescent effect is sealed with a terminal
tombstone, so a stale queued worker cannot publish after guards are handed onward. A missing intent record
with a retained native receipt stays unresolved instead of guessing non-application after rollback/restore.

Stage cancellation combines request and driver lifetime, closes a blocking io.ReadCloser once and releases
its lock/pool slot. Arbitrary nonclosable readers must be cancellable by their caller; no unbounded goroutine
per Read is spawned. Native Discard only removes the exact known staging filename, keeps permanent intent
fences and never deletes Target. Unknown temporary allocations are preserved for explicit reconciliation.
COS/S3 staged publication is not enabled; their provider capability/recovery gates remain unverified.

## Validation and progress

Real local tests cover exact names/bytes, zero files, digest/length rejection, no-replace races, OS-lock process
recovery, delayed callbacks, stale workers, inode/digest attribution, retained unknown evidence, post-link EIO,
blocked-body cancellation and cleanup preserving unrelated files. Linux compilation and local race/vet pass.

Disposable PostgreSQL 17 tests apply all 18 migrations twice and exercise concurrent idempotency/path/quota
admission, principal/root isolation, lease epoch loss, permanent unknown guards, checkpoint preservation,
replacement credit, over-limit no-growth, partial accounting and cumulative settlement. Actual Postgres + Local
coordinator tests additionally prove native-success/index-failure repair, response/body replay, cancellation,
revocation after native effect, owner recovery, and temporary-only cleanup without deleting ordinary files.
Fixtures use random databases and temporary native roots; no live database, bucket, `.env` or user data is used.

```bash
make test
cd backend
go test -race ./internal/fileops ./internal/localstorage
# Real database tests require EAM_TEST_DATABASE_URL for a disposable CREATEDB-capable instance.
```

Progress: P0–P2 implemented; P3 internal durable foundation implemented. P4 native actions/simple writes,
P5 resumable ingress, P6 client/grant cutover and P7 release/performance acceptance remain. P0 live COS
proofs, production baseline, old-writer quiescence and deployment verification are still separate gates.
