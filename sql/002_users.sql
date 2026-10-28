-- User system: accounts, login sessions and file ownership
--
-- Identity model:
--   users.id is the stable unique user ID (UUID); every foreign key points at it;
--   users.email is just a mutable login identifier, so changing it leaves the id unchanged.
-- No username is stored; add display_name later if a nickname is needed, without touching the id
-- or the references.

create table if not exists users (
  id               uuid primary key default gen_random_uuid(),
  email            text not null,                    -- always stored lowercased
  password_hash    text not null,                    -- scrypt$N$r$p$salt$hash
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  email_changed_at timestamptz,
  last_login_at    timestamptz
);

-- Case-insensitive unique constraint on email (writes normalize too, belt and braces)
create unique index if not exists users_email_lower_uniq
  on users (lower(email));

-- Sessions: the cookie holds only a random token; the database stores its SHA-256
create table if not exists sessions (
  id           uuid primary key default gen_random_uuid(),
  token_hash   text not null unique,
  user_id      uuid not null references users(id) on delete cascade,
  created_at   timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  expires_at   timestamptz not null,
  user_agent   text,
  ip           text
);

create index if not exists sessions_user on sessions (user_id);
create index if not exists sessions_expires on sessions (expires_at);

-- File ownership. Nullable to accommodate records from before the multi-user migration:
-- old rows have an empty owner_id; claim them with `npm run claim -- --email you@example.com`.
alter table nodes
  add column if not exists owner_id uuid references users(id) on delete cascade;

-- Path uniqueness moves from global to per-user
drop index if exists nodes_path_live_uniq;

-- deleted_at was dropped in 010; on re-run, create these indexes only if the column exists.
do $$
begin
  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'nodes' and column_name = 'deleted_at'
  ) then
    create unique index if not exists nodes_owner_path_live_uniq
      on nodes (owner_id, path) where deleted_at is null;

    -- Unclaimed legacy rows (null owner_id) keep the same global path uniqueness as before
    create unique index if not exists nodes_legacy_path_uniq
      on nodes (path) where deleted_at is null and owner_id is null;

    create index if not exists nodes_owner_parent_live
      on nodes (owner_id, parent) where deleted_at is null;
  end if;
end $$;

create index if not exists nodes_owner on nodes (owner_id);
