-- The cleanup sweep runs several times a second while an import settles, and it asks the
-- journal a question it could not previously index: which finished upload operations still
-- have staged parts to discard. The state and the cleanup mark both live inside the snapshot
-- document, so the question was a sequential scan of every operation row, sorted, to find the
-- few candidates — measured at 114,766 buffer hits and ~259 ms per call against 15k rows, on
-- a pool connection competing with the import it was cleaning up after.
--
-- Partial, because only an uncleaned terminal operation is a candidate; the index stays the
-- size of the backlog rather than of the journal, which is what makes it acceptable on a table
-- that keeps every terminal row for replay.
--
-- The keys are the filter alone: the sweep's ORDER BY casts a JSON text to timestamptz, which
-- is stable rather than immutable and so cannot be indexed. It sorts the handful of rows the
-- index returns, which costs nothing at that size.
create index if not exists file_operations_cleanup on file_operations (id)
 where action='upload_commit' and snapshot->>'State' in ('succeeded','failed','cancelled')
   and (snapshot->'CleanedAt' is null or snapshot->'CleanedAt'='null'::jsonb);
