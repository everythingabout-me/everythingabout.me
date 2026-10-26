-- 对外 API 的访问令牌（Bearer token）。
--
-- 一个 token 绑定到一个用户，并可以限制：
--   * prefix    —— 只能访问 drive/<uid>/<prefix>/ 这棵子树（空表示整个盘）
--   * can_read / can_write —— 只读、读写
-- 库里只存 secret 的 SHA-256；token_id 是明文，用来定位这一行并在界面上认出是哪一把。

create table if not exists api_tokens (
  id           uuid primary key default gen_random_uuid(),
  token_id     text not null unique,              -- eam_<token_id>_<secret> 里的中间那段
  token_hash   text not null,                     -- sha256(secret) 的十六进制
  user_id      uuid not null references users(id) on delete cascade,
  name         text not null,                     -- 给用户看的名字，如 "Obsidian"
  prefix       text not null default '',          -- 逻辑路径前缀，空 = 整个盘
  can_read     boolean not null default true,
  can_write    boolean not null default true,
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  expires_at   timestamptz,
  revoked_at   timestamptz
);

create index if not exists api_tokens_user on api_tokens (user_id);
