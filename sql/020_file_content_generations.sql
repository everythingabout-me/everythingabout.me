-- Preserve public content generations through proven native moves. Bind every
-- revision to observed native evidence so external replacements cannot inherit it.
create table if not exists file_content_generations (
 owner_id uuid not null references users(id) on delete cascade,
 root_namespace text not null,
 path text not null,
 native_fingerprint bytea not null,
 content_revision text not null,
 primary key(owner_id, root_namespace, path)
);
