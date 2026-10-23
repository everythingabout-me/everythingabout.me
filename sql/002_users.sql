-- 用户系统：账号 + 登录会话 + 文件归属
--
-- 身份模型：
--   users.id  是稳定的唯一用户 ID（UUID），所有外键都指向它；
--   users.email 只是可变的登录标识，改邮箱时 id 不变。
-- 不存用户名；以后要昵称就加 display_name，不影响 id 和引用关系。

create table if not exists users (
  id               uuid primary key default gen_random_uuid(),
  email            text not null,                    -- 统一小写存储
  password_hash    text not null,                    -- scrypt$N$r$p$salt$hash
  created_at       timestamptz not null default now(),
  updated_at       timestamptz not null default now(),
  email_changed_at timestamptz,
  last_login_at    timestamptz
);

-- 邮箱大小写不敏感的唯一约束（写入时也会归一化，双保险）
create unique index if not exists users_email_lower_uniq
  on users (lower(email));

-- 会话：cookie 里只放随机 token，库里存它的 SHA-256
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

-- 文件归属。可空是为了容纳多用户改造前的旧记录：
-- 旧数据 owner_id 为空，跑 `npm run claim -- --email 你@example.com` 认领。
alter table nodes
  add column if not exists owner_id uuid references users(id) on delete cascade;

-- 路径唯一性从"全局唯一"改成"每个用户各自唯一"
drop index if exists nodes_path_live_uniq;
create unique index if not exists nodes_owner_path_live_uniq
  on nodes (owner_id, path) where deleted_at is null;

-- 还没认领的旧数据（owner_id 为空）保持和改造前一样的全局路径唯一
create unique index if not exists nodes_legacy_path_uniq
  on nodes (path) where deleted_at is null and owner_id is null;

create index if not exists nodes_owner_parent_live
  on nodes (owner_id, parent) where deleted_at is null;

create index if not exists nodes_owner on nodes (owner_id);
