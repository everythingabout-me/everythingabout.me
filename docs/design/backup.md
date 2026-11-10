# Backup by whole-tree snapshots

> Status: connections (§7–§8) are implemented by `sql/035_storage_connections.sql` and
> `internal/connections`; the snapshot job (§1–§5, §9) and the restore command (§6) are not built yet.
> This replaces the earlier framing in which application metadata was part of the backup. Per `AGENTS.md` (*Bytes are the source of truth*), a backup is a
> storage-layer artifact: the user's content survives because the storage layer holds copies of the tree.
> Postgres is a read cache; losing it is a rebuild cost for the operator, never a loss for the user.

## 1. The artifact

A **snapshot** is a complete, read-only copy of one owner's native content root, named by the date it was
taken, stored under that owner's backup connection:

```text
<backup connection>/<owner>/
  2026-11-08/
    Photos/2026/trip.jpg
    Notes/diary.md
  2026-11-10/
    Photos/2026/trip.jpg
    Notes/diary.md
```

- Same relative paths, original names, raw bytes. No manifest, no index, no database needed to read it.
- Each snapshot stands alone. A restore picks a date; it never reconstructs a chain.
- Only the content root is copied. The system root (staging, spool) and the index are never part of a
  snapshot.

The layout reuses the existing per-owner root derivation (`Backend.Binding`), so a backup connection is
just another connection with a different prefix and credential. No second path scheme is introduced.

## 2. Why snapshots rather than file versions

The unit of a decision is a time, not a file. That single change removes most of the cost of versioning:

- **One restore, one choice.** A per-file history asks which version of which file; a snapshot asks which
  day.
- **Predictable contents.** Every snapshot is a whole world. A version chain's contents depend on rules
  about what was kept, which is exactly the unpredictability that makes versioning feel heavy.
- **Sharing stays invisible.** N complete trees can share bytes (hardlinks on a local target, provider-side
  copies on an object target). The user still sees N trees, never a chain.
- **Restoring one file needs no feature.** Open the dated directory with any tool.

Rejected for this feature: per-file version history (rules multiply, and behavior becomes chain-dependent),
and mirroring/synchronizing as the backup mechanism (§5).

## 3. Consistency

The source tree is live while a snapshot is taken. The guarantee is stated rather than assumed:

**Guaranteed**

- **No torn file.** Publication is atomic per representation — `CompleteMultipartUpload` for an object,
  rename for a local file — so a snapshot contains only complete representations that existed during the
  window. It never contains half an upload or a partially written file.
- **A subtree move may be split.** A move is copy → verify → delete (§4 of `storage.md`), so an in-flight
  move can appear partly at the old path and partly at the new one in the same snapshot. The result is a
  duplicate, visible and mergeable. It is never a hole.

**Not guaranteed**

- **One global point in time.** Entries are observed over a window; a snapshot is labelled by its start.
  The window is bounded by the work (entries and bytes it processes), not by a wall clock.

This is sufficient because the failure the feature prevents is loss, not forensic exactness: a duplicate
directory is visible to a person and mergeable; a missing file is neither.

**Optional strengthening.** At snapshot start, read the paths of nonterminal operations and guards and
record them in the snapshot's run log as "as of the previous snapshot". That costs one journal read and
removes the visible split. It is an improvement to predictability, not a correctness requirement.

## 4. Immutability and retention

- **Append-only is a property of the credential, not of our code.** The backup connection's credential may
  create objects under the snapshot prefix and must be denied delete and overwrite; an object-lock/WORM
  target is equivalent. A code path that "never deletes" is not an enforcement — one bug and history is
  gone.
- **Retention keeps whole dates.** The schedule and the number of snapshots kept are per-owner settings the
  user can change. Rotation removes an entire date; it never removes individual files from a kept snapshot.
- **Retention never depends on the newest snapshot's health, and never mirrors.** If the newest snapshot
  differs from its predecessor beyond a configured ratio (or shows a jump in entropy), pruning is skipped
  and an operator alert is raised. The correct response to suspected encryption is to keep more history,
  not to follow it.

Both rules exist for the same reason: encryption and accidental deletion are exactly the events where the
older snapshots are the only remaining copy.

## 5. Why not sync

A mirror is the wrong mechanism for backup, for reasons that are semantic rather than technical:

- It **follows deletion**. A mistaken delete propagates on the next cycle, and the mirror was the only other
  copy.
- It **follows encryption**. Ransomware rewrites files; a mirror replaces the good copy with the bad one in
  one pass.
- It has **nothing to restore to**. "Restore" needs a point in time; a mirror only has "now".

Sync may exist later as a separate feature with its own contract, but it must never be the thing behind
"backup", and a backup connection must never be a sync target. A backup connection is write-once from the
snapshot job and read-only to everything else.

## 6. Restore

**Whole tree** — one command, `eam backup restore --owner <id> --at 2026-11-08`:

1. Quiesce writers and settle accepted work.
2. Materialize the snapshot into a new root on the authority connection and verify it (walk + names).
3. Bind that root and bump the generation; rotate the change-stream namespace
   (`file-changes-reset --writers-quiesced`).
4. `reconcile` to rebuild the index from the native tree.

Paths, names and bytes are preserved. Service IDs change; that is accepted, because the tree is the truth
and the index is the cache.

