-- Connections gain an owner. `owner_id is null` is the deployment default that every
-- account may use (the operator's COS bucket, configured by the environment); a
-- non-null owner is a connection that account added for itself.
--
-- The name is unique per owner, not globally: two accounts may both call their own
-- connection "nas", and resolution always happens for a known owner, so the pair is
-- what identifies a row. Two partial indexes because a unique index treats NULLs as
-- distinct and the default row must still be unique by name.
alter table storage_connections add column if not exists owner_id uuid references users(id) on delete cascade;
drop index if exists storage_connections_name_key;
create unique index if not exists storage_connections_default_name_key on storage_connections(name) where owner_id is null;
create unique index if not exists storage_connections_owner_name_key on storage_connections(owner_id, name) where owner_id is not null;
create index if not exists storage_connections_owner on storage_connections(owner_id);
