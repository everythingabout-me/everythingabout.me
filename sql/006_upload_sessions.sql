-- 大文件直传的“上传会话”。
--
-- 流程：POST /upload 建一条会话并下发预签名 URL → 客户端把字节直接 PUT 到 COS
--      → POST /upload/complete 校验后落索引。
-- 会话本身不存字节，只存“谁、往哪写、多大、什么时候过期”。

create table if not exists upload_sessions (
  id           text primary key,                 -- u_<24 位十六进制>
  user_id      uuid not null references users(id) on delete cascade,
  path         text not null,                    -- 逻辑路径（NFC）
  size         bigint not null,
  mime         text not null default '',
  part_size    bigint not null default 0,        -- 单分片时等于 size
  created_at   timestamptz not null default now(),
  expires_at   timestamptz not null,
  completed_at timestamptz,
  cancelled_at timestamptz
);

create index if not exists upload_sessions_user on upload_sessions (user_id);
create index if not exists upload_sessions_expires on upload_sessions (expires_at);
