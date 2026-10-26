-- 内容流（内部叫 timeline / moments，界面上不暴露这些概念）。
--
-- 设计见 docs/design/timeline.md。这里只放服务端需要的表：
--   timeline_assets    每个文件的时间、类型和是否参与叙事
--   timeline_moments   内容流的原子单元（可跨天、可多来源）
--   timeline_snapshots 已发布/可读的按月结果（读路径只读它）
--   timeline_jobs      后台整理队列（幂等、可重试）
--   timeline_series   健康等环境数据的日粒度序列

-- 对象的真实修改时间。nodes 原来的 created_at/updated_at 是"写进索引的时间"，
-- 用 reconcile 灌历史数据时会全是今天，时间线需要一个文件自身的时间兜底。
alter table nodes add column if not exists source_modified_at timestamptz;

create index if not exists nodes_source_modified
  on nodes (owner_id, source_modified_at)
  where is_dir = false;

-- scan 只看"最近三天更新过的文件"，给它一个索引。
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
  is_narrative     boolean not null default false,  -- 是否参与内容流（照片/视频）
  is_ambient       boolean not null default false,  -- 是否被当作环境数据（健康 CSV 等）
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

-- 按月的结果。payload 是去规范化的卡片形状（见 internal/timeline/types.go），
-- moments 才是事实源，快照随时可以从 moments 重新生成。
create table if not exists timeline_snapshots (
  user_id      uuid not null references users(id) on delete cascade,
  month        date not null,
  payload      jsonb not null default '{}'::jsonb,
  coverage     text not null default 'ready',  -- ready | indexing | empty
  file_count   int not null default 0,
  moment_count int not null default 0,
  source_rev   timestamptz,                    -- 生成时的事实水位，用来判断快照是否过期
  pipeline_version int not null default 1,
  generated_at timestamptz not null default now(),
  primary key (user_id, month)
);

-- 后台整理队列。同一用户同类任务只保留一个未完成的，避免上传风暴时堆任务。
create table if not exists timeline_jobs (
  id           bigserial primary key,
  user_id      uuid not null references users(id) on delete cascade,
  kind         text not null,                 -- scan | backfill | month
  node_id      uuid,
  month        date,
  cursor       text,                          -- backfill 的 keyset 游标，可断点续跑
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

-- 健康/步数/睡眠这类"观测"数据。它们不进内容流，只做数据带。
create table if not exists timeline_series (
  id       uuid primary key default gen_random_uuid(),
  user_id  uuid not null references users(id) on delete cascade,
  source   text not null default '',          -- 文件来源，如 health/apple-health.csv
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
