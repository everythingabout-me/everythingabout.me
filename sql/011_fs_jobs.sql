-- Background jobs for directory moves and copies.
--
-- There is no "rename in one shot" primitive for a directory: the key is the path, so moving N
-- objects costs N server-side copies. This table is server-side only: there is no public "job"
-- concept, and the progress URL is the action's own subpath (/move/{id}, /copy/{id}), so one op
-- column is enough to tell the two actions apart.

do $$
begin
  if to_regclass('public.move_jobs') is not null
     and to_regclass('public.fs_jobs') is null then
    alter table move_jobs rename to fs_jobs;
  end if;
end $$;

create table if not exists fs_jobs (
  id                  text primary key,                -- m_<24 hex chars> / c_<24 hex chars>
  user_id             uuid not null references users(id) on delete cascade,
  op                  text not null default 'move',    -- move / copy
  from_path           text not null,
  to_path             text not null,
  status              text not null default 'pending', -- pending / running / done / failed / cancelled
  total               int  not null default 0,
  done                int  not null default 0,
  error               text,
  cancel_requested_at timestamptz,
  created_at          timestamptz not null default now(),
  started_at          timestamptz,
  finished_at         timestamptz
);

-- Renamed from move_jobs on old databases; backfill the new columns.
alter table fs_jobs add column if not exists op text not null default 'move';
alter table fs_jobs add column if not exists cancel_requested_at timestamptz;

-- Rename the indexes along with the table so each migration does not create another set.
-- Rename the indexes along with the table so each migration does not create another set.
-- 008 recreates move_jobs every time, so only rename when the target index is still missing.
do $$
begin
  if to_regclass('public.move_jobs_user_time') is not null
     and to_regclass('public.fs_jobs_user_time') is null then
    alter index move_jobs_user_time rename to fs_jobs_user_time;
  end if;
  if to_regclass('public.move_jobs_active') is not null
     and to_regclass('public.fs_jobs_active') is null then
    alter index move_jobs_active rename to fs_jobs_active;
  end if;
end $$;

create index if not exists fs_jobs_user_time on fs_jobs (user_id, created_at desc);
create index if not exists fs_jobs_active on fs_jobs (status) where status in ('pending', 'running');

-- 008 re-creates move_jobs on every migrate (if not exists).
-- If it is already an empty shell, drop it rather than leave a dead table in the database.
do $$
begin
  if to_regclass('public.fs_jobs') is not null
     and to_regclass('public.move_jobs') is not null then
    if not exists (select 1 from move_jobs limit 1) then
      drop table move_jobs;
    end if;
  end if;
end $$;
