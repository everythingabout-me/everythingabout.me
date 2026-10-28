-- Access tokens (Bearer tokens) for the public API.
--
-- A token binds to one user and can be restricted:
--   * prefix    -- only the drive/<uid>/<prefix>/ subtree is reachable (empty = the whole drive)
--   * can_read / can_write -- read-only, or read and write
-- Only the SHA-256 of the secret is stored; token_id is plaintext, used to find this row and to
-- tell tokens apart in the UI.

create table if not exists api_tokens (
  id           uuid primary key default gen_random_uuid(),
  token_id     text not null unique,              -- the middle segment of eam_<token_id>_<secret>
  token_hash   text not null,                     -- hex of sha256(secret)
  user_id      uuid not null references users(id) on delete cascade,
  name         text not null,                     -- user-facing name, e.g. "Obsidian"
  prefix       text not null default '',          -- logical path prefix; empty = the whole drive
  can_read     boolean not null default true,
  can_write    boolean not null default true,
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  expires_at   timestamptz,
  revoked_at   timestamptz
);

create index if not exists api_tokens_user on api_tokens (user_id);
