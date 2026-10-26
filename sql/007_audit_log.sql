-- 对外 API 的审计日志：谁、用哪个令牌、对哪个路径、做了什么、结果如何。
-- 只记录写操作（PUT / DELETE / MOVE / UPLOAD），读操作不记，避免日志爆炸。

create table if not exists audit_log (
  id         bigserial primary key,
  user_id    uuid not null references users(id) on delete cascade,
  token_id   text,                                  -- 令牌 ID；cookie 会话为空
  action     text not null,                         -- put / delete / move / upload / upload_complete
  path       text,                                  -- 涉及的逻辑路径
  status     int not null,                          -- HTTP 状态码
  ip         text,
  user_agent text,
  detail     jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now()
);

create index if not exists audit_log_user_time on audit_log (user_id, created_at desc);
