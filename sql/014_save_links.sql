-- 保存链接：给 agent / 脚本的一条 URL，本身就是凭证。
--
-- 链接形如 /s/<link_id><mac>：link_id 用来查行，mac 由服务端密钥对
-- (user_id, link_id) 做 HMAC 得出。库里不存明文链接，账户页每次按行重算，
-- 所以链接可以一直显示，撤销时把 revoked_at 填上即可。
--
-- 全权：保存链接不设 prefix / 能力位，和用户本人一样能读写整个盘。

create table if not exists save_links (
  id           uuid primary key default gen_random_uuid(),
  link_id      text not null unique,
  user_id      uuid not null references users(id) on delete cascade,
  name         text not null default '',
  created_at   timestamptz not null default now(),
  last_used_at timestamptz,
  revoked_at   timestamptz
);

create index if not exists save_links_user on save_links (user_id, created_at desc);
