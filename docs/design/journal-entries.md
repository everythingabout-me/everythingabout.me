# Operation journal entries: measured cost, and the table that would replace them

> Status: **measured; Release A and Release B implemented.** Phase 1 of this document is
> the measurement the sql/047 header asserted but never took; Phase 2 is the table design that
> measurement justifies. Release A (schema, shadow writes, backfill, no reader switch) is in
> `sql/049`, `sql/050` and `internal/fileops/postgres_entries.go`; §4 records its measured cost.
> Release B (the flip: new rows are `entries_layout='table'`, the document carries no entries,
> settlement loads the rows inside its own transaction, a `'document'` row is upgraded on
> `Claim`) is in `internal/fileops/postgres_entries.go`, `postgres.go`,
> `postgres_transitions.go` and `postgres_actions.go`; §5 records what it measured. It is
> **forward-only** (§2.6).
>
> The measured harness is `backend/internal/fileops/journal_entries_cost_test.go`
> (`TestJournalEntriesDocumentCost`). It drives a real move through `Actions.Prepare` +
> `Actions.Run` against a disposable PostgreSQL database, so every transition is the
> production one (`Claim`, `BeginEntry`, `SaveEntry`, the native checkpoint save, `Release`,
> `Settle`); only the native tree is in memory. Section 3 says how to reproduce it.

## 1. Measurement

### 1.1 What is measured, and how

`file_operations.snapshot` is the whole Go `Operation` as one `jsonb` document. Since
`sql/047` the fields SQL predicates read are real columns, but every transition still
rewrites the whole document, entries included. Two instrumentations, on the same operation:

- **Bytes.** An `AFTER INSERT OR UPDATE` trigger on `file_operations` records, for every
  write, `octet_length(snapshot::text)` — the logical JSON volume the settlement path
  produces — and `octet_length((snapshot - 'Entries')::text)`, the same document with only
  the `Entries` key removed. The second number is the like-for-like comparison the review
  asked for: it is what the same transitions would write if entries lived somewhere else.
  The whole operation's WAL volume is measured separately with
  `pg_current_wal_insert_lsn()`, because logical JSON is not what a disk sees.
- **Time.** A second pool (trigger off, so recording costs nothing) carries a `pgx`
  `QueryTracer` that attributes every statement to a class and sums its execution time.
  Every journal transition is one transaction that takes the owner advisory lock first and
  releases it at COMMIT, so the traced statements of a transition are the floor of its
  owner-lock hold; the report also gives the run's total wall clock, so the share is
  visible. The native tree is in memory, so the run's wall clock is the journal path with no
  provider latency; a real COS move adds time to the same run and lowers the journal's *share*
  of it, but not its per-transition cost or the owner-lock hold.

A move of *N* files is the shape used: the frozen manifest has one `copy` and one `delete`
per file plus the folder's own `mkdir`/`delete` — `2N+2` entries. The fixture entries carry
a native ETag, a native ref, a SHA-256 digest, media type and mtime, so the document is the
size a real photo manifest is, not a path-only skeleton.

### 1.2 Results

```bash
EAM_JOURNAL_COST_MEASUREMENT=1 EAM_TEST_DATABASE_URL=postgres://… \
  go test ./internal/fileops -run TestJournalEntriesDocumentCost -v
```

Section 3 has the `make` targets.

| files | manifest entries | journal writes | writes / entry | peak document | logical document bytes written | same transitions without `Entries` | WAL | run |
|---:|---:|---:|---:|---:|---:|---:|---:|---:|
| 100 | 202 | 1,017 | 5.03 | 275 KB | 214 MB | 1.92 MB (0.9%) | ~17 MB | — |
| 500 | 1,002 | 5,017 | 5.01 | 1.36 MB | 5,248 MB | 9.45 MB (0.2%) | ~369 MB | 3m00s |
| 5,000 | 10,002 | *freeze only* | — | **8.18 MB** | — | 7.2 KB | — | — |

WAL varies by a few percent between runs with checkpoint timing; the other columns are
deterministic on one machine. The 5,000-file row is the frozen manifest only: admission plus the one write that stores
the manifest. Its document is 8.18 MB — the "roughly 10 MB" the review named — and settling
it would perform the same 5 writes per entry the 500-file run performed 5,017 times. That
run is not executed because, at the measured per-write size, it would write roughly **520 GB**
of JSON (5.25 GB × (10,002/1,002)²), about five hours of database time on the machine that
measured it.

Transition breakdown of the 500-file move (5,017 writes):

