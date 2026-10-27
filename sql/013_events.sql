-- 事件（events）。
--
-- 设计见 docs/design/events.md。这里只放服务端需要的表：
--   events        时间线的最小单位：一条事件 = 一张卡（type + payload）
--   event_media   事件引用的文件（payload 里写路径，落库时解析成 node）
--
-- 和上一版的关系：
--   * timeline_moments 由 events 取代（origin='auto'、producer_key 去重）；
--   * timeline_snapshots 不再写入也不再读取。它是"按月预选 + 预算"的读模型，
--     现在没有预算、没有密度，读路径直接按 local_date 查 events。
--     表先留着（里面有历史数据），确认没用之后再删。
--   * timeline_assets / timeline_series 保留：文件时间和健康数据仍然存在那里。

-- 令牌的第三个能力位：写事件。和读写文件分开，因为一个应用可能只投稿、不碰文件。
alter table api_tokens add column if not exists can_events boolean not null default false;

create table if not exists events (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references users(id) on delete cascade,

  -- 归因：谁写的
  origin        text not null default 'native',      -- native | auto | app
  token_id      uuid references api_tokens(id) on delete set null,
  external_id   text,                                -- 应用内的事件 id
  producer_key  text,                                -- auto：流水线的去重键

  -- 时间
  starts_at     timestamptz not null,
  ends_at       timestamptz,
  tz            text not null,
  local_date    date not null,                       -- starts_at 在 tz 下的日期，分桶用
  precision     text not null default 'day',         -- day | minute

  -- 卡片
  type          text not null,                       -- photo | note | metric | html
  title         text not null,
  payload       jsonb not null default '{}'::jsonb,  -- 随 type 变，见 docs/design/events.md §3
  detail_url    text,                                -- 可选：标题指向的详情页

  -- 状态
  state         text not null default 'published',   -- published | detached | deleted
  merged_into   uuid references events(id) on delete set null,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now(),

  constraint events_type_check check (type in ('photo', 'note', 'metric', 'html')),
  constraint events_state_check check (state in ('published', 'detached', 'deleted')),
  constraint events_origin_check check (origin in ('native', 'auto', 'app'))
);

-- 同一个应用、同一个外部 id，只可能有一条。
create unique index if not exists events_app_ext
  on events (user_id, token_id, external_id)
  where external_id is not null;

-- 自动事件：流水线重跑不产生重复。
create unique index if not exists events_auto_key
  on events (user_id, producer_key)
  where producer_key is not null;

-- 读路径：按时间倒序翻页。
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

-- 事件表上线后，老库里已有的片段要重算一次才会变成事件。
-- 把快照的事实水位清空；服务启动时 bootstrap 也会兜底排一次队列，逐月重算，
-- 重算过程中会调用 events.PublishAuto 把片段写成 origin='auto' 的事件。
--
-- 只在还没有任何事件时清空：migrate 每次都会重跑所有 SQL，加了这个条件之后，
-- 已经产生过事件的库再跑 migrate 不会把时间线再重置一遍。
update timeline_snapshots set source_rev = null
 where not exists (select 1 from events);

-- 直接排队，不依赖服务重启：migrate 经常是在 dev server 还开着的时候跑的，
-- 启动时的 bootstrap 那时已经跑过了。worker 会在几秒内领走这些月份。
insert into timeline_jobs (user_id, kind, month)
select s.user_id, 'month', s.month
  from timeline_snapshots s
 where s.source_rev is null
on conflict do nothing;
