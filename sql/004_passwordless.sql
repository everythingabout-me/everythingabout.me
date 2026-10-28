-- Passwords become optional: new accounts have none by default and sign in with email codes only.
-- Existing accounts (if any) keep their current password_hash and are unaffected.
alter table users alter column password_hash drop not null;
