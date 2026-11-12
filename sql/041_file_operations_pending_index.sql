-- The settlement scanner asks one question, on every worker slot it frees and on every
-- sweep: which operations are queued, running or outcome_unknown, oldest first. The state
-- lives inside the operation's snapshot document, so until now there was nothing to ask an
-- index: the scan read every operation row, tested the document, and sorted what survived.
-- At 16k rows that was 44,924 buffer hits per call, repeated while an import settles.
--
-- Partial, because an unsettled operation is the only candidate: a row leaves the index at
-- the moment it stops being pending, so its size follows the work in flight, not the journal
-- this table deliberately keeps for replay. That is what makes the maintenance cost
-- acceptable on a table whose whole point is to retain every terminal row.
--
-- The two keys are the scan's filter and its order (CreatedAt, then id), so the plan reads
-- the candidates in order and can stop at its page limit instead of sorting the journal.
create index if not exists file_operations_pending on file_operations ((snapshot->>'CreatedAt'), id)
 where snapshot->>'State' in ('queued','running','outcome_unknown');
