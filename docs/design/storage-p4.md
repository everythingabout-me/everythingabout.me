# P4: admitted native file mutations

P4 connects the unified file API to [P3 durable coordination](storage-p3.md) for explicitly admitted
Local roots. It implements single PUT, native folder creation, finite copy/move/delete workflows and
Operations. COS/S3 remain read-only through the new contract until their real publication/conditional/
quiescence capabilities are proven. Existing COS legacy routes are not silently fenced by this release.

## Admission and deployment

Writes remain disabled by default. To prepare a Local owner root:

1. Coordinate all application/external native writers and drain any old native write authority. Back up
   owner/root mappings, application metadata and native content independently. A DB lease does not lock
   Finder, rclone, provider sessions or another process outside the declared writer model.
2. Prepare the exact content/system owner directories on an admitted same local filesystem. Run
   `reconcile --owner ... --dry-run`, inspect the comparison, then import the index. Reconcile always uses
   a read-only factory even if the write flag is set, so it does not allocate publication state.
3. Set `FILE_COORDINATED_WRITES=true` with `STORAGE_BACKEND=local`. This is an explicit operator
   assertion that native writers are coordinated; it cannot be used with COS/S3. The filesystem and
   root-isolation restrictions in P3 still apply. Configure `FILE_MAX_SINGLE_PUT_BYTES` if needed
   (default 33554432, deployment policy rather than a provider limit).
4. Before starting write-serving requests, initialize the owner's budget explicitly:

```bash
cd backend
go run ./cmd/eam storage-admit \
  --owner-id 00000000-0000-0000-0000-000000000001 \
  --writers-quiesced --evidence /secure-backup/admission-proof.json --dry-run
# Inspect the output, then repeat without --dry-run.
```

The evidence file must be a bounded nonempty JSON document; it is not printed. The command requires a
complete exact native/index comparison (including folders, parent/name/key mapping and size), an existing
owner and an empty pinned system owner root. Unknown system content is preserved and admission refused.
It uses a read-only clone of backend configuration, so dry-run allocates no native state and writes no budget.
Logical and physical baseline both include all committed native file bytes; the system root is empty at this
first admission. Existing over-quota files are preserved. An already initialized identical root reports retained
usage without resetting counters or replacing evidence/limits; another root/generation requires migration.

Starting a mutation wrapper creates private system ledger directories, so perform initial admission before
serving mutation-enabled requests. No startup path automatically trusts an index SUM or creates an empty
budget. Missing baseline returns a safe unavailable response. Keep `APP_SECRET`, independent SES credentials
and explicit `FILE_COOKIE_ORIGINS` from earlier phases. Cookie changes require the `/api/files/me` CSRF token.

## HTTP resources

| Method | Resource | Behavior |
|---|---|---|
| PUT | `/api/files/content?path=/...` | Stream exact bytes to an existing native parent; create or conditional replacement |
| POST | `/api/files` | `{kind:"folder",path,parents?}` creates native folders explicitly |
| POST | `/api/files/move` | `{from,to,ifRevision}`; no merge/target replacement |
| POST | `/api/files/copy` | `{from,to,ifSourceRevision}`; recursive finite folder copy, independent files |
| DELETE | `/api/files?path=/...&recursive=true` | Captured native deletion; nonempty folders require explicit recursion |
| GET | `/api/files/operations/{id}` | Creator/scoped durable state/result, including after source deletion |
| POST | `/api/files/operations/{id}/cancel` | Stop further safe steps; no promise to undo applied or unknown effects |

Successful PUT returns 201 for create or 200 for replacement only after native placement, index repair,
usage and result settlement. Content-Length is required (411), zero is valid, over-limit bodies return 413,
and optional `Content-Digest: sha-256=:base64:` verifies ingress. `X-Source-Modified-At` is accepted as RFC3339
and retained as actual native modification time. Bad length/digest never replaces old complete bytes.
Existing target default create is 409; explicit `If-None-Match:*` collision is 412. Replacement requires one
strong contentRevision ETag in If-Match. Publication checks native conditions, captured parent identity and
current privileges again. Missing parents are not silently created; parent movement/recreation invalidates binding.

An optional Idempotency-Key binds the original client intent, including path, declared size, MIME, source
modification time, digest and public create/replace condition, independently of captured native versions.
Identical replays consume/validate bytes before returning the recorded result. A different body/intent conflicts.
Terminal replay validates current credentials/scope but does not require an old moved/deleted parent to exist.
An unresolved final effect returns a Problem with Operation identity/Location, not a claim of rollback.

Folder creation defaults to parents:false; parents:true records each real parent creation. Existing native
folder returns 200 without changing its revision. Move/copy source revisions are required (428 when absent,
412 when stale), both paths are authorized, root/self/descendant actions are rejected, and collisions never
pre-delete destination bytes or merge folders. Native copy produces independent ordinary bytes, not hardlinks.
File/folder deletion is permanent; only captured unchanged sources are removed, never a new rescan's additions.

Fast actions return metadata or 204. `Prefer: respond-async` returns 202 + Operation + origin-relative Location
and Retry-After, while bounded workers open a fresh root and recheck the creator's current token/scope before
new effects. The standard Operation is used for synchronous uncertainty too. Polling/cancel require the creator
principal or explicit owner management; sharing an owner is not enough for another app. Partial failure/cancel
reports applied paths and remaining captured entries. Native references, guard internals and intents stay private.
Resumable `/uploads` remains unavailable until P5; thumbnails and Web/media migration remain later work.

