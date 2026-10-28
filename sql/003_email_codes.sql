-- Email verification codes: used for sign-up today, reusable for email changes later
--
-- Only the scrypt hash of a code is stored, never the plaintext; a code expires after 10 minutes,
-- allows at most 5 attempts, and issuing a new one invalidates the old one.

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
