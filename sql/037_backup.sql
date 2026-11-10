-- A backup is a user setting, not a constant: how often to snapshot and how many
-- snapshots to keep belong to the account. The run log is operator-visible state
-- only; losing it costs a status display, never a snapshot.
create table if not exists backup_policies (
  owner_id      uuid not null references users(id) on delete cascade,
  connection_id uuid not null references storage_connections(id) on delete cascade,
  schedule_hours integer not null default 24 check (schedule_hours between 1 and 720),
  keep          integer not null default 14 check (keep between 1 and 365),
  enabled       boolean not null default true,
  updated_at    timestamptz not null default now(),
  primary key (owner_id, connection_id)
);

create table if not exists backup_runs (
  id            uuid primary key default gen_random_uuid(),
  owner_id      uuid not null references users(id) on delete cascade,
  connection_id uuid not null references storage_connections(id) on delete cascade,
  snapshot      date not null,
  state         text not null check (state in ('running', 'complete', 'failed')),
  files         integer not null default 0,
  folders       integer not null default 0,
  bytes         bigint not null default 0,
  error         text,
  started_at    timestamptz not null default now(),
  finished_at   timestamptz
);

create index if not exists backup_runs_owner_started on backup_runs(owner_id, started_at desc);
create unique index if not exists backup_runs_one_per_day on backup_runs(owner_id, connection_id, snapshot);