| what | count | document bytes |
|---|---:|---:|
| per-entry transitions (`phase=effect_started`) | 5,010 | 5,242,909,065 |
| admission / claim / freeze / release-around-run (`phase=admitted`) | 4 | 2,460,336 |
| `SaveActionApplied` (`phase=native_applied`) | 1 | 1,363,499 |
| settlement (`state=succeeded`) | 1 | 1,363,536 |
| the `INSERT` at admission | 1 | 1,743 |

The five writes per manifest entry are exactly: `Claim`, `BeginEntry`, the native
checkpoint save (`SaveEntry state=started`, which the fake tree issues through the real
checkpoint callback), `SaveEntry state=applied`, and `Release`.

### 1.3 Does the claim hold?

**Yes, and it is understated.** The document is rewritten 5 times per manifest entry — 10
times per file for a move — not "dozens per operation", and because both the number of
writes and the size of the document grow with the entry count, the total is quadratic: for a
500-file move the database parses and rewrites half a gigabyte of JSON per 100 entries.

The two numbers that change the decision:

- **5.25 GB for a 500-file move, ~520 GB projected for 5,000 files.** The 5,000-file
  manifest document is a measured 8.18 MB; the write count per entry is measured at 5.01 and
  is constant. Nothing in the projection is a guess about where bytes go.
- **The owner lock was held for ~108 ms per manifest entry, ~216 ms per file.** Five
  transitions, each a locked document read (6.5 ms average) plus a document write (14.9 ms
  average, at a document that grows to a 1.36 MB peak) plus the advisory lock and commit. At
  10,002 entries the document is ~6× larger, so a 5,000-file move held the owner lock for
  close to two hours, and that owner's uploads and other operations queued behind it. The
  lock is per owner, so what it serialized was one owner's own writes — which on a large
  import is the only traffic there is.

  The lock is now taken only by the steps that write state two operations share: taking and
  releasing guards, charging `file_budgets`/`file_reservations`/`file_entry_charges`,
  appending change facts, and applying the entry index. The transitions that write one
  operation's own row — the checkpoint, the staged evidence, the effect intent, the observed
  result, an entry charged *before* `Settle` applies it — take that row's `for update` alone
  (`fileops.transactionOwnRow`), so they no longer queue the owner. This changes the number
  above rather than the argument: it was always the owner-wide state that had to be ordered.

What the review got wrong or left out:

- **The comparison target is 0.2%, not "dozens of times cheaper".** The same transitions
  with a document that carries no entries write 9.45 MB instead of 5,248 MB. Entries are
  99.8% of the volume; the promoted columns already did the cheap part.
- **Reads cost as much as writes and were not counted.** The 500-file move performed 5,017
  locked document reads (32.4 s) and 7,021 read-only document reads (20.7 s): 53 s of the
  180 s run spent parsing documents that were only ever read. The status endpoint
  (`writeFilesOperationWithRetry`) and `problemDetail` parse the whole document on every
  client poll, and the pending scan (`PendingPageMax`) parses a page of 4,096 of them.
- **"10 MB per transition" is a logical-byte figure; WAL is 7% of it.** The operation's
  whole WAL volume was ~369 MB against 5,248 MB of JSON, because `jsonb` is TOAST-compressed.
  That does not rescue the design: the cost that hurts is the client-side marshal, the
  server-side parse and the TOAST rewrite per transition, all of which track the logical
  size — and the lock hold, which is what serializes the owner, is measured by wall clock,
  not WAL.
- **`maxActionEntries` (100,000) is not a usable bound for copy/move.** At the measured
  per-entry cost a 100,000-entry manifest would write ~50 TB. The manifest limit is not what
  bounds these operations; the journal's storage shape is.

### 1.4 Consequence for the decision

The cost is what the review said and worse. Phase 2 is justified: move `Entries` out of the
document.

What must **not** change: per-entry transitions still rewrite the parent document.
`sql/047` deliberately kept the promoted fields in the document so a document still reads as
a whole, and a parent document is ~1.8 KB — the 5 writes per entry would then cost ~9 KB per
entry instead of ~5.2 MB. Rewriting a 1.8 KB document is not the problem; rewriting it with
10,002 entries in it is.

## 2. Design

### 2.1 Table

