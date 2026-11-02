-- Durable provider-neutral upload sessions and immutable acknowledgements. Bytes
-- remain in private durable spool; terminal rows and idempotency keys are retained.
create table if not exists file_uploads (
    operation_id uuid primary key references file_operations(id),
    owner_id uuid not null references users(id),
    principal_id text not null,
    idempotency_key text,
    client_fingerprint text not null,
    comparison_path text not null,
    part_size bigint not null check(part_size > 0),
    part_count integer not null check(part_count >= 0),
    max_parallelism integer not null check(max_parallelism > 0),
    state text not null check(state in ('open','committing','aborted','expired')),
    received_parts integer not null default 0 check(received_parts >= 0),
    received_bytes bigint not null default 0 check(received_bytes >= 0),
    ack_sequence bigint not null default 0 check(ack_sequence >= 0),
    created_at timestamptz not null,
    expires_at timestamptz not null,
    chunks_cleaned_at timestamptz,
    unique(owner_id, principal_id, idempotency_key),
    unique(operation_id, owner_id)
);
create index if not exists file_uploads_owner_page on file_uploads(owner_id, operation_id);
create index if not exists file_uploads_expiry on file_uploads(expires_at, operation_id) where state='open';
create table if not exists file_upload_parts (
    operation_id uuid not null,
    owner_id uuid not null,
    number integer not null check(number > 0),
    size bigint not null check(size > 0),
    sha256 text not null check(sha256 ~ '^[0-9a-f]{64}$'),
    handle text not null check(length(handle) > 0),
    sequence bigint not null check(sequence > 0),
    primary key(operation_id, number),
    foreign key(operation_id, owner_id) references file_uploads(operation_id, owner_id),
    unique(operation_id, sequence)
);
