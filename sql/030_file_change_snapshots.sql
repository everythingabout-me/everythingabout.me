-- Immutable indexed bootstrap baselines share a transaction with a confirmed
-- change head. Snapshot pagination never rereads a moving live directory tree.
create table if not exists file_change_snapshots (
  id uuid primary key default gen_random_uuid(),
  owner_id uuid not null references users(id) on delete cascade,
  principal_id text not null,
  root_namespace text not null,
  policy_fingerprint text not null,
  epoch uuid not null,
  head_position bigint not null check (head_position >= 0),
  selector_mode text not null check (selector_mode in ('id','path')),
  selector_id uuid,
  selector_path text,
  target_id uuid not null,
  anchor_path text not null,
  kind text not null check (kind in ('file','folder')),
  recursive boolean not null default false,
  entry_count integer not null check (entry_count between 1 and 100000),
  changes_cursor text not null,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null default now() + interval '24 hours',
  unique (owner_id,id),
  check ((selector_mode='id' and selector_id is not null and selector_path is null)
      or (selector_mode='path' and selector_id is null and selector_path is not null)),
  check (kind='folder' or not recursive)
);
create index if not exists file_change_snapshots_principal
  on file_change_snapshots(owner_id,principal_id,root_namespace,id);
create index if not exists file_change_snapshots_expiry on file_change_snapshots(expires_at);

-- Updating this owner/principal row fences concurrent admission snapshots. Under
-- repeatable read, an admission waiting on a newer generation must retry rather
-- than count against an older snapshot and exceed the active-resource limit.
create table if not exists file_change_snapshot_admissions (
  owner_id uuid not null references users(id) on delete cascade,
  principal_id text not null,
  generation bigint not null default 1,
  primary key(owner_id,principal_id)
);

create table if not exists file_change_snapshot_entries (
  owner_id uuid not null,
  snapshot_id uuid not null,
  ordinal integer not null check (ordinal between 0 and 99999),
  metadata jsonb not null check (jsonb_typeof(metadata)='object'),
  primary key (owner_id,snapshot_id,ordinal),
  foreign key (owner_id,snapshot_id) references file_change_snapshots(owner_id,id) on delete cascade
);

-- Entries have no update path: changing an indexed file never revises its saved
-- baseline. Deleting a snapshot explicitly removes all its materialized metadata.
create or replace function file_change_snapshot_entry_immutable()
returns trigger language plpgsql as $$
begin
  raise exception 'file change snapshot entries are immutable' using errcode='23514';
end $$;
drop trigger if exists file_change_snapshot_entry_immutable on file_change_snapshot_entries;
create trigger file_change_snapshot_entry_immutable before update on file_change_snapshot_entries
  for each row execute function file_change_snapshot_entry_immutable();
