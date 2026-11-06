-- Upload sessions no longer advertise or enforce a per-session parallelism.
-- Concurrent part bodies are admitted by one process-wide receiver pool (half of
-- the database pool), so a client-chosen number could only ever describe a promise
-- the server does not need to keep. Drop the column instead of keeping a dead
-- not-null field; the drop is idempotent because make migrate re-runs every file.
alter table file_uploads drop column if exists max_parallelism;
