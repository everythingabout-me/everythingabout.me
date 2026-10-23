-- 邮箱验证码：目前用于注册，后续也可复用到改邮箱
--
-- 只存验证码的 scrypt 哈希，不存明文；验证码 10 分钟过期，
-- 最多尝试 5 次，发送新码时旧码自动作废。

create table if not exists email_codes (
  id          uuid primary key default gen_random_uuid(),
  email       text not null,
  purpose     text not null,              -- register / change_email
  code_hash   text not null,
  expires_at  timestamptz not null,
  attempts    integer not null default 0,
  consumed_at timestamptz,
  created_at  timestamptz not null default now(),
  ip          text
);

create index if not exists email_codes_lookup
  on email_codes (email, purpose, created_at desc);

create index if not exists email_codes_expires
  on email_codes (expires_at);
