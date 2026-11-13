-- Release B of docs/design/journal-entries.md: the per-entry record lives in
-- file_operation_entries, and the operation document no longer carries `Entries`.
--
-- This file is comments only. It adds no data migration because none is needed:
--
--   * a new operation is written with entries_layout='table' by the binary that created it;
--   * a non-terminal 'document' row is upgraded on its first Claim (upgradeEntries in
--     internal/fileops/postgres_entries.go), which re-reads that row's document — the
--     authority for its layout — and overwrites any Release A shadow;
--   * a terminal 'document' row is never upgraded: it cannot take another transition, and
--     file-operations-prune deletes it (and any entry rows, by ON DELETE CASCADE) when its
--     retention passes.
--
-- sql/050's backfill still exists and is still correct for a database upgraded from Release
-- A: it moves the rows of documents nothing may claim, and it is idempotent.
--
-- Why the column default stays 'document'. It is the safe failure mode: a writer that
-- forgets to set entries_layout produces a complete, readable document-backed row rather
-- than a row that claims the table and has no rows, so the bug is a slow operation instead
-- of a lost manifest. The writer sets 'table' explicitly (insertOperationSQL), then flips
-- nothing; the only flip is the Claim-time upgrade. The default also keeps every fixture
-- row that other packages insert directly (a document with no entries) readable exactly as
-- before.
--
-- FORWARD-ONLY — the accepted decision. An earlier binary cannot read a 'table' row: it
-- decodes the document, sees an operation with no entries, and a move or copy fails
-- settlement with invalid_intent while its guards keep fencing the source and destination
-- paths; the native effects it may have made are only adopted by a reconcile because the
-- index never learned about them. That is a stuck operation plus operator work, not a
-- rollback. Rolling this release back is therefore allowed only after draining the journal:
-- stop the writers, let every non-terminal operation settle (or end it through the explicit
-- recovery commands), confirm `select count(*) from file_operations where entries_layout =
-- 'table' and state not in ('succeeded','failed','cancelled')` is zero, and only then start
-- the older binary. There is no runtime downgrade path and no migration reverses it; the
-- drain is the whole rollback procedure (docs/design/journal-entries.md §2.6).
comment on column file_operations.entries_layout is
  'document = entries are in the operation document''s Entries key (rows written before Release B, and terminal rows still pruned with their document); table = entries are in file_operation_entries and the document carries none. Readers branch on this column, never on whether the table happens to have rows. Immutable after the manifest freezes; the only normal-operation flip is the Claim-time upgrade. FORWARD-ONLY: an older binary cannot settle a ''table'' row, so rolling back requires draining every non-terminal operation first (sql/051).';

comment on table file_operation_entries is
  'One row per frozen manifest entry (Release B of docs/design/journal-entries.md). The parent file_operations row is the operation document (intent, result, guards, reservation, problem, purge state) without Entries; these rows are the per-entry state and evidence the index settlement validates. Rows are written only in a transaction that already holds the parent row lock, and are deleted with it by ON DELETE CASCADE. Forward-only: see file_operations.entries_layout (sql/051).';