```sql
-- One row per frozen manifest entry. The parent `file_operations` row stays the operation
-- document (intent, result, guards, reservation, problem, purge state), minus `Entries`.
--
-- Why a row and not a document: every entry has its own durable state (pending → started →
-- applied/not_applied/outcome_unknown) that is written 5 times, and every write of one
-- entry's state must not rewrite the other 10,001. A row makes one entry's transition one
-- row update.
create table if not exists file_operation_entries (
    owner_id       uuid    not null references users(id),
    operation_id   uuid    not null references file_operations(id) on delete cascade,
    -- The entry's manifest number. It is already the identity used by
    -- file_operation_checkpoints.entry_number, file_entry_charges.entry_number and the
    -- change fact key "<operation>:entry:<number>", so it is not a new ordering: it is the
    -- existing one made a key. It is assigned once at freeze and never renumbered.
    number         integer not null,
    action         text    not null,   -- mkdir | copy | move | delete
    source_path    text    not null,   -- "" for mkdir
    target_path    text    not null,   -- "" for delete
    source_revision bytea,             -- captured native revision of the source
    -- The captured storage.Entry this effect was admitted against. It is jsonb because no
    -- SQL predicate ever reads into it: it is decoded only by the settlement that validates
    -- the effect, exactly as it was decoded from the document. Keeping it as a document
    -- inside the row is deliberate, and does not reintroduce the 047 problem (a predicate
    -- over a document), because there is no predicate.
    source_entry   jsonb,
    target_absent  boolean not null,
    target_revision bytea,
    size           bigint  not null,
    state          text    not null,   -- pending | started | applied | not_applied | outcome_unknown
    checkpoint     bytea,              -- driver checkpoint of the last attempt
    evidence       bytea,              -- driver recovery evidence
    result         jsonb,              -- validated native result once applied
    updated_at     timestamptz not null default clock_timestamp(),
    primary key (operation_id, number)
);

-- Owner isolation: owner_id is denormalized from the parent. The only writer copies both
-- from the same parent row it already locked, so a row cannot name a different tenant, and
-- every query can filter the tenant without joining the journal. The index makes owner-scoped
-- cleanup and accounting (prune, future per-owner reports) not depend on the join.
create index if not exists file_operation_entries_owner on file_operation_entries(owner_id);

-- The one cross-operation query is recovery: "does this operation still have an entry that
-- may have landed?" It is answered per operation, so the primary key already covers it. No
-- partial state index is added: unlike the journal itself, this table is deleted with its
-- parent (ON DELETE CASCADE), so it never accumulates the settled rows a partial index would
-- have to skip.
```

The parent gains one immutable column so the callers that only need a count never read the
table:

```sql
alter table file_operations add column if not exists entry_count integer not null default 0;
alter table file_operations add column if not exists entries_layout text not null default 'document';
-- 'document' = entries are in snapshot->'Entries' (pre-migration rows, and rows written by
-- the previous release during its deploy window); 'table' = entries are in
-- file_operation_entries and the document carries none. Readers branch on this column, never
-- on whether the table happens to have rows.
```

`entry_count` is set once at freeze from the manifest, and entries are never added after the
freeze (`FreezeAction` is the only writer), so it cannot drift from the table. It exists
because `writeFilesOperationWithRetry` reports `total = len(op.Entries)`, `validation` bounds
the manifest by it, and `mkdir` asks whether the manifest is empty — all of which would
otherwise force a query for a number the freeze already knows. It is authoritative **only**
when `entries_layout='table'`; a `'document'` row's count is in its document.

### 2.2 Incremental write protocol

Freeze (one transaction, one statement that cannot be partly applied):

```sql
insert into file_operation_entries (owner_id, operation_id, number, action, source_path, ...)
select @owner_id, @operation_id,
       (e->>'Number')::int, e->>'Action', ...
from jsonb_array_elements(@manifest::jsonb) e;
update file_operations set entry_count=@count, entries_layout='table', snapshot=@document_without_entries, ... where id=@id;
```

Admission either observes every entry in the table or none: the `INSERT` and the parent's
`entry_count` are in the same transaction as the guard freeze.

One entry's transition is one row update plus the parent document rewrite, in one
transaction:

```sql
-- BeginEntry
update file_operation_entries set state='started', updated_at=clock_timestamp()
 where operation_id=@id and owner_id=@owner and number=@n and state='pending';
-- 0 rows ⇒ recovery_required (the entry is not pending: something else claimed it or it is
-- already terminal). Never a second effect.

-- SaveEntry (checkpoint, applied, not_applied, outcome_unknown)
update file_operation_entries
   set state=@state, checkpoint=@checkpoint, evidence=@evidence, result=@result,
       updated_at=clock_timestamp()
 where operation_id=@id and owner_id=@owner and number=@n;
```

