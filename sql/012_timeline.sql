-- The content stream (internally timeline / moments; the UI never exposes those names).
--
-- See docs/design/timeline.md. Only the tables the server needs live here:
--   timeline_assets    each file's time, type and whether it takes part in the narrative
--   timeline_moments   the atomic unit of the stream (may span days, may have several sources)
--   timeline_snapshots published, readable per-month results (the read path only uses this)
--   timeline_jobs      background organization queue (idempotent, retryable)
--   timeline_series    daily series for ambient data such as health

-- The object's real modification time. nodes' original created_at/updated_at is "when it entered
-- the index", so backfilling history with reconcile would date everything today; the timeline needs
-- the file's own time as a fallback.
alter table nodes add column if not exists source_modified_at timestamptz;

create index if not exists nodes_source_modified
  on nodes (owner_id, source_modified_at)
  where is_dir = false;

-- scan only looks at files updated in the last three days; give it an index.
create index if not exists nodes_owner_updated
  on nodes (owner_id, updated_at)
  where is_dir = false;

create table if not exists timeline_assets (
  user_id          uuid not null references users(id) on delete cascade,
  node_id          uuid not null references nodes(id) on delete cascade,
  event_time       timestamptz not null,
  local_date       date not null,
  time_source      text not null default 'mtime',   -- exif | filename | mtime | ingest
  time_confidence  real not null default 0.5,
  media_type       text not null default 'other',   -- photo | video | audio | doc | data | other
  is_narrative     boolean not null default false,  -- joins the stream (photos/videos)
  is_ambient       boolean not null default false,  -- treated as ambient data (health CSVs)
  is_screenshot    boolean not null default false,
  quality          real not null default 0.5,
  source_sig       text not null default '',
  pipeline_version int not null default 1,
  analyzed_at      timestamptz not null default now(),
  primary key (user_id, node_id)
);

create index if not exists timeline_assets_day
  on timeline_assets (user_id, local_date);
create index if not exists timeline_assets_node
  on timeline_assets (node_id);

create table if not exists timeline_moments (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references users(id) on delete cascade,
  dedupe_key    text not null,
  kind          text not null default 'session',     -- session | series | milestone | note
  starts_at     timestamptz not null,
  ends_at       timestamptz not null,
  local_date    date not null,
  title         text not null default '',
  subtitle      text not null default '',
  cover_node_id uuid references nodes(id) on delete set null,
  item_count    int not null default 0,
  source_kinds  text[] not null default '{}',
  score         real not null default 0,
  cost          int not null default 2,
  layout        text not null default 'mosaic',
  reason        jsonb not null default '{}'::jsonb,
  state         text not null default 'published',   -- published | hidden | user_edited
  pipeline_version int not null default 1,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),
  unique (user_id, dedupe_key)
);

create index if not exists timeline_moments_range
  on timeline_moments (user_id, local_date, starts_at);
create index if not exists timeline_moments_live
  on timeline_moments (user_id, state, local_date);

create table if not exists timeline_moment_items (
  moment_id uuid not null references timeline_moments(id) on delete cascade,
  node_id   uuid not null references nodes(id) on delete cascade,
  rank      int not null default 0,
  role      text not null default 'grid',   -- cover | grid | detail
  primary key (moment_id, node_id)
);

-- Per-month results. payload is the denormalized card shape (see internal/timeline/types.go);
-- moments is the source of truth, and a snapshot can be regenerated from it at any time.
create table if not exists timeline_snapshots (
  user_id      uuid not null references users(id) on delete cascade,
  month        date not null,
  payload      jsonb not null default '{}'::jsonb,
  coverage     text not null default 'ready',  -- ready | indexing | empty
  file_count   int not null default 0,
  moment_count int not null default 0,
  source_rev   timestamptz,                    -- fact watermark; used to detect staleness
  pipeline_version int not null default 1,
  generated_at timestamptz not null default now(),
  primary key (user_id, month)
);

-- Background organization queue. At most one unfinished job of each kind per user, so an upload
-- storm does not pile up jobs.
create table if not exists timeline_jobs (
  id           bigserial primary key,
  user_id      uuid not null references users(id) on delete cascade,
  kind         text not null,                 -- scan | backfill | month
  node_id      uuid,
  month        date,
  cursor       text,                          -- keyset cursor for backfill; resumes after a break
  status       text not null default 'pending', -- pending | running | done | failed
  attempts     int not null default 0,
  available_at timestamptz not null default now(),
  lease_until  timestamptz,
  error        text,
  created_at   timestamptz not null default now()
);

alter table timeline_jobs add column if not exists cursor text;

create unique index if not exists timeline_jobs_scan_pending
  on timeline_jobs (user_id)
  where kind = 'scan' and status in ('pending', 'running');
create unique index if not exists timeline_jobs_backfill_pending
  on timeline_jobs (user_id)
  where kind = 'backfill' and status in ('pending', 'running');
create unique index if not exists timeline_jobs_month_pending
  on timeline_jobs (user_id, month)
  where kind = 'month' and status in ('pending', 'running');
create index if not exists timeline_jobs_ready
  on timeline_jobs (status, available_at);
create index if not exists timeline_jobs_user
  on timeline_jobs (user_id, status);

-- "Observational" data such as health, steps and sleep. It stays out of the stream and only
-- feeds data bands.
create table if not exists timeline_series (
  id       uuid primary key default gen_random_uuid(),
  user_id  uuid not null references users(id) on delete cascade,
  source   text not null default '',          -- file source, e.g. health/apple-health.csv
  metric   text not null,
  label    text not null default '',
  unit     text not null default '',
  pinned   boolean not null default true,
  created_at timestamptz not null default now(),
  unique (user_id, source, metric)
);

create table if not exists timeline_points (
  series_id uuid not null references timeline_series(id) on delete cascade,
  local_date date not null,
  value     double precision not null,
  meta      jsonb not null default '{}'::jsonb,
  primary key (series_id, local_date)
);
