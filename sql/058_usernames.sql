-- Usernames give every account one address a person can type: /<handle>/<path>, and the
-- same shape behind the eam:// scheme. A handle is not identity and not a credential —
-- users.id stays the identity and email stays the login identifier — so this table is
-- derived, and deleting it costs addresses, not files.
--
-- Why one table instead of a users.username column plus an alias table:
--
-- A rename has to keep the old name resolving (a bookmark, a pasted link, an agent's saved
-- pointer must not 404 because someone tidied their handle), and the old name must never be
-- claimable by another account while it still redirects. That is a uniqueness rule across
-- "current names" and "historical names" at once, and only a single primary key states it
-- atomically: `handle` is unique over both, so a claim and a rename cannot race into two
-- accounts owning one name. Two tables would need a transaction-level lock to say the same
-- thing and would still leave a window where the pair disagrees.
--
-- Why `current`: one account has exactly one live handle, enforced by the partial unique
-- index below. Superseded rows stay as the alias that redirects.
create table if not exists usernames (
  handle        text primary key,                    -- lowercase, validated by validation.NormalizeUsername
  user_id       uuid not null references users(id) on delete cascade,
  current       boolean not null default true,
  created_at    timestamptz not null default now(),
  superseded_at timestamptz,
  constraint usernames_handle_lowercase check (handle = lower(handle))
);

-- At most one current handle per account. Not an index on user_id alone: an account may
-- hold any number of superseded names.
create unique index if not exists usernames_current_user_uniq
  on usernames (user_id) where current;

-- Resolution by handle is the primary key lookup; this covers "my current handle".
create index if not exists usernames_user_current
  on usernames (user_id) where current;