**Single file** — no command. Open the snapshot directory with any file tool.

**The snapshot is never the live tree.** Restore materializes a separate copy and binds that; binding the
live tree directly to the backup directory would let one bug write into history. Materializing by hardlink
is allowed only while every writer replaces files instead of modifying them in place, which this service
does.

## 7. Connections are rows

```sql
create table if not exists storage_connections (
  id         uuid primary key default gen_random_uuid(),
  name       text not null unique,              -- label; not identity
  backend    text not null,                     -- cos | s3 | local
  role       text not null,                     -- authority | backup
  identity   text not null unique,              -- the digest Backend.Binding already computes
  config     jsonb not null,                    -- endpoint, region, bucket, prefixes, path style, case rules
  secret     jsonb not null,                    -- this connection's credential fields
  generation bigint not null default 1,
  admission  jsonb,                             -- measured evidence plus the identity it measured
  enabled    boolean not null default true,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);
```

Rules, each answering a specific failure:

- **Uniqueness is on `identity`, not on `(backend, bucket)`.** Two prefixes in one bucket are two trees;
  rotating a credential is still the same tree. This is the rule `Binding` already enforces, kept rather
  than reinvented.
- **A row's location never changes in place.** A new endpoint, bucket or prefix is a new generation or a new
  row. The change-stream namespace embeds connection, generation and both roots, so an in-place edit would
  let an old cursor continue on a different tree.
- **Evidence is per row and carries its identity.** A second object connection that inherits the first
  row's "atomic publish" would reintroduce the exact failure that once cost hours: evidence for one bucket
  applied to another. A mismatch means *unproven*, and unproven disables writes on that connection while
  reads continue.
- **The row carries its own credentials.** Adding a connection is one `INSERT`; a reference into a host file
  would split the definition across two places and need shell access, which is the thing this table exists to
  remove. The accepted cost is that the database holds usable credentials, so a dump is secret material.
- **The table wins over the environment for an existing owner.** Two sources of "where the tree is" would
  fight: startup would adopt on one path and the table would hold on another. The environment only seeds the
  first row (idempotent insert by identity), so a wiped database still bootstraps to today's behavior.
- **One `authority` row per owner, zero or more `backup` rows.** Authority is what the binding names;
  backups are never on the write path and their failure never fails a user request.

## 8. Runtime

- A `Resolver` implements the existing `OwnerStorageFactory`
  (`Open(ctx, ownerID) (storage.Driver, error)`), so the HTTP layer is untouched: it still sees one method
  and never sees a provider, bucket or credential.
- `Resolver` reads the owner's binding, finds the connection row, and asks that connection's `Backend` for
  a driver. `storagebackend` stays database-free, so maintenance commands and dozens of tests keep
  constructing it from environment only.
- Each connection owns its spool root, admission evidence and transfer class. A shared spool across
  connections would let one target's temporary material be published to another.

## 9. Scheduling and operation

- The job is a CLI command (`eam backup run|verify|prune`), scheduled by the deployment's timer, the same
  pattern as the existing database dump. There is no scheduler in the process today, and a long job does
  not belong inside the server's request lifecycle.
- The job bounds itself by entries and bytes processed, never by a wall clock (see `AGENTS.md`).
- One snapshot per owner per day is the default. RPO is therefore one day, which matches the stated need:
  this is backup, not sync, and the copy does not have to be reachable immediately.

## 10. Acceptance

A passing suite is not evidence that a deployment backs up. These are the gates:

1. **Restore from the artifact alone.** Fresh empty Postgres, `.env`, and one snapshot: every file is
   visible at its path after the restore procedure. This is the definition of "the backup works".
2. **No torn file.** Kill the job mid-run; every file in the partial snapshot is complete and readable.
3. **Immutability.** A delete or overwrite attempted with the backup credential is rejected by the
   provider, not by our code.
4. **Name preflight at snapshot time.** A source containing `Notes.md` and `notes.md` aimed at a
   case-insensitive target fails with the colliding paths named, instead of failing at restore time when
   the snapshot is the only copy.
5. **Retention safety.** Rotation removes whole dates; a corrupted newest snapshot never expires an older
   one.
6. **Measured growth.** The second snapshot's added bytes are measured on both a local target (hardlink
   sharing) and an object target (no copy-on-write), and recorded. The two differ by the whole tree size and
   that number decides how many snapshots a target can keep.

## 11. Out of scope

- Sync, mirroring, and bidirectional replication.
- Per-file version history or per-file restore.
- Staging connections for upload acceleration; a second connection does not by itself make an upload faster,
  only the client-to-target path does.
- Automatic failover or promotion.
- Any user-facing surface beyond a status page: dates available, last success, and the restore entry point.

## 12. Decisions in one place

| Decision | Reason it is this way | Revisit if |
|---|---|---|
| Artifact is the tree, not the database | Bytes are the product; the cache is disposable | Never for user content |
| Snapshot granularity is time | One restore decision instead of per-file rules | Users ask for per-file history |
| Immutability by credential | Code cannot promise what a bug can undo | The provider cannot express an append-only policy |
| Retention never follows the newest snapshot | Encryption and deletion are the events being defended against | — |
| A connection row carries its credentials | Adding a provider is one INSERT, nothing to place on the host | A second person can read the database |
| Table, not environment, decides the location | One authority for "where"; two would fight | — |
