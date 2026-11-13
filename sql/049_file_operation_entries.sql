-- Release A of docs/design/journal-entries.md: one row per frozen manifest entry.
--
-- The measured problem this table exists for (docs/design/journal-entries.md §1.2): every
-- operation transition rewrites the whole `file_operations.snapshot` document, and the document
-- carries every manifest entry. A 500-file move performs 5,017 transitions and writes 5,248 MB
-- of JSON while the same transitions without `Entries` write 9.45 MB — entries are 99.8% of the
-- volume, and the cost grows with the square of the entry count because both the document and
-- the write count grow with it. At 5,000 files the frozen document alone is 8.18 MB.
--
-- The fix is a row per entry, so one entry's transition writes one row instead of rewriting the
-- other 10,001. That is Release B's change. **This file is Release A: schema only.**
--
-- Release A is additive and rollback-safe on purpose. It adds the table, backfills it
-- (sql/050), and the Release A binary writes the table as a *shadow* while the operation
-- document stays complete and authoritative for every read and for settlement. A previous
-- binary can therefore run against this schema unchanged: it reads and writes the same
-- documents it always did and ignores the columns and the table. Flipping the layout to
-- 'table' and removing `Entries` from documents is Release B, and it is a separate release
-- because a previous binary reading a 'table' row would see an operation with no entries (see
-- §2.6).
--
-- What is deliberately not here:
--
--   * No reader. Nothing in this release reads `file_operation_entries` for behaviour. The
--     only reader is the backfill in sql/050 (a one-time move of existing documents), and the
--     shadow-verification tests. TestJournalEntriesTableIsWriteOnly fails if production code
--     starts reading the table before Release B.
--   * No CHECK on `state` or `action`: this release writes what the document already says, so
--     a vocabulary change must not fail a live operation's transition transaction on a schema
--     that is one release behind. The vocabulary is still fixed in one place in Go
--     (postgres_transitions.go), and the shadow-agreement test compares every field against
--     the document.
--   * No partial index on `state`: unlike the journal, these rows are deleted with their
--     parent by ON DELETE CASCADE, so the table never accumulates the settled rows a partial
--     index would have to skip. The one cross-operation query (recovery: "does this operation
--     still have an entry that may have landed?") is answered per operation, which the primary
--     key already covers.
--
-- Why this file is DDL only. `alter table ... add column` takes ACCESS EXCLUSIVE on
-- file_operations, and that lock is held until this file's transaction commits. The backfill
-- reads every retained operation document and can take a while on a large journal, so putting
-- it in this file would hold the exclusive lock for the whole backfill and stop every journal
-- read and write for that time. Two files are two transactions: the exclusive lock is held for
-- the two metadata-only adds below, and the backfill then runs under ACCESS SHARE, which
-- blocks only DDL. `eam migrate` applies each file in its own transaction (sql/044).

-- One row per frozen manifest entry. The parent `file_operations` row stays the operation
-- document (intent, result, guards, reservation, problem, purge state), minus `Entries` in
-- Release B.
--
-- Why a row and not a document: every entry has its own durable state (pending -> started ->
-- applied/not_applied/outcome_unknown) that is written 4-5 times, and every write of one
-- entry's state must not rewrite the other 10,001.
create table if not exists file_operation_entries (
    owner_id       uuid    not null references users(id),
    operation_id   uuid    not null references file_operations(id) on delete cascade,
    -- The entry's manifest number. It is already the identity used by
    -- file_operation_checkpoints.entry_number, file_entry_charges.entry_number and the change
    -- fact key "<operation>:entry:<number>", so it is not a new ordering: it is the existing
    -- one made a key. It is assigned once at freeze and never renumbered.
    number         integer not null,
    action         text    not null,   -- mkdir | copy | move | delete
    source_path    text    not null,   -- "" for mkdir
    target_path    text    not null,   -- "" for delete
    source_revision bytea,             -- captured native revision of the source
    -- The captured storage.Entry this effect was admitted against. It is jsonb because no SQL
    -- predicate ever reads into it: it is decoded only by the settlement that validates the
    -- effect, exactly as it was decoded from the document. Keeping it as a document inside the
    -- row is deliberate and does not reintroduce the sql/047 problem (a predicate over a
    -- document), because there is no predicate.
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

-- Owner isolation: owner_id is denormalized from the parent. The only writer copies both from
-- the same parent row it already locked, so a row cannot name a different tenant, and every
-- query can filter the tenant without joining the journal. The index makes owner-scoped
-- cleanup and accounting (prune, future per-owner reports) not depend on the join.
create index if not exists file_operation_entries_owner on file_operation_entries(owner_id);

-- The parent gains two immutable columns so callers that only need a count never read the
-- table:
--
--   entry_count     the number of manifest entries, set once when the manifest freezes
--                   (`FreezeAction` is the only writer; entries are never added after a freeze,
--                   so it cannot drift). It exists because the progress endpoint reports
--                   `total = len(Entries)`, admission bounds a manifest by its length, and
--                   mkdir asks whether the manifest is empty — all values the freeze already
--                   knows. It is authoritative only when entries_layout='table'; a
--                   'document' row's count is in its document.
--   entries_layout  'document' = entries are in snapshot->'Entries' (every row in Release A,
--                   including rows written by the previous release during the deploy window);
--                   'table' = entries are in file_operation_entries and the document carries
--                   none. Readers branch on this column, never on whether the table happens to
--                   have rows. The default keeps every existing row document-backed, which is
--                   what makes Release A rollback-safe.
alter table file_operations add column if not exists entry_count integer not null default 0;
alter table file_operations add column if not exists entries_layout text not null default 'document';
