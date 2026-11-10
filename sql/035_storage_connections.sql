-- Storage connections become data: one row per native tree root, so adding a provider
-- is an INSERT instead of a deployment edit. Rows are operator data and are never
-- exposed to a file client; `file_budgets.root_binding` stays the durable per-owner
-- pointer, and the row that pointer names is the authority for that owner's tree.
--
-- `identity` is a connection-level digest over backend, endpoint, bucket, prefixes and
-- name rules. Credentials are deliberately excluded: rotating a key keeps the same
-- tree, while changing the endpoint does not. It is unique because one tree is one row,
-- and it excludes the human name so two rows cannot claim the same tree.
--
-- `generation` changes only when the location changes. The change-stream namespace
-- embeds connection + generation + both roots, so an in-place endpoint edit under an
-- unchanged generation would let an old cursor continue against a different tree.
create table if not exists storage_connections (
  id         uuid primary key default gen_random_uuid(),
  name       text not null,
  backend    text not null check (backend in ('cos', 's3', 'local')),
  role       text not null check (role in ('authority', 'backup')),
  identity   text not null,
  config     jsonb not null default '{}'::jsonb,
  secret     jsonb not null default '{}'::jsonb,
  generation bigint not null default 1 check (generation >= 1),
  admission  jsonb,
  enabled    boolean not null default true,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create unique index if not exists storage_connections_identity_key on storage_connections(identity);
create unique index if not exists storage_connections_name_key on storage_connections(name);
