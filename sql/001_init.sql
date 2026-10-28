-- Personal data foundation: the file index table
-- Object bytes stay in COS; this table only carries the directory tree, attributes and search.

create extension if not exists pg_trgm;
create extension if not exists pgcrypto;

create table if not exists nodes (
  id            uuid primary key default gen_random_uuid(),
  key           text not null unique,              -- COS object key, e.g. drive/Photos/a.jpg
  path          text not null,                     -- logical path, e.g. Photos/a.jpg (NFC)
  parent        text not null default '',          -- parent directory path; '' at the root
  name          text not null,
  is_dir        boolean not null default false,
  size          bigint not null default 0,
  mime          text,
  etag          text,
  storage_class text,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  deleted_at    timestamptz,                       -- soft delete; non-null means it is trashed
  meta          jsonb not null default '{}'::jsonb -- tags / EXIF / custom attributes
);

-- Path uniqueness is set up per user in 002_users.sql; no global unique index here,
-- or re-running the migration would fail once two users own the same path.
--
-- 010 has already dropped deleted_at. So this SQL stays re-runnable (`make migrate` replays
-- every file from the top): indexes that depend on deleted_at are created only if the column
-- still exists.
do $$
begin
  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'nodes' and column_name = 'deleted_at'
  ) then
    create index if not exists nodes_parent_live
      on nodes (parent) where deleted_at is null;
    create index if not exists nodes_deleted
      on nodes (deleted_at) where deleted_at is not null;
  end if;
end $$;

-- Fuzzy filename search
create index if not exists nodes_name_trgm
  on nodes using gin (name gin_trgm_ops);

-- Filtering by tag / attribute
create index if not exists nodes_meta_gin
  on nodes using gin (meta jsonb_path_ops);

create index if not exists nodes_size on nodes (size);

-- Look up objects by key when emptying the trash
create index if not exists nodes_key on nodes (key);
