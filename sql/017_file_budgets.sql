-- Explicit operator-established usage and independent logical/physical reservations.
-- No index-derived backfill: initialization must prove native/index comparison and
-- legacy writer drain. P3 keeps the owner root binding immutable, even after settle.
create table if not exists file_budgets (
    owner_id uuid primary key references users(id),
    root_binding jsonb not null,
    baseline jsonb not null,
    logical_bytes bigint not null check(logical_bytes >= 0),
    physical_bytes bigint not null check(physical_bytes >= 0),
    logical_reserved bigint not null default 0 check(logical_reserved >= 0),
    physical_reserved bigint not null default 0 check(physical_reserved >= 0),
    logical_limit bigint not null check(logical_limit >= 0),
    physical_limit bigint not null check(physical_limit >= 0)
);
create table if not exists file_reservations (
    operation_id uuid primary key references file_operations(id),
    owner_id uuid not null references users(id),
    request jsonb not null,
    logical_reserved bigint not null check(logical_reserved >= 0),
    physical_reserved bigint not null check(physical_reserved >= 0),
    settled boolean not null default false,
    settlement jsonb
);
create index if not exists file_reservations_owner on file_reservations(owner_id);
-- A native revision's size credit can be consumed only once, including after the
-- original path guard is released. Proven no-effect termination releases the claim.
create table if not exists file_credit_claims (
    owner_id uuid not null references users(id),
    comparison_path text not null,
    version bytea not null,
    operation_id uuid not null references file_operations(id),
    primary key(owner_id, comparison_path, version)
);
-- Positive proven entry charges may be booked while other entries remain unknown.
-- A unique entry identity makes replay safe without releasing unresolved liability.
create table if not exists file_entry_charges (
    operation_id uuid not null references file_operations(id),
    owner_id uuid not null references users(id),
    entry_number integer not null,
    charge jsonb not null,
    primary key(operation_id, entry_number)
);
create index if not exists file_entry_charges_owner on file_entry_charges(owner_id);