The parent document rewritten on each of those transitions is the same document as today
minus `Entries` (~1.8 KB). It still carries state, phase, lease, problem, result, guards,
reservation, purge state, stage and the promoted fields, so `sql/047`'s contract — a
document reads as a whole, and its columns are derived from it in the same `UPDATE` — is
unchanged.

The serialization that makes this safe is **the parent row lock**, not the entry row. Every
transition goes through `readOperation … FOR UPDATE` on `file_operations` (the `leased`
path), so two writers cannot interleave entry states for one operation, and the entry update
is ordered behind that lock. This is the one rule the implementation must not lose: an entry
row must never be written outside a transaction that already holds the parent row lock. A
direct `UPDATE … FROM` the API, or a batch that updates entries before reading the parent,
would reintroduce the interleaving the current code prevents.

### 2.3 Atomicity with index settlement (the part that must not break)

Settlement stays one transaction, and that is what preserves atomicity: the entry rows are
in the same database as the index, so `Settle` committing the index rows and the journal
rows is still one COMMIT.

`Settle` today loads the operation (document) under the row lock, verifies every entry is
applied, and calls `commit` (`nodes.CommitFileAction`), which iterates `op.Entries` and
writes the index and the change facts. The design adds exactly one step and moves nothing
else:

1. `Settle` loads the parent (document, no entries) `FOR UPDATE`;
2. loads the entries in one ordered query inside the same transaction:
   `select number, action, source_path, target_path, source_revision, source_entry,
   target_absent, target_revision, size, state, checkpoint, evidence, result
   from file_operation_entries where operation_id=$1 and owner_id=$2 order by number`;
3. builds the same in-memory `[]EntryEffect` the document used to decode, and runs the
   existing validation and commit path unchanged.

The invariant the index commit relies on — "every applied entry's evidence is durable before
the index row that names it" — is unchanged: the entry updates and the index rows commit
together, and a crash before the commit rolls all of them back. What must hold:

- **The parent row lock precedes the entry read.** Without it a concurrent transition could
  change an entry between the read and the commit, and `commitFileAction`'s "all entries
  applied" check would be checking a state that is no longer true.
- **The commit callback still receives the entries, not a count.** `CommitFileAction`
  re-validates each applied entry's `Result`/`Evidence` pairing and its `SourceRevision`; a
  design that trusted `state='applied'` alone would let a wrong result into the index without
  its evidence.
- **`file_credit_claims` stays as it is.** Deletion credits are keyed by
  `(owner, path, revision, operation)` and are read during settlement; they are not entries
  and must not be collapsed into this table.

Everything else in `Settle` (budget, reservation, guards, problem) is untouched.

### 2.4 What reads entries today, and where each goes

**The loading rule is part of the design.** Today `Get`/`readOperation` decode the document
and `op.Entries` is simply there, so every caller scans it for free. After the change a
blanket "load the entries with the operation" would put back an O(N) read per transition —
`Actions.Run` calls `Get` once per entry — and the measurement says reads are 30% of the
cost, so the design is explicit instead:

- `Get`/`readOperation` decode the parent. For `entries_layout='document'` the entries still
  come out of the document exactly as today (terminal rows keep that layout); for
  `entries_layout='table'` the returned `Operation` has no `Entries`, and callers that need
  the list ask for it.
- `Settle` asks for it, once, inside the transaction (2.3). That is the only O(N) load in the
  settlement path.
- Every other per-transition question becomes a SQL predicate on one row, or a `LIMIT 1` /
  `EXISTS`, evaluated under the same parent row lock.

