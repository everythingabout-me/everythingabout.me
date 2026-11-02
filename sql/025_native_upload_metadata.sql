-- Native multipart allocation and immutable part identities are shared by the
-- application and independent byte receivers. Only metadata is stored here;
-- payloads live in unpublished provider parts, never in Postgres.
create table if not exists file_upload_native_metadata (
    operation_id uuid not null,
    owner_id uuid not null,
    binding text not null,
    key text not null,
    value jsonb not null,
    primary key(operation_id, binding, key),
    foreign key(operation_id, owner_id) references file_uploads(operation_id, owner_id)
);
