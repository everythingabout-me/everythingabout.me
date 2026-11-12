-- The migration runner stops replaying every file and records what it applied.
--
-- Why this exists: `eam migrate` used to run all of sql/*.sql in one transaction on every
-- invocation. That was append-only in intent but not in effect. 035 creates a globally
-- unique index on storage_connections(name); 036 drops it and replaces it with per-owner
-- partial indexes. Once two owners both had a connection named "nas", the next replay ran
-- 035 again, tried to recreate the global unique index, and failed -- the database could no
-- longer be migrated at all. Editing 035 is not an option because migrations are append-only.
--
-- From here on a file executes only when its base name is not recorded. `name` is that base
-- name, so the runner compares directory and history by the same string; `applied_at` is when
-- this database recorded it; `method` answers "did this file really run, or was it adopted?"; `reason`
-- records why an adopted row was not executed, and the check below makes a baseline row
-- without a reason impossible.
--
-- Bootstrap rule (implemented in backend/cmd/eam/migrate.go): a database that predates this
-- table already has the schema but has no history. Its first run finds schema_migrations
-- empty while the core `nodes` table from 001_init.sql exists. The old runner ran all files
-- in one transaction, so a database last migrated by a release containing 001-043 has all of
-- them and no partial state; one last migrated by an older release could be behind, and this
-- history-less rule cannot tell where it stopped. The database carries no per-file record to
-- recover, and replaying the old files is exactly the 035/036 failure above. The runner
-- therefore records every file that sorts before this one as method='baseline' without
-- executing it. A genuinely empty database has no `nodes` table,
-- so every file runs in order and is recorded as method='executed'. Once any row exists,
-- this table is the authority and only names without a row run.
--
-- The bootstrap boundary is why this file is 044: everything numbered below it was already
-- replayed by the old runner; everything from 044 on has never run. If the version table is
-- ever lost after 044, the same rule adopts 001-043 and re-executes 044 onward, which is why
-- new files must stay cheaply re-runnable.
create table if not exists schema_migrations (
  name       text primary key,
  applied_at timestamptz not null default now(),
  method     text not null check (method in ('executed', 'baseline')),
  reason     text,
  check (method <> 'baseline' or reason is not null)
);
