-- Journal permanent browser deletions before hiding their index rows. The captured
-- keys survive restart; unfinished native requests retain the path reservation.
alter table fs_jobs add column if not exists delete_binding text not null default '';
alter table fs_jobs add column if not exists next_attempt_at timestamptz not null default now();

create table if not exists fs_delete_entries (
  job_id text not null references fs_jobs(id) on delete cascade,
  owner_id uuid not null references users(id) on delete cascade,
  path text not null,
  key text not null,
  is_dir boolean not null,
  etag text,
  size bigint not null,
  state text not null default 'pending' check (state in ('pending','removing','done')),
  primary key (job_id, path)
);
create index if not exists fs_delete_entries_remaining
  on fs_delete_entries (job_id, state);
create index if not exists fs_jobs_delete_pending
  on fs_jobs (user_id, delete_binding) where op='delete' and status<>'done';
