-- Events.
--
-- See docs/design/events.md. Only the tables the server needs live here:
--   events        the smallest unit of the timeline: one event = one card (type + payload)
--   event_media   files an event references (payload carries paths, resolved to nodes on insert)
--
-- Relation to the previous version:
--   * events replaces timeline_moments (origin='auto', deduped by producer_key);
--   * timeline_snapshots is no longer written or read. It was a "monthly preselect + budget" read
--     model, and there is no budget or density now: the read path queries events by local_date.
--     Keep the table for now (it holds history) and drop it once it is confirmed unused.
--   * timeline_assets / timeline_series stay: file times and health data still live there.

-- The token's third capability bit: writing events. Separate from file access because an app may
-- only submit events and never touch files.
alter table api_tokens add column if not exists can_events boolean not null default false;

create table if not exists events (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references users(id) on delete cascade,

  -- Attribution: who wrote it
  origin        text not null default 'native',      -- native | auto | app
  token_id      uuid references api_tokens(id) on delete set null,
  external_id   text,                                -- the app's own event id
  producer_key  text,                                -- auto: the pipeline's dedupe key

  -- Time
  starts_at     timestamptz not null,
  ends_at       timestamptz,
  tz            text not null,
  local_date    date not null,                       -- starts_at's date in tz, used for bucketing
  precision     text not null default 'day',         -- day | minute

  -- Card
  type          text not null,                       -- photo | note | metric | html
  title         text not null,
  payload       jsonb not null default '{}'::jsonb,  -- by type; see docs/design/events.md section 3
  detail_url    text,                                -- optional: detail page the title points to

  -- State
  state         text not null default 'published',   -- published | detached | deleted
  merged_into   uuid references events(id) on delete set null,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),

  constraint events_type_check check (type in ('photo', 'note', 'metric', 'html')),
  constraint events_state_check check (state in ('published', 'detached', 'deleted')),
  constraint events_origin_check check (origin in ('native', 'auto', 'app'))
);

-- One app plus one external id can only ever be one row.
create unique index if not exists events_app_ext
  on events (user_id, token_id, external_id)
  where external_id is not null;

-- Automatic events: re-running the pipeline produces no duplicates.
create unique index if not exists events_auto_key
  on events (user_id, producer_key)
  where producer_key is not null;

-- Read path: page backwards in time order.
create index if not exists events_time
  on events (user_id, starts_at desc, id desc)
  where state = 'published';

create index if not exists events_token
  on events (token_id)
  where token_id is not null;

create table if not exists event_media (
  event_id uuid not null references events(id) on delete cascade,
  idx      int not null,
  node_id  uuid not null references nodes(id) on delete cascade,
  primary key (event_id, idx)
);

create index if not exists event_media_node on event_media (node_id);

-- Once the events table ships, existing fragments in an old database must be recomputed to become
-- events. Clear the snapshots' fact watermark; the bootstrap at service start also queues a
-- fallback pass that recomputes month by month, calling events.PublishAuto along the way to write
-- fragments as origin='auto' events.
--
-- Only clear while there are no events at all: migrate replays every SQL file, so with this guard a
-- database that already produced events will not have its timeline reset again.
update timeline_snapshots set source_rev = null
 where not exists (select 1 from events);

-- Queue directly instead of relying on a restart: migrate often runs while the dev server is
-- already up, so the startup bootstrap has long finished. The worker picks these months up within
-- seconds.
insert into timeline_jobs (user_id, kind, month)
select s.user_id, 'month', s.month
  from timeline_snapshots s
 where s.source_rev is null
on conflict do nothing;
