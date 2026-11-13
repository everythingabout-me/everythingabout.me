-- Index the operation journal for its retention prune.
--
-- `eam file-operations-prune` is the first reader of `RetainUntil`, which every op recorded
-- and nothing consumed, so terminal rows accumulated forever. The command deletes only
-- settled operations past their retention window and outside every record that still needs
-- them, one bounded batch per transaction. That guard reads three things this file indexes:
--
--   1. the candidate set — one owner's settled terminal operations, oldest retention first;
--   2. the change feed — a retained change fact names the operation that produced it, and a
--      client replaying that cursor must still be able to read the operation;
--   3. everything else (guards, credit claims, live upload sessions) already has its own
--      index, or is small enough that the candidate query's not-exists checks stay cheap.
--
-- Both indexes are partial: a prune's candidate set is the expired backlog, not the journal,
-- and a change fact that names an operation is a small subset of the feed. Without them the
-- prune would scan the whole journal and every retained fact for each batch.
--
-- The prune refuses to delete a candidate whose promoted columns disagree with its operation
-- document (sql/047). That check runs per candidate, after these indexes have selected it, so
-- it needs no index of its own.

-- The prune's candidate set: one owner's settled operations, oldest retention first. Partial,
-- so its size follows the backlog of expired rows rather than the journal, and terminal-only,
-- so a non-terminal operation can never be a candidate whatever its retention says.
create index if not exists file_operations_prunable on file_operations (owner_id, retain_until, id)
 where state in ('succeeded','failed','cancelled') and phase = 'settled';

-- A change fact names the operation that produced it, and the prune must never delete an
-- operation a retained fact still points at. Facts expire with the feed, so the lookup is
-- partial and small; without it the prune would scan all retained facts for every candidate.
create index if not exists file_changes_operation on file_changes (owner_id, operation_id)
 where operation_id <> '';