| reader | today | after |
|---|---|---|
| `Service.coordinateEntry` / `ApplyEntry` (one entry's effect and resume) | `manifestEffect` scans `op.Entries` for `Number` | `select … from file_operation_entries where operation_id=$1 and owner_id=$2 and number=$3` |
| `Actions.Run` loop (recover uncertain, then drive pending) | two passes over `op.Entries`, inside a per-entry `Get` that re-read the whole document | **one** ordered load per run, then two passes over the in-memory list; each transition refreshes its own row with one ordered read. The sketch here said "two ordered queries for the numbers", but the run also validates whole-manifest facts (destination verification, mkdir parents, deltas) that need the list anyway, so loading it once per run is the smaller number of reads and still removes the per-entry manifest read the measurement counted |
| `Postgres.Claim` / `StartEffect` / `SaveApplied` (`uncertainEntries`, `anyApplied`) | scans the document | `exists (select 1 from file_operation_entries where … and state in (…))` |
| `Postgres.Settle` (`validateActionSettlement`, pending/uncertain/all-applied checks) | scans the document | `exists`/count predicates plus the ordered load of 2.3, then the same checks |
| `nodes.CommitFileAction` (index + change settlement) | iterates `op.Entries` | the same `[]EntryEffect` built from the settlement load — code unchanged |
| `nodes.fileChanges` (move pairing, per-entry change facts) | iterates `op.Entries` | same |
| `fileops.AccountEntry` | finds the entry, checks `applied` | one row read |
| `fileops` recovery scans (`ResolveNotApplied`, `MarkUnknown`, prune, cleanup) | `uncertainEntries`/`anyApplied` on the document | the same `EXISTS` predicates |
| API progress (`writeFilesOperationWithRetry`) | `len(op.Entries)` + scan for applied | `entry_count` for `total`, `select count(*) filter (where state in ('applied','not_applied'))` for `completed` |
| API failure detail (`problemDetail`) | scans for the first non-applied entry | `select action, source_path, target_path from file_operation_entries where … and state not in ('applied','not_applied') order by number limit 1`, plus the applied count |
| `nodes.CommitNativeTreeMove` / `mkdir` (`len(op.Entries)` cases) | length of the list | `entry_count` on the parent |
| pending scan (`EachPending`), prune, cleanup candidates | decode the whole document per row | unchanged code; the document no longer holds entries, so a page is kilobytes instead of megabytes |
| `canonicalOperation` (normalizes entry `Result` JSON) | walks entries | the helpers that write entries call it before the row write; the settlement load reads canonical JSON |

Two callers need attention because they currently treat "no entries" as a fact about the
operation rather than a fact about the document: `mkdir` (`len(op.Entries) == 0` is a
verified no-op create) and `CommitNativeTreeMove` (`len(op.Entries) == 1`). Both must read
`entry_count`, which is immutable after freeze, not "the document has no `Entries` key".

### 2.5 Backfill

Release A populates the table for every row that already carries entries in its document.
That covers the live journal (the operations that will still transition) and the retained
terminal rows the journal keeps until retention. The migration is read-only with respect to
`file_operations`: it adds rows, never rewrites a document and never flips
`entries_layout`, which is what keeps Release A rollback-safe.

```sql
insert into file_operation_entries (owner_id, operation_id, number, action, source_path,
        target_path, source_revision, source_entry, target_absent, target_revision, size,
        state, checkpoint, evidence, result)
select o.owner_id, o.id, (e->>'Number')::int, e->>'Action',
       coalesce(e->>'Source',''), coalesce(e->>'Target',''),
       case when e ? 'SourceRevision' then decode(e->>'SourceRevision','base64') end,
       e->'SourceEntry',
       coalesce((e->'TargetConditions'->>'Absent')::boolean, false),
       case when e->'TargetConditions' ? 'ExpectedRevision'
            then decode(e->>'TargetConditions'->>'ExpectedRevision','base64') end,
       coalesce((e->>'Size')::bigint, 0),
       coalesce(e->>'State','pending'),
       case when e ? 'Checkpoint' then decode(e->>'Checkpoint','base64') end,
       case when e ? 'Evidence' then decode(e->>'Evidence','base64') end,
       e->'Result'
  from file_operations o
  cross join lateral jsonb_array_elements(
      case when jsonb_typeof(o.snapshot->'Entries') = 'array'
           then o.snapshot->'Entries' else '[]'::jsonb end) e
on conflict (operation_id, number) do nothing;
```

It is idempotent (`on conflict do nothing`; a rerun after a partial journal is not possible,
but a rerun after plain success inserts nothing) and restartable: the migration runner
executes the file in one transaction, so an interrupted run rolls back whole and the next run
starts from the same state without half-writing a row. The lateral uses an explicit
empty-array fallback rather than a `WHERE … jsonb_typeof` clause, because a filter is not
guaranteed to run before the set-returning function and a missing `Entries` key would
otherwise raise. The scope is bounded by the journal's retention: only copy/move/delete carry
entries, and terminal rows are pruned by `file-operations-prune` once their window passes.

Two things make the backfill's "existing rows" complete after the deploy:

- **The deploy window.** A previous-release binary writing between `migrate` and the restart
  adds rows with `entries_layout='document'`, a document, and no table rows. Release B
  upgrades such a row on its first `Claim`, under the parent lock: re-read the document's
  `Entries` (the authority for that layout), `insert … on conflict (operation_id, number) do
  update set …` so a stale shadow cannot survive, set `entry_count`/`entries_layout='table'`,
  strip `Entries` from the document, and commit before the claim proceeds. Idempotent and
  repeatable, and it is the only path that ever turns a `'document'` row into a `'table'` one
  during normal operation.
- **The backfill's own staleness.** While Release A runs, its shadow writes keep new rows
  current, but a row that A backfilled and then updated through the document path has a
  shadow that is behind. That is exactly what the lazy upgrade repairs, which is why it
  overwrites rather than inserts-and-ignores.

A terminal `'document'` row keeps its document and is never upgraded: it cannot take another
entry transition, `Get` serves its progress from the document exactly as today, and prune
deletes it (and any shadow rows, by cascade) when retention passes.

### 2.6 Rollback: two releases, because one cannot be rolled back

A single release that stops writing `Entries` cannot be rolled back. A previous-revision
binary reads the document, so against a `'table'` row it sees an operation with no entries.
For a move or copy that is not silent corruption — `Settle` re-derives the expected manifest
from the frozen `file_action_bindings` and refuses with `invalid_intent` — but it is not
recovery either: the worker retries an operation that can never settle, its guards keep
fencing the source and destination paths, and the native copies it did make are only adopted
by a reconcile because the index never learned about them. A stuck operation plus an operator
procedure is still a rollback failure.

So:

1. **Release A (schema + shadow writes, rollback-safe).** Add the table, `entry_count` and
   `entries_layout` (default `'document'`); backfill every row that carries entries; write
   the entry rows as a *shadow* for new operations while the document stays authoritative and
   still carries `Entries`. Release A reads and settles exactly as today, so rolling the
   binary back to the previous revision is safe: the schema is additive and every document is
   complete.
2. **Release B (flip).** New operations are written as `entries_layout='table'` with no
   `Entries` in the document; readers branch on the column; the settlement loads the table;
   rows still marked `'document'` are upgraded on `Claim` (2.5). Release B reads both layouts,
   which is what lets the deploy window and the backfill coexist.

**Accepted decision: Release B is forward-only.** An old binary cannot settle a `'table'`
row: it decodes the document, sees an operation with no entries, and a move or copy fails
`invalid_intent` while its guards keep fencing the source and destination paths; any native
copies it made are adopted only by a reconcile, because the index never learned about them.
The old binary also cannot upgrade the row — the upgrade exists only in this release — so the
operation never recovers on its own.

There is therefore no runtime downgrade and no reversing migration (sql/051 is comments
only). The drain alternative is named here so the next operator knows what a rollback would
require: stop the writers, let every non-terminal operation settle (or end it through the
explicit recovery commands), confirm
`select count(*) from file_operations where entries_layout='table' and state not in
('succeeded','failed','cancelled')` is zero, and only then start the older binary. That
procedure is the whole rollback, and it is deliberately *not* automated. The same statement
is written next to the column (`sql/051`'s column comment), because that is where someone
about to redeploy an older commit will look.

### 2.7 Riskiest part

**The settlement load is the riskiest part, not the table.** The entry rows become the only
record of what was applied, so the one thing that must not slip is that the load happens
inside the transaction that holds the parent row lock and is the same list the index commit
validates. Today a wrong `op.Entries` cannot happen because the document and the row lock are
one read; after the change there are two reads, and the natural-looking refactor — load the
entries in `Get`, pass them into `Settle` — would reopen exactly the window this repository
has fought before: a native effect recorded in a row that the index commit did not see, or an
index row committed for an entry whose evidence was not read. Mitigation: `Settle` owns the
load and the commit callback takes `[]EntryEffect` as an argument; a lint-style test (like
`TestOperationJournalPredicatesReadColumns`) fails on any `from file_operation_entries` in
the package outside the allowed readers (`Settle`, `Claim`, `Pending`-style scans and the
progress query), which keeps the settlement load from being hoisted.

Second: the shadow writes in Release A double the entry write volume for one release. That is
a one-release cost, and it is why Release A must not be kept for long.

### 2.8 If it has to be cut across releases

Cut the table out of the first release and ship the write-count reduction alone. The
measurement says five writes per manifest entry are avoidable without any schema change: the
per-entry `Release` (the enclosing `Run` already holds a lease and immediately re-claims the
next entry) and the `Claim` that re-acquires it are two of the five, so an operation that
holds one lease for its whole run cuts the volume by ~40% — 5,248 MB to ~3.1 GB for the
500-file move, with no migration and nothing to roll back. What that cut does **not** fix is
the quadratic growth: the document still carries every entry, so a 5,000-file move still
rewrites ~310 GB. It is a defensible first release only if the table is guaranteed to follow;
as a permanent answer it is not enough.

## 3. Reproducing the measurement

```bash
# 500 files: bytes, WAL and traced timing (about six minutes)
EAM_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable \
  make measure-journal

# other sizes, and the freeze-only pass used for 5,000
FILES=100 make measure-journal
FILES=5000 FREEZE=1 make measure-journal
```

`EAM_TEST_DATABASE_URL` must name a disposable PostgreSQL service with `CREATEDB`; the
harness creates a random database, applies every migration twice, and drops it. The gate
`EAM_JOURNAL_COST_MEASUREMENT=1` (which `make measure-journal` sets) keeps the harness out of
`make test` and `make test-pg`. The harness asserts nothing: it measures and prints, so a
slow or fast machine changes the numbers, not the result.

## 4. Release A: implemented

Release A adds the table from §2.1, `file_operations.entry_count` and `entries_layout` (default
`'document'`), fills the table from the documents already in the journal, and makes every entry
transition write its row to the table **in the same transaction as the document rewrite**.
Nothing reads the table: the document stays authoritative, settlement and progress are unchanged,
and a previous binary runs against this schema unchanged. Schema: `sql/049`. Backfill:
`sql/050`. Shadow: `internal/fileops/postgres_entries.go`.

**Where the shadow is written.** `FreezeAction` (one bulk insert, plus `entry_count`), the
admission `INSERT` when a `Begin` carries a manifest, and `BeginEntry` / `SaveEntry` (one row
upsert each). Each runs inside the transaction that already holds the parent row lock and that
`saveOperation` writes the document in, so a commit produces both writes or neither.
`TestShadowEntryAndDocumentWriteCommitTogether` proves the direction a crash would hurt by
making the document update fail *after* the shadow write: the transaction rolls back and the
table is not ahead of the document.

**Measured cost of the shadow.** `FILES=500 make measure-journal`, same machine, same fixture;
the "before" column is §1.2 above.

| | before | Release A | delta |
|---|---:|---:|---:|
| journal writes | 5,017 | 5,018 | +1 |
| logical document bytes | 5,248,098,217 | 5,248,100,046 | +1,829 (+0.00003%) |
| WAL | 368,974,256 | 371,480,328 | +2,506,072 (+0.68%) |
| run | 2m48.25s | 2m49.03s | +0.79 s (+0.47%) |
| prepare (the freeze) | 167.7 ms | 190.6 ms | +22.9 ms |
| shadow statements | — | 3,007, 881.7 ms | +0.52% of the run |

The document bytes barely move because the one extra `file_operations` statement — the
`entry_count` update at freeze — runs before the document gains its entries, so it records the
1.8 KB admitted document. The real cost is one small row write per entry transition (three per
entry: `BeginEntry`, `SaveEntry started`, `SaveEntry applied`; 0.29 ms average per statement),
and the owner lock is held for that much longer. That is the cost §2.7 named: Release A must not
be kept for longer than the release after it.

**Backfill.** `sql/050` is one `DO` loop: at most 500 operations per iteration, locked `for
update skip locked`, moved in one `jsonb_to_recordset` insert, with `entry_count` set in the same
iteration. The DDL is a separate file so the ACCESS EXCLUSIVE lock from `add column` is not held
for the backfill; the backfill itself takes only ACCESS SHARE on `file_operations` and never
waits on a row a live worker holds. `on conflict do nothing` plus the "no rows yet" target filter
make a re-run a no-op. A file is one transaction (§2.5, sql/044), so an interrupted run rolls
every batch back: the schema stays, the next `eam migrate` re-runs the file from the start, and
per-batch progress is deliberately not kept. The scope is bounded by the journal's retention.

**Disagreement detection.** The exact detector is `TestShadowEntriesMatchDocument`: after a real
`Prepare` + `Run` it rebuilds each row as the document's entry object and compares it with `is
distinct from`; it fails on a single differing field and on a count that disagrees with
`entry_count`. `TestJournalEntriesBackfill` and its interruption variant hold the migration to the
same comparison. The gross detector for a live journal is in `sql/050`'s header (table row count
vs `entry_count`). A row written by a pre-Release-A binary during the deploy window may have a
stale or missing shadow; that is safe in Release A (nothing reads it) and Release B's Claim-time
upgrade overwrites it from the document, which is why §2.5's upgrade overwrites rather than
inserts-and-ignores.

**Read guard.** Release A's `TestJournalEntriesTableIsWriteOnly` required that no production
source reads the table; Release B inverted it. `TestEntriesTableReadsStayInSettlement` now
requires that (a) every statement naming the table stays in `postgres_entries.go`, (b) the one
ordered manifest load (`loadEntries`) is called from `Settle` and from nowhere in `postgres.go`
— the file that implements `Get`/`readOperation`, which is exactly where the hoist §2.7 forbids
would land — and (c) `Claim` upgrades a document-backed row and admission writes new rows as
`'table'`. It is textual, in the style of `TestOperationJournalPredicatesReadColumns`, because
the spellings it forbids are exactly the ones such a refactor is typed as.

The sql/047 predicate lint's allowance for `snapshot->'Entries'` narrowed with the flip:
production Go may no longer read the key at all; only test fixtures (which must build the old
document shape to prove the upgrade) and `sql/050` (the backfill) still may.

**Release B is implemented** (§2.3-2.5): settlement loads the entries inside its transaction;
the per-transition questions are row predicates or one-row reads; `Claim` upgrades a
`'document'` row; new operations are written as `'table'`; `mkdir` and
`CommitNativeTreeMove` read `entry_count` (or the list the settlement passed) instead of a
document; and progress is a count query (`Postgres.EntryProgress`) rather than a load.


## 5. Release B: implemented, and the measured payoff

The flip is the change §1.4 justified: a new operation is `entries_layout='table'`, the
document carries no `Entries`, one entry's transition is one row update plus the ~1.8 KB
document, settlement loads the rows inside its own transaction (§2.3), a `'document'` row is
upgraded on `Claim` (§2.5), and progress is `entry_count` plus one count query.
`TestEntriesTableReadsStayInSettlement` is the inverted read guard (§4): it fails if the
settlement load moves out of `Settle` or if a new statement names the table outside
`postgres_entries.go`. `sql/051` is the schema comment that records the forward-only decision
next to the column.

`FILES=500 make measure-journal`, same machine and fixture as §1.2 and §4:

| | Release A | Release B | delta |
|---|---:|---:|---:|
| journal writes | 5,018 | 5,018 | 0 |
| logical document bytes | 5,248,100,046 | **9,452,419** | −5,238,647,627 (−99.82%) |
| WAL | 371,480,328 | 6,165,712 | −365,314,616 (−98.34%) |
| run | 2m49.03s | **12.76s** | −156.27 s (−92.45%) |
| prepare (the freeze) | 190.6 ms | 158.3 ms | −32.3 ms |
| peak document | 1,363,536 B | 149,754 B | −89.0% |

The projection is no longer quadratic in practice: document bytes per rewrite went from
~1.36 MB to ~1.8 KB, and the 5,010 per-entry transitions wrote 9.14 MB together instead of
5.24 GB. The row work that replaced it is visible in the timing pass: 12,029 statements
against `file_operation_entries` (row writes at freeze, one conditional update at
`BeginEntry`, one update and one read at `SaveEntry`, the one-row accessor, and the
settlement load), 2.84 s = 22.3% of the 12.76 s run. Document writes fell to 1.25 s (9.8%) and
locked document reads to 1.04 s.

Two numbers deserve their reason rather than a footnote:

- **The remaining peak document is 149,754 B, and it is not entries.** It is the settled
  operation's aggregated `NativeResult.Evidence` — one JSON array of every applied entry's
  result, which is the operation's final answer and has to be O(N) once. It is written once at
  `SaveActionApplied` and once at settlement, not on every transition.
- **The write count is unchanged (5.01 per entry).** Release B changes what one write costs,
  not how many there are: the parent document is still rewritten on every transition, because
  §1.4's contract — a document reads as a whole, and its columns are derived from it in the
  same statement — was deliberately kept. Rewriting 1.8 KB is not the problem; rewriting
  10,002 entries was.

What still supports both layouts, and why:

- `Get`, `readOperationSnapshot` and the pending/recovery scans decode whichever the row's
  `entries_layout` names: a terminal `'document'` row keeps its list and serves progress from
  it until prune removes it (with any shadow rows by `ON DELETE CASCADE`).
- `Claim` is the only normal-operation flip. It runs inside the transaction that already holds
  the parent row lock, overwrites any stale Release A shadow from the document (the authority
  for that layout), sets `entry_count`, and strips the document before the claim proceeds.
- `Settle` loads from the row only when the layout says `'table'`; a `'document'` row still
  settles from its decoded list, exactly as before.
- Prune, retention and recovery never branch on layout: they are about the parent row and its
  state, and entry rows follow the parent.

Rollback is **forward-only** (§2.6): the drain procedure is named there and in `sql/051`'s
column comment, because an old binary would fail `invalid_intent` on every `'table'` row and
leave its guards behind.
