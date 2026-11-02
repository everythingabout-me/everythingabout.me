-- Temporary legacy browser upload intents. No bytes or provider credentials live
-- here. Completed receipts leave the recovery query and expire independently.
create table if not exists browser_uploads (
  id text primary key,
  binding text not null,
  owner_id uuid not null references users(id) on delete cascade,
  path text not null check(path <> ''),
  size bigint not null check(size >= 0),
  mime text not null default '',
  modified_at bigint not null default 0,
  state text not null default 'open'
    check(state in ('open','receiving','staged','publishing','applied','completed','aborting','aborted','unknown')),
  checkpoint jsonb not null default '{}',
  result jsonb,
  expires_at timestamptz not null,
  lease_token text,
  lease_until timestamptz,
  next_check_at timestamptz not null,
  last_error text not null default '',
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  retain_until timestamptz
);
create unique index if not exists browser_uploads_pending_path
  on browser_uploads(binding,owner_id,path)
  where state not in ('completed','aborted');
create index if not exists browser_uploads_due
  on browser_uploads(binding,next_check_at)
  where state not in ('completed','aborted');
create index if not exists browser_uploads_owner on browser_uploads(owner_id);
create index if not exists browser_uploads_receipt_cleanup
  on browser_uploads(binding,retain_until)
  where state in ('completed','aborted');
