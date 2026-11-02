-- Freeze finite action manifests and native identity observations only after a
-- persistent scope guard exists. No file contents or provider credentials belong here.
create table if not exists file_action_bindings (
    operation_id uuid primary key references file_operations(id),
    owner_id uuid not null references users(id),
    binding jsonb not null
);
create index if not exists file_action_bindings_owner on file_action_bindings(owner_id);
