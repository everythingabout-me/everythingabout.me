-- "Upload sessions" for streaming large files straight to storage.
--
-- Flow: POST /upload creates a session and hands out a presigned URL -> the client PUTs the bytes
--       straight to COS -> POST /upload/complete verifies them and writes the index.
-- A session stores no bytes, only "who, writing where, how large, and when it expires".

create table if not exists upload_sessions (
  id           text primary key,                 -- u_<24 hex chars>
  user_id      uuid not null references users(id) on delete cascade,
  path         text not null,                    -- logical path (NFC)
  size         bigint not null,
  mime         text not null default '',
  part_size    bigint not null default 0,        -- equals size for a single part
  created_at   timestamptz not null default now(),
  expires_at   timestamptz not null,
  completed_at timestamptz,
  cancelled_at timestamptz
);

create index if not exists upload_sessions_user on upload_sessions (user_id);
create index if not exists upload_sessions_expires on upload_sessions (expires_at);
