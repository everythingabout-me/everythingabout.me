-- Connect: api_tokens gains a product kind so the browser can show apps and tokens as two lists.
--
--   * app   -- a long-lived connection handed over with a QR code (a service, a device)
--   * token -- a temporary connection copied as a text block (an agent, a skill, a script)
--
-- Kind is a label, not a permission: prefix + can_read / can_write / can_events remain the
-- security boundary. Existing rows were all minted for apps, so they default to 'app'.
-- There is no version table and this file re-runs on every `make migrate`, so every statement
-- is idempotent.

alter table api_tokens add column if not exists kind text not null default 'app';

create index if not exists api_tokens_user_active
  on api_tokens (user_id, created_at desc)
  where revoked_at is null;
