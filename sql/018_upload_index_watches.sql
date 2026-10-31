-- Recover index entries for legacy direct uploads when browser confirmation is lost.
-- Watches contain no file bytes or credentials and remain bound to the original
-- storage configuration. Credential expiry changes polling frequency, not native
-- publication outcome: confirmed and expired paths remain observable indefinitely.

create table if not exists upload_index_watches (
  binding          text not null,
  owner_id         uuid not null references users(id) on delete cascade,
  path             text not null check (path <> ''),
  is_dir           boolean not null default false,
  issued_until     timestamptz not null,
  settled          boolean not null default false,
  generation       bigint not null default 1,
  lease_version    bigint not null default 0,
  lease_until      timestamptz,
  next_check_at    timestamptz not null default now(),
  checked_at       timestamptz,
  last_success_at  timestamptz,
  last_fingerprint text not null default '',
  retry_count      integer not null default 0,
  last_error       text not null default '',
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  primary key (binding, owner_id, path, is_dir)
);

create index if not exists upload_index_watches_due
  on upload_index_watches (binding, next_check_at);
create index if not exists upload_index_watches_owner
  on upload_index_watches (owner_id);
