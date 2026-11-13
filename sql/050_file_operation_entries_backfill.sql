-- Backfill file_operation_entries from the operation documents that already carry entries.
--
-- This is the data half of Release A of docs/design/journal-entries.md (§2.5). sql/049 added
-- the table and the columns; this file copies every existing operation's `snapshot->'Entries'`
-- into it and sets `entry_count` on the parent. It is read-only with respect to the operation
-- documents: it never rewrites a snapshot and never flips `entries_layout`, which is what keeps
-- Release A rollback-safe — a previous binary sees exactly the journal it left.
--
-- Why the DDL is a separate file: `alter table` takes ACCESS EXCLUSIVE on file_operations, and a
-- transaction holds that lock until it commits. With the backfill in the same transaction the
-- exclusive lock would be held for the whole backfill, stopping every journal read and write.
-- Here the DDL has committed already, and this transaction takes only ACCESS SHARE on
-- file_operations (which blocks DDL, not ordinary reads or writes) and RowExclusive on the new
-- table.
--
-- How it is batched. The loop takes at most `batch` operations per iteration, locks them
-- `for update skip locked`, and moves their entries in one insert. Batching bounds two things
-- that one statement over the whole journal would not: how much JSON is detoasted and parsed in
-- a single statement, and how many parent rows are locked at once. `skip locked` is what keeps
-- it from waiting on a row a live worker is currently transitioning: a skipped row is simply
-- picked up on a later iteration when it is free. If it is never free during this run, it keeps
-- `entry_count = 0` and no rows; that is safe in Release A (nothing reads the table) and Release
-- B repairs it on `Claim` by re-reading the document, which is the authority for a
-- 'document' row.
--
-- Idempotency and interruption. `on conflict (operation_id, number) do nothing`, plus the
-- target filter (`no rows in the table yet`), makes a second run insert nothing. The migration
-- runner executes a file in one transaction (sql/044), so an interrupted run rolls back every
-- batch: the table and `entry_count` are exactly as they were, the schema from sql/049 stays,
-- and the next `eam migrate` re-runs this file from the first batch. Progress from a partially
-- completed run is therefore not kept — the trade for one atomic migration — which is
-- acceptable because the backfill's scope is bounded by the journal's retention: it is the
-- documents the journal still keeps, not the history it has pruned.
--
-- How a shadow that fell behind is detected. The exact detector is a test:
-- TestShadowEntriesMatchDocument rebuilds each entry from the table and compares it to the
-- document with `is distinct from`, after driving a real Prepare + Run, and it fails if any
-- field differs. The gross detector an operator can run on a live journal is the same shape:
--
--   select o.id, o.entry_count, count(e.operation_id) as table_rows
--     from file_operations o
--     left join file_operation_entries e on e.operation_id = o.id
--    where o.entries_layout = 'document'
--    group by o.id, o.entry_count
--   having count(e.operation_id) <> o.entry_count;
--
-- A row whose per-entry content disagrees while the count agrees is not produced by this
-- release: every shadow write is in the same transaction as the document write that matches it
-- (see postgres_entries.go), so a crash rolls both back. Such a row can only come from a
-- binary that does not shadow (the deploy window between `eam migrate` and the restart) or from
-- corruption, and Release B's Claim-time upgrade overwrites it from the document.

do $$
declare
    -- One operation can carry up to maxActionEntries (100,000) entries, so the chunk size
    -- bounds operations, not entries: the work is proportional to rows either way. 500 keeps
    -- one iteration's detoasting and lock set small on a journal that kept many operations.
    batch constant integer := 500;
    moved integer;
    operations integer := 0;
begin
    loop
        with target as (
            select o.id
              from file_operations o
             where o.entries_layout = 'document'
               -- The CASE is load-bearing: `jsonb_typeof(...) = 'array' and
               -- jsonb_array_length(...) > 0` is reorderable, and a document whose
               -- Entries is JSON null (a nil Go slice marshals to null) made the
               -- length call abort the whole migration on a database that had rows.
               and (case when jsonb_typeof(o.snapshot->'Entries') = 'array'
                         then jsonb_array_length(o.snapshot->'Entries') else 0 end) > 0
               and not exists (
                   select 1 from file_operation_entries e where e.operation_id = o.id)
             order by o.id
             limit batch
             for update skip locked
        ), inserted as (
            insert into file_operation_entries
                (owner_id, operation_id, number, action, source_path, target_path,
                 source_revision, source_entry, target_absent, target_revision, size, state,
                 checkpoint, evidence, result)
            select o.owner_id, o.id, (e->>'Number')::int, e->>'Action',
                   coalesce(e->>'Source', ''), coalesce(e->>'Target', ''),
                   case when e ? 'SourceRevision'
                        then decode(e->>'SourceRevision', 'base64') end,
                   nullif(e->'SourceEntry', 'null'::jsonb),
                   coalesce((e->'TargetConditions'->>'Absent')::boolean, false),
                   case when e->'TargetConditions' ? 'ExpectedRevision'
                        then decode(e->'TargetConditions'->>'ExpectedRevision', 'base64') end,
                   coalesce((e->>'Size')::bigint, 0),
                   coalesce(e->>'State', 'pending'),
                   case when e ? 'Checkpoint'
                        then decode(e->>'Checkpoint', 'base64') end,
                   case when e ? 'Evidence'
                        then decode(e->>'Evidence', 'base64') end,
                   nullif(e->'Result', 'null'::jsonb)
              from target t
              join file_operations o on o.id = t.id
              cross join lateral jsonb_array_elements(o.snapshot->'Entries') e
            on conflict (operation_id, number) do nothing
        )
        update file_operations o
           set entry_count = case when jsonb_typeof(o.snapshot->'Entries') = 'array'
                                  then jsonb_array_length(o.snapshot->'Entries') else 0 end
          from target t
         where o.id = t.id;

        get diagnostics moved = row_count;
        exit when moved = 0;
        operations := operations + moved;
    end loop;
    raise notice 'file_operation_entries backfill: % operation(s) moved from their documents', operations;
end $$;
