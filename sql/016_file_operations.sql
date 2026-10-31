-- Durable native mutation intent and guards. Guards survive worker lease expiry;
-- operation snapshots contain metadata/checkpoints only, never native file bytes.
create table if not exists file_operations (
    id uuid primary key,
    owner_id uuid not null references users(id),
    principal_id text not null,
    action text not null,
    idempotency_key text,
    fingerprint text not null,
    snapshot jsonb not null,
    unique (owner_id, principal_id, action, idempotency_key)
);
create index if not exists file_operations_owner on file_operations(owner_id);
create table if not exists file_operation_guards (
    operation_id uuid not null references file_operations(id),
    owner_id uuid not null references users(id),
    ordinal integer not null,
    native_path text not null,
    comparison_path text not null,
    subtree boolean not null,
    primary key(operation_id, ordinal)
);
create index if not exists file_operation_guards_owner on file_operation_guards(owner_id);
create table if not exists file_operation_checkpoints (
    sequence bigint generated always as identity primary key,
    operation_id uuid not null references file_operations(id),
    owner_id uuid not null references users(id),
    entry_number integer,
    lease_epoch bigint not null,
    checkpoint bytea not null,
    recorded_at timestamptz not null default clock_timestamp()
);
create index if not exists file_operation_checkpoints_operation on file_operation_checkpoints(operation_id, sequence);
