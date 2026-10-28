-- Save links: one URL for agents and scripts, a credential in itself.
--
-- A link looks like /s/<link_id><mac>: link_id finds the row, and the mac is an HMAC over
-- (user_id, link_id) with a server-side secret. The plaintext link is never stored; the account
-- page recomputes it per row, so links stay displayable and revoking only means setting
-- revoked_at.
--
-- Full power: save links carry no prefix or capability bits, so they read and write the whole
-- drive just like the user.

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