## Native execution and recovery

Local mutation wrappers require the same admitted coordinated writer model and filesystem checks as P3.
File versions bind verified SHA-256, native identity, size, mtime and mode; directory versions bind stable native
identity, while API metadata revision also hashes direct membership. Local replacement uses atomic rename
with pre-effect version revalidation under coordination. Atomic no-replace creation/move uses native primitives.
`NativeConditionalMutation` remains false: application coordination does not pretend to be native backend CAS.
Open pins/validates the selected representation; no arbitrary external in-place writer is claimed fenced.

Every effect has a persistent intent, checkpoint and OS lock through the native call and its outcome record.
Ambiguous native errors retain executing/publication state. Recovery inspects the same native identity and never
replays because a worker lease expired. Mismatched owner/root/naming bindings are rejected before native I/O.
Remove is file-or-empty-folder only; the service, not the primitive, owns explicit recursive enumeration.

Action preparation takes a conservative actor-scope subtree guard before revision validation and enumeration,
then freezes a finite bounded manifest and native ancestor/source identities. This serializes all owner writes
for an owner-scope actor. A preparation crash before freeze is lease-fenced and terminated as proven pre-effect,
so it cannot leave a permanent orphan guard. Manifest capture revalidates observed identities before freezing.

Folder moves deliberately use captured copy/mkdir steps, verify the entire completed destination set, then remove
captured unchanged sources in postorder. No native root-folder move shortcut infers descendant index changes.
External additions survive and may cause the final empty-folder removal to fail partially. Recovery follows saved
manifest entries, with unknown entries retaining guards and reservations. Already-proven native effects are indexed
and charged even if the original creator is revoked; revocation prevents future effects, not repair of actual files.

Copies reserve full new logical and physical sizes. Moves conservatively reserve possible independent duplicate
bytes until source removal, so near-quota moves may need headroom even when an atomic rename could avoid it.
Deletes spend only captured source-version credit; partial positive charges and final cumulative settlement avoid
double billing. Successful/failed PUT foreground cleanup discards known staging before releasing retained physical
liability; periodic cleanup retries failures. Unknown effects are never cleaned as if aborted.

Workers are bounded to four concurrent operations per server. Durable pending scans avoid newly admitted requests
and active leases; recovery after restart waits for safe worker takeover. They retain no HTTP bearer secrets.
First-party owner work becomes account-owned after authenticated admission; token/save-link work is revalidated
against current DB permissions/revocation before each new effect. Metadata/read deadlines remain bounded;
large byte streaming and accepted workers use their own lifecycles. Cancellation before publication can discard
staging; cancellation during final I/O remains unknown until native outcome proof.

## Index, content generations and read behavior

Migrations 019/020 add frozen action bindings and content-generation mappings; no file bytes are stored.
Settlement updates only proven applied native entries. Moves preserve private node IDs/event references when
captured source removal and matching target placement are proven; partial copies while sources remain get
independent IDs. Copies never alias source application identity. Index failures roll back only the database
transaction, leaving native bytes and recovery evidence intact for repair.

Metadata results use shared revision helpers with subsequent GET. Final folder results capture current native
root metadata and direct children under the retained guard, including existing-folder no-ops, rather than assuming
an early mkdir timestamp still describes a completed tree. Stored content generations bind a contentRevision to
exact native evidence; proven move pairs transfer that revision to the new path/inode, while replacement/copy
creates a new generation. Lookup rechecks the fingerprint, so stale mapping cannot validate changed native bytes.
Native ContentIdentity remains private. Read-only backends without these proofs keep P2's conservative validators.

Mutation-enabled Local listings validate a parent once per page and enrich direct children without rescanning all
siblings for each Stat. File hashing still reads returned bytes to establish trustworthy evidence, so large datasets
have measured-cost work ahead. Limits remain finite (100000 captured entries, 1000 listing pages); they are not
an unmeasured performance guarantee. Search/list caches and read request deadlines remain active.

## Validation and progress

Tests use isolated native temporary roots and dedicated disposable Postgres 17 databases, never `.env`, live
buckets or user roots. They cover real Cookie/Bearer HTTP writes, CSRF and scope/owner isolation, exact names,
zero/binary bytes, checksums/length, conditional replacement preserving old content, mkdir/copy/move/delete,
contentRevision continuity, independent files, explicit recursion, async results, cancellation, retained deletion
polling and idempotent replay. Native tests inject lost replies, applied-then-error, stale callbacks, inode/ancestor
changes, symlink swaps and restart recovery. Actual CLI tests prove dry-run and baseline initialization do not
rewrite content/index or reset existing usage. All schema migrations are exercised twice.

```bash
make test
cd backend
go test -race ./internal/api ./internal/fileops ./internal/localstorage ./internal/nodes ./internal/filerevision
# Actual database integrations require EAM_TEST_DATABASE_URL for a dedicated disposable instance.
```

Progress: P0–P3 implemented; P4 adds admitted Local native writes/actions and Operations. COS/S3 mutation
capability proof remains a release gate, with their new write routes explicitly unavailable. P5 resumable ingress,
P6 Web/media/legacy-grant cutover and P7 deployment/restore/performance acceptance remain. No production
admission, storage rewrite, external credential revocation or live provider mutation was performed by this work.
