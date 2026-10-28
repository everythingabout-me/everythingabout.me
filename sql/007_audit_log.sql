-- Audit log for the public API: who, with which token, on which path, doing what, and the result.
-- Only writes are recorded (PUT / DELETE / MOVE / UPLOAD); reads are skipped to keep the log small.

create table if not exists audit_log (
  id         bigserial primary key,
  user_id    uuid not null references users(id) on delete cascade,
  token_id   text,                                  -- token ID; empty for cookie sessions
  action     text not null,                         -- put / delete / move / upload / upload_complete
  path       text,                                  -- logical path involved
  status     int not null,                          -- HTTP status code
  ip         text,
  user_agent text,
  detail     jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);

create index if not exists audit_log_user_time on audit_log (user_id, created_at desc);
