-- Owner-serialized confirmed changes and durable access audit. No file bytes or
-- credential values are stored here. Retained snapshots survive live node deletion.
create table if not exists file_change_heads (
  owner_id uuid primary key references users(id) on delete cascade,
  root_epoch uuid not null default gen_random_uuid(),
  head_position bigint not null default 0 check (head_position >= 0),
  retained_after bigint not null default 0 check (retained_after >= 0 and retained_after <= head_position),
  retained_since timestamptz not null default '1970-01-01 00:00:00+00',
  updated_at timestamptz not null default clock_timestamp()
);

-- Deduplication receipts outlive feed retention, so delayed settlement recovery
-- cannot turn an expired effect into a second change. Deleting an owner removes them.
create table if not exists file_change_effects (
  owner_id uuid not null references users(id) on delete cascade,
  effect_key text not null check (length(effect_key) between 1 and 1024),
  event_id uuid not null,
  position bigint not null check (position > 0),
  fingerprint text not null,
  primary key (owner_id, effect_key),
  unique (owner_id, position),
  unique (event_id)
);

create table if not exists file_changes (
  owner_id uuid not null references users(id) on delete cascade,
  position bigint not null check (position > 0),
  id uuid not null default gen_random_uuid(),
  effect_key text not null,
  kind text not null check (kind in ('created','updated','deleted','moved','invalidated')),
  coverage text not null check (coverage in ('entry','subtree')),
  entry_id uuid,
  before_snapshot jsonb check (before_snapshot is null or jsonb_typeof(before_snapshot) = 'object'),
  after_snapshot jsonb check (after_snapshot is null or jsonb_typeof(after_snapshot) = 'object'),
  before_ancestors jsonb not null default '[]'::jsonb check (jsonb_typeof(before_ancestors) = 'array'),
  after_ancestors jsonb not null default '[]'::jsonb check (jsonb_typeof(after_ancestors) = 'array'),
  impact_paths jsonb not null default '[]'::jsonb check (jsonb_typeof(impact_paths) = 'array'),
  affected_folders jsonb not null default '[]'::jsonb check (jsonb_typeof(affected_folders) = 'array'),
  actor jsonb not null check (jsonb_typeof(actor) = 'object'),
  operation_id text not null default '',
  request_id text not null default '',
  origin text not null,
  observed_at timestamptz not null default clock_timestamp(),
  recorded_at timestamptz not null default clock_timestamp(),
  expires_at timestamptz not null default (clock_timestamp() + interval '90 days'),
  primary key (owner_id, position),
  unique (id),
  unique (owner_id, effect_key),
  foreign key (owner_id, effect_key) references file_change_effects(owner_id, effect_key),
  check (kind = 'invalidated' or entry_id is not null),
  check ((kind = 'created' and before_snapshot is null and after_snapshot is not null)
    or (kind = 'deleted' and before_snapshot is not null and after_snapshot is null)
    or (kind in ('updated','moved') and before_snapshot is not null and after_snapshot is not null)
    or kind = 'invalidated')
);
alter table file_changes add column if not exists affected_folders jsonb not null default '[]'::jsonb check (jsonb_typeof(affected_folders) = 'array');
create index if not exists file_changes_expiry on file_changes(owner_id, expires_at, position);
create index if not exists file_changes_entry on file_changes(owner_id, entry_id, position);

-- Query matching uses the same versioned NFC/full-fold function as identity.
-- Every producer validates absolute paths before insertion; NULL endpoints don't match.
create or replace function file_changes_within(scope_path text, entry_path text, comparison_version text)
returns boolean language sql immutable strict as $$
  select scope_path = '/' or
    file_identity_comparison(entry_path,comparison_version) = file_identity_comparison(scope_path,comparison_version) or
    starts_with(file_identity_comparison(entry_path,comparison_version),
      file_identity_comparison(scope_path,comparison_version) || '/')
$$;

-- Recursive equality markers are application metadata, separate from revisions.
-- No live-node FK: retained tombstones and moved entries keep their marker identity.
create table if not exists file_change_markers (
  owner_id uuid not null references users(id) on delete cascade,
  entry_id uuid not null,
  position bigint not null check (position > 0),
  change_token uuid not null default gen_random_uuid(),
  last_changed_at timestamptz not null,
  primary key(owner_id, entry_id)
);
alter table file_change_markers add column if not exists change_token uuid not null default gen_random_uuid();

create table if not exists file_audit (
  id uuid primary key default gen_random_uuid(),
  owner_id uuid not null references users(id) on delete cascade,
  effect_key text,
  action text not null,
  outcome text not null,
  allowed boolean,
  status integer not null default 0 check (status between 0 and 599),
  bytes_written bigint check (bytes_written is null or bytes_written >= 0),
  reason_code text not null default '',
  entry_id uuid,
  path text,
  actor jsonb not null check (jsonb_typeof(actor) = 'object'),
  operation_id text not null default '',
  request_id text not null default '',
  origin text not null,
  observed_at timestamptz not null default clock_timestamp(),
  fingerprint text not null,
  unique (owner_id, effect_key)
);
create index if not exists file_audit_owner on file_audit(owner_id, observed_at desc, id desc);
