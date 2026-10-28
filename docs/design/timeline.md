# Timeline design draft

> Status: v0.1 · 2026-10 · under discussion
>
> One line: the timeline is not "a file list sorted by date" but a **personal chronicle with editorial judgment**.
> Files are evidence; the moment is the unit of the timeline.
>
> **2026-10 update**: what actually ships is governed by §17 and [events.md](events.md).
> The `moments` / snapshot model in §3, the monthly budget in §4, and user editing in §8 were never implemented, and there is no plan to implement them (see §17.4).

---

## 1. Positioning and goals

### 1.1 What problem it solves

A user pours ten years of photos, videos, health exports, and documents into a drive and ends up with a **directory tree that keeps getting harder to browse**.
What they want to see is not "437 files on 2019/05/12" but "in May 2019 I went to Qinghai."

The timeline's job:

1. **Turn files back into experiences**: aggregate by event time, dedupe, pick representatives, and turn the result into readable moments;
2. **Control narrative density**: no dumping everything, no random truncation, but "what deserves attention gets space, what doesn't gets folded away";
3. **Always traceable**: anything folded away can be expanded level by level back to the original file, without creating the illusion that data was lost;
4. **Automatic flow-in**: new uploads and old-file backfill go through the same pipeline, so the user never has to "organize" by hand.

### 1.2 Non-goals

- No "running log of every file". That is the file browser's job; the timeline keeps only the chronicle summary plus a full entry point.
- No social feed, no check-ins, no nagging like "you've logged 87 days in a row".
- No analysis on the read path (thumbnails, EXIF, clustering must all be precomputed).
- The first version does not chase AI aesthetics or face recognition; get the structure and the judgments right first — ML is a bonus, not the foundation.

### 1.3 The smallest unit is not "the day"

This is the single most important decision in the whole design.

If the unit were "the day", a day with 500 photos and a day with 1 video would both become one date card: either it explodes or it's empty.
So the smallest unit of the timeline is the **moment**:

- A moment has a start and an end and can **span days** (a 5-day trip = 1 moment);
- A moment is made of some files plus (optionally) evidence from other sources (health, GPS, documents);
- The timeline is a sequence of "moments + gaps", not a list of "dates + files".

The day is only the bucket of the chronicle layer; **days without a moment collapse into one gap** and take up no space.

---

## 2. Three layers

```
┌──────────────────────────────────────────────────────────────┐
│ 编年层 chronicle   2024 年 5 月 · 1,284 个文件 · 6 个片段      │  ← 永远在，保证"完整"
│   （月/年标题 + 文件密度条 + 覆盖状态）                          │
├──────────────────────────────────────────────────────────────┤
│ 叙事层 moments     [ 西湖 · 5/12 · 87 张 → 9 张 ]  hero        │  ← 有取舍，有密度
│                    [ 家 · 5/18 · 生日 · 3 人 ]      mosaic     │
│                    [ 数据带：睡眠/心率/步数 ]        ambient    │
│                    [ 5/20 – 6/02 · 214 个文件 ]     gap        │  ← 空白也表达
├──────────────────────────────────────────────────────────────┤
│ 明细层 detail      片段内的全部文件 / 全部会话 / 原始数据       │  ← 无损，"都能找到"
└──────────────────────────────────────────────────────────────┘
```

Three principles:

1. **The narrative layer is lossy; the chronicle layer and the detail layer are lossless.** The timeline may show only 6 moments, but the month heading must state how many files the month has, and clicking through must show all of them.
2. **A moment can span days and draw on multiple sources.** A trip, a renovation, a stretch of continuous health data should each be one narrative unit, not chopped up by day.
3. **A gap is information.** If three months hold nothing worth looking at, show one collapsed gap row instead of filling it with low-value content.

---

## 3. Data model

### 3.1 First, fill an existing hole: file time

Right now `nodes` only has `created_at / updated_at` (both are "when it was written into the index").
When `reconcile` imports files from ten years ago, `created_at` is all today — the timeline would think every historical file was uploaded today.

It must gain:

```sql
-- Last modified time of the object on the storage side (LastModified from COS List/HeadObject)
-- It is not "capture time", but it is the only reliable time belonging to the file itself, used as a fallback
alter table nodes add column if not exists source_modified_at timestamptz;
```

`reconcile` / upload confirmation / API PUT must all write it. This is the P0 prerequisite; without it, every backfilled timeline is wrong.

### 3.2 Schema

```sql
-- Each file's time and presentable facts. Media, documents, and data files all have a row.
create table if not exists asset_facts (
  user_id        uuid not null references users(id) on delete cascade,
  node_id        uuid not null references nodes(id) on delete cascade,
  event_time     timestamptz,          -- event time: EXIF > filename date > mtime > ingest time
  local_date     date,                 -- local date at the shooting location, used for bucketing (no day drift across time zones)
  tz             text,                 -- time zone at the shooting location, e.g. Asia/Shanghai
  time_source    text not null,        -- exif | filename | mtime | ingest
  time_confidence real not null default 0.5,
  media_type     text,                 -- photo | video | audio | doc | data | other
  width int, height int, duration_ms bigint,
  gps_lat double precision, gps_lon double precision,
  quality        real,                 -- sharpness/exposure/composition heuristic, 0..1
  phash          bit(64),              -- perceptual hash, used for dedupe
  is_screenshot  boolean not null default false,
  duplicate_of   uuid references nodes(id) on delete set null,
  content_sig    text,                 -- etag + size + pipeline_version, used for idempotency
  analyzed_at    timestamptz,
  pipeline_version int not null default 1,
  primary key (user_id, node_id)
);
create index asset_facts_day on asset_facts (user_id, local_date);
create index asset_facts_time on asset_facts (user_id, event_time);

-- The atomic unit of the timeline
create table if not exists moments (
  id             uuid primary key default gen_random_uuid(),
  user_id        uuid not null references users(id) on delete cascade,
  dedupe_key     text not null,        -- used to claim the old moment on recompute, keeping the id / deep link stable
  kind           text not null,        -- trip | outing | session | series | metric | milestone | note
  starts_at      timestamptz not null,
  ends_at        timestamptz not null,
  local_date     date not null,        -- narrative anchor day (the local day of starts_at)
  title          text,
  subtitle       text,
  cover_node_id  uuid references nodes(id) on delete set null,
  item_count     int not null default 0,
  source_kinds   text[] not null default '{}',  -- {photo,health,gps,doc} multi-source evidence
  score          real not null,        -- value 0..1
  cost           int not null,         -- display cost (units in §4)
  layout         text not null,        -- hero | mosaic | strip | series | metric | note | compact
  reason         jsonb not null default '{}',   -- scoring rationale, explainable + debuggable
  evidence       jsonb not null default '{}',   -- which other sources were fused in
  state          text not null default 'provisional', -- provisional|published|hidden|user_edited
  locked_fields  text[] not null default '{}',  -- title/cover/pin/hide, never overwritten by recompute
  pipeline_version int not null default 1,
  created_at     timestamptz not null default now(),
  updated_at     timestamptz not null default now(),
  unique (user_id, dedupe_key)
);
create index moments_range on moments (user_id, local_date, starts_at);
create index moments_published on moments (user_id, state, local_date);

create table if not exists moment_items (
  moment_id uuid not null references moments(id) on delete cascade,
  node_id   uuid not null references nodes(id) on delete cascade,
  rank      int not null,
  role      text not null default 'detail',  -- cover | grid | detail
  score     real not null default 0,
  primary key (moment_id, node_id)
);

-- Directory-level source policy: photos go to the narrative, health goes to the data band, ID photos stay off the timeline
create table if not exists source_rules (
  id          uuid primary key default gen_random_uuid(),
  user_id     uuid not null references users(id) on delete cascade,
  path_prefix text not null default '',
  mode        text not null,           -- timeline | ambient | series | off
  options     jsonb not null default '{}',
  created_at  timestamptz not null default now(),
  unique (user_id, path_prefix)
);

-- Ambient data (health, steps, weight, spending... let's start with health)
create table if not exists metric_series (
  id       uuid primary key default gen_random_uuid(),
  user_id  uuid not null references users(id) on delete cascade,
  source   text not null,              -- health | apple-health | csv:xxx
  metric   text not null,              -- resting_hr | sleep_hours | steps
  unit     text,
  cadence  text,                       -- daily | hourly | irregular
  pinned   boolean not null default true,  -- whether it goes on the data band
  unique (user_id, source, metric)
);

create table if not exists metric_points (
  user_id   uuid not null references users(id) on delete cascade,
  series_id uuid not null references metric_series(id) on delete cascade,
  local_date date not null,
  value     double precision not null,
  meta      jsonb not null default '{}',
  primary key (series_id, local_date)
);

-- Published monthly snapshots: the read path reads only this table.
-- It is the denormalized read model of moments (selected moments + ordering + card shapes + gaps),
-- moments remains the source of truth; a snapshot can be regenerated from moments at any time.
create table if not exists timeline_snapshots (
  user_id    uuid not null references users(id) on delete cascade,
  month      date not null,            -- the 1st of the month
  payload    jsonb not null,           -- selected moments + gaps + data band summary
  coverage   text not null,            -- indexed | analyzed | curated
  file_count int not null default 0,
  moment_count int not null default 0,
  pipeline_version int not null,
  generated_at timestamptz not null default now(),
  primary key (user_id, month)
);
```

### 3.3 Recomputing a moment ≠ deleting and rebuilding

`moments` is a persistent entity; it is not wiped on every recompute:

- Each moment has a `dedupe_key` (usually a hash of "anchor node id + date + kind");
- On recompute, the old moment is claimed by `dedupe_key`, and only changed fields are updated;
- Fields listed in `locked_fields` (a title, cover, pin, or hide the user changed) are never overwritten;
- A moment that disappears is marked `state='hidden'` instead of being deleted, preserving deep links and the record of user actions.

That way timeline URLs, favorites, and user edits stay valid after recomputes and backfills.

---

## 4. Density: constant bandwidth + elastic allocation

### 4.1 Turning "density" into a computable budget

The user's intuition is "give the valuable stuff more room and the worthless stuff less".
In implementation that becomes two constraints:

- **Global bandwidth is constant**: whether the month holds 50 photos or 5,000, the cap on a month's display height is about the same;
- **Local density is elastic**: value decides who gets the bandwidth, and a month is allowed to borrow from its quiet days for a high-value event.

Define the display unit `u = 72px` (one compact row):

| Card | Cost | Typical height | Where it's used |
|---|---|---|---|
| hero | 6u | 432px | a single video, a trip cover, a big event |
| mosaic | 4u | 288px | the 5–9 selected shots from one outing |
| series | 3u | 216px | a daily vlog, an episodic series |
| regular | 2u | 144px | a single photo, an ordinary moment |
| compact | 1u | 72px | a weekly report, a small milestone |
| data band | 2u/month | one side rail | health and other ambient data |

The monthly budget `B` defaults to 24u (about 2.2 screens), with `calm 14 / standard 24 / rich 36` presets.
**The budget is a ceiling, not a target** — if there is nothing worth looking at, leave it empty.

### 4.2 Selection algorithm

Run a small knapsack per month:

```
Input: candidate moments m_i, value v_i ∈ [0,1], cost c_i ∈ {1,2,3,5,6}
Constraints:
  Σ c_i ≤ B_month
  ≤ 3 moments per day, and ≤ 12u
  v_i ≥ τ (τ = 0.35; relax to 0.25 when the whole month is empty, at most 1 compact card)
  when v_i ≥ 0.75, borrow at most 12u from the two adjacent months (month cap 1.5B)
Objective: maximize Σ v_i (with diversity suppression for the same source / same time window, see §5.3)
```

- The actual implementation is **greedy plus a decaying minimum gap** (2 days → 1 day → 0 days): select by value first, spread ordinary content out as much as possible, and let high-value content (≥0.75) sit next to itself;
- With few cost tiers and a small budget, this could just as well be exact DP; for now the priority is "homogeneous content must not bunch up", because the rhythm of the timeline matters more than squeezing in one more item;
- Diversity suppression happens before selection: candidates from the same session are merged into one, and near-duplicates are dropped outright;
- Order is always chronological; selection only decides "who appears and at what cost".

### 4.3 Behavior this produces naturally

| Situation | Result |
|---|---|
| An ordinary month (a few photos each week) | 2–4 ordinary moments, plus the month heading; no padding |
| A travel month (2,000 photos in 5 days) | 3 hero/mosaic cards (including the cross-day trip card); the remaining days are collapsed |
| One vlog per day | 1 series card (3u) plus maybe 1 featured episode; not 30 cards |
| 500 photos per day | Clustered into 1–3 moments by session, each with 1 mosaic; not 500 rows |
| Health data every day | 1 data band (2u/month) + a weekly report + a few anomaly cards |
| Three months with nothing | One collapsed gap row + file count; click to see everything |

### 4.4 Floors and overrides

- **Floor**: for a month that has content in the index but no moment reaching τ, show at most 1 compact card (titled "N files"), so the timeline does not look broken;
- **Borrowed bandwidth**: high-value events (a wedding, a graduation, a long trip) may exceed the monthly budget by borrowing from the adjacent months, up to 1.5×;
- **Current-month exemption**: moments from the last 14 days may all appear in the "latest" section (the user just uploaded them and must be able to see them), but the narrative layer still follows the budget. The two are visually separate in the UI (see §8).

---

## 5. Value scoring: what counts as "interesting"

### 5.1 Scoring framework

```
v = clamp( 0.9·content + 0.5·structure + 0.7·context + 0.5·people + 0.8·manual + 0.5·multi-source bonus - penalty )
```

Each term is normalized to 0..1 first; the weights are educated guesses, calibrated against fixed fixtures (see §12).

| Group | Signals (computable in v1) | Notes |
|---|---|---|
| Content | resolution, sharpness, exposure, duration, page/word count for documents, audio-level swing in video | not about aesthetics; first filter out the duds |
| Structure | session size, burst ratio, how rare the time of day is, folder semantics (trip/wedding/graduation), filename semantics | 500 photos is itself a signal |
| Context | distance from home, new place (relative to personal history), elevation gain, co-occurrence with health data, time since the last event of the same kind | "first time / haven't done this in ages" weighs heavily |
| People | face count (no recognition), share of group shots, share of selfies | face clustering comes in P2 |
| Manual | favorites, tags, notes, pins, shares; hide / "not on the timeline" counts negative | explicit user signals have the highest priority |
| Multi-source | photos + GPS + heart rate + documents in the same window → ×(1 + 0.15·(n-1)) | the timeline is "a join over life facts" |
| Penalty | screenshots, memes, receipts, installers, backups, node_modules, caches, near-duplicates | push straight below τ |

Each moment stores its `reason` (which signals, how many points each), for three uses: debugging density, explaining to the user "why am I seeing this", and — in P2 — as input for an LLM writing the title.

### 5.2 Classification comes before scoring

Before scoring, decide **which kind of narrative this file/directory belongs to**, because the kinds are handled completely differently:

| Kind | Examples | Timeline handling |
|---|---|---|
| episodic | photos, videos, trips, parties | clustered into moments, participate in the budget |
| ambient | health, steps, sleep, location traces, dashcam | **never enters a moment**, goes to the data band; only aggregates and anomalies surface |
| series | a daily vlog, episodic shows, podcasts | 1 series card; takes no space per episode |
| collected | downloaded PDFs, installers, code, backups, IDs | off the narrative layer by default; only counted in the chronicle layer's file count |
| work | project directories, design files, document revisions | P2: one milestone per "project active period" |

Telling "collected" from "episodic" matters a lot: 80% of a drive is collected material, and stuffing it into the timeline drowns the real memories.
Default rules can be inferred from path/extension/size, and the user can right-click a folder to change them (`source_rules`).

### 5.3 Sessions and dedupe

**Photos (the P0 core)**:

1. Sort by `event_time`;
2. Split into sessions: `Δt > 25min` or displacement > 3km, or (P2) scene-vector distance too large;
3. Dedupe within a session: `Δt < 3s` and pHash Hamming distance ≤ 8 → same group; keep only the highest-quality frame, record the rest as `duplicate_of`;
4. Representative frames: cover = highest quality × face count × centrality; the grid picks 4–9 with MMR (λ=0.7, so you don't get 9 identical shots);
5. Merge across days into a trip: several consecutive days "more than 50km from home and in the same direction" with gaps < 24h → one `trip` moment, split into chapters by day inside (detail layer).

**Video**:

- Short video (< 3min): handled as an ordinary moment, cover = first/key frame + duration;
- Long video: clustered by session; the card shows duration and chapter count (chapters cut from scene changes, P1);
- One per day, similar duration, fixed camera position → **ambient** (surveillance/dashcam), producing a "video band + motion/voice highlights", not 365 cards;
- One per day, with duration and content varying → **series**, 1 series card + featured episodes.
  P0 already implements the basic call: same directory, spanning ≥7 days, ≥7 videos → one series card (`SessionGrouper`).

**Health data**:

- CSV / JSON / Apple Health `export.xml` / FIT / TCX → parsed into `metric_points` (aggregated by day);
- Importing the same data twice: idempotent upsert keyed by content hash + date range;
- The files themselves **never appear on the timeline**; they are reachable only through "N data files" in the month heading.

---

## 6. Three key scenarios

### 6.1 One video per day

```
2025 年 6 月 · 30 个视频 · 1 个系列
┌─────────────────────────────────────────────────┐
│ [封面 16:9]  每日一镜 · 6 月                     │
│ 30 集 · 平均 4 分 12 秒 · 最近 6/30 第 181 天    │
│ ▸ 6/30 海边   [▣][▣][▣][▣][▣]        查看 30 集 → │
└─────────────────────────────────────────────────┘
```

- First decide "series" or "ambient": theme/duration/camera position vary → series; nearly constant → ambient.
- A series card costs only 3u; a whole year of daily uploads takes 1/8 of one month's bandwidth.
- Individual episodes do not appear one by one; "the month's best episode" may take one extra ordinary card (picked by value), as an entry point.
- If the user really wants to watch them one by one: click into the series → episode list (grouped by month, with duration and cover), or just use the file browser.

### 6.2 500 photos in one day

```
6 月 15 日 · 周日
┌─────────────────────────────────────────────────┐
│ 西湖 · 09:12–17:40 · 87 张 → 9 张               │
│ [    大图    ] [图][图]                          │
│ [图][图][图][图]                                 │
│ 另有 78 张相似 · 查看全部 →                      │
└─────────────────────────────────────────────────┘
┌─────────────────────────────────────────────────┐
│ 家 · 19:20–21:05 · 413 张 → 6 张                │
│ [图][图]  生日 · 3 人                            │
│ 另有 407 张 · 查看全部 →                         │
└─────────────────────────────────────────────────┘
```

- 500 photos are not "500 items", and it is not "grab the first 9": sessionize first (West Lake in the morning / home in the evening), then dedupe within each session and pick representatives with MMR.
- At most 3 moments per day (12u); extra sessions fold into the detail layer, disclosed explicitly with "N more".
- The card always carries the honest `N photos → 9` annotation, and clicking goes to the full grid for that day, with the selected ones marked.
- Near-duplicates (bursts, the same scene) are merged by default, showing "78 similar" instead of 78 scattered rows.
- (P1) The server pre-composes one mosaic cover image, so mobile makes 1 image request instead of 9 presigned URLs.

### 6.3 Health data every day

Health data is an **observation**, not an **event**. It does not enter the narrative layer; it becomes a "data band" on the time axis:

```
2025 年 6 月 ────────────────────────────────
静息心率  ▁▂▁▂▃▂▁▂▁▁▂▁▂▁▂  54 bpm  ×
睡眠      ▅▄▅▃▄▅▄▃▅▄▄▅▄▄▅  7.1 h   ×
步数      ▃▅▂▇▄▃▅▂▄▅▃▆▄▃▅  6.8 k   ×

┌ 6/12 · 连续 3 天静息心率 > 60（基线 54） → 看看 ┐   compact
└ 6/23 · 月报：睡眠 -0.4h，步数 +12%              ┘   compact
```

- **Data band**: a fixed 24–40px narrow rail on the left of the time axis (on mobile it collapses into an expandable strip at the top), drawing a mini sparkline per week/month, colored by **z-score relative to the personal baseline**, not by absolute value;
- **Weekly/monthly reports**: only appear when "something changed" (if the change < ε, they don't appear), 1u;
- **Anomaly cards**: three consecutive days off baseline, a record broken, or a gap > 3 days in the data — only then is it promoted to an ordinary moment, with evidence and a "take a look";
- Raw daily files never become cards on their own; the month heading carries the "N data files" entry point;
- When importing ten years of data, the data band should **backfill history silently** and not produce "you added 3,650 files" notifications;
- The data band is a generic mechanism; weight, spending, screen time, and listening history can attach later without changing the timeline itself.

**Cross-source fusion is the endgame**: photos + GPS + heart rate on the same day can compose a moment that an ordinary card cannot:

```
┌ 6/15 · 西湖徒步 · 12.6km · 爬升 620m ────────────┐
│ [3 张精选]  平均心率 132 · 最高 168 · 4 小时 12 分 │
│ 照片 87 张 · 轨迹 1 条 · 健康 3 项                │
└──────────────────────────────────────────────────┘
```

That is also what this product should look like: the timeline is not a photo album, it is **the place that joins the life facts scattered across all kinds of files**.
Photos provide the picture, GPS the place and the trace, health data the intensity, documents the context. A single source still works; with several sources the value multiplies by 1.15^(n-1).

### 6.4 A few other scenarios that are easy to get wrong

| Scenario | Handling |
|---|---|
| One day across several cities/sights | split by session, at most 3 moments; same-kind sessions merge into "A and B" |
| A 5-day trip | 1 trip moment (spanning days), detail layer split into chapters by day |
| 3,000 photos at a wedding | 1 hero + 1 mosaic (ceremony/banquet), the rest into detail |
| Screenshots / memes / receipts | filtered by default, not counted in the narrative layer, but still counted in the chronicle layer |
| IDs, contracts, installers | collected, off the timeline by default |
| A monthly statement | ambient or series (judged by file shape), never per file |
| Ten years of photos imported at once | first show the chronicle layer's "file density heatmap" (seconds), then refine month by month |
| Old files uploaded today | event time falls on the old date, plus a light note in "recently organized" |
| Time zones / crossing days | bucket by the local date at the shooting location; when EXIF is missing, convert mtime to the user's home time zone and mark low confidence |
| Untrustworthy file times (downloaded old images, WeChat images) | low `time_confidence` → not in the narrative layer, but in a "time unconfirmed" list |

---

## 7. Backfill: automatic, incremental, explainable

### 7.1 Trigger points

| Source | Trigger |
|---|---|
| Web upload confirmation | queued after `upload/confirm` succeeds |
| Public API PUT | queued after the index write |
| `reconcile` (COS inventory / coscli bulk load) | queued in batches, deduped by etag |
| User changes a folder policy | the affected months are recomputed |
| Pipeline version bump | lazily recomputed in the background, never blocking reads |

### 7.2 Three-stage progression, "coarse before fine"

When importing 2TB of data, the user must not wait for analysis to finish before seeing a timeline:

| Stage | Input | Output | Cost |
|---|---|---|---|
| **indexed** (seconds) | `nodes` + `source_modified_at` | per-day **file density heatmap** + month counts + the chronicle layer | pure SQL |
| **analyzed** (minutes–hours) | image/video metadata, thumbnails | moments, covers, scores | per file count, rate-limited |
| **curated** (continuous) | user edits, cross-source fusion, ML | stable curated results | incremental |

So the correct experience of "backfill happens automatically" is: **first see the outline of ten years (where things are, where it's empty), then watch the detail grow month by month**. Not a spinner.

### 7.3 Job queue

```sql
create table if not exists timeline_jobs (
  id bigserial primary key,
  user_id uuid not null,
  kind text not null,              -- probe | group | fuse | publish
  node_id uuid,                    -- used by probe
  month date,                      -- used by publish
  status text not null default 'pending',  -- pending|running|done|failed|skipped
  attempts int not null default 0,
  available_at timestamptz not null default now(),
  lease_until timestamptz,
  error text,
  unique (kind, node_id),
  unique (kind, user_id, month)
);
create index timeline_jobs_ready on timeline_jobs (status, available_at);
```

- A Go background worker (2–4 concurrent) claims jobs with `FOR UPDATE SKIP LOCKED`, and `lease_until` prevents stuck leases;
- Failures back off exponentially; after 5 attempts they go to dead-letter (keeping the reason, and the UI shows "12 files could not be parsed");
- Global rate limits (COS / CI QPS), with fair scheduling per user so one big library cannot starve the others;
- **The most recent 12 months go first** by default (so the default view is complete), then backfill from recent to old; the user can mark a month "organize first";
- Upload storms are debounced: 500 uploads in one go trigger one monthly recompute, 30s after the last event.

### 7.4 Idempotency and recomputability

- `content_sig = etag + size + pipeline_version`; results are written back to `asset_facts`, and duplicate jobs are skipped outright;
- A monthly snapshot is replaced wholesale (upsert `timeline_snapshots` in one transaction), so the read path always sees the complete old version or the complete new version, never a half-built one;
- A rerun must produce a **byte-identical** snapshot (same input + same version), guaranteed by a golden test;
- The user deletes a file → `asset_facts` / `moment_items` cascade, and the next monthly recompute adjusts automatically; derived thumbnails are deleted by node_id.

### 7.5 Coverage state must be visible

Every month and every source needs a coverage state. A sparse 2013 and a 2013 that "hasn't been organized yet" must look different in the UI, or the user will think data was lost:

```
2025 年 6 月 · 1,284 个文件 · 6 个片段        ✅ 已整理
2013 年 3 月 · 2,914 个文件 · 整理中 43%      ⏳ 预计 6 分钟
2013 年 2 月 · 0 个文件                       — 没有内容
```

`GET /api/timeline/status` returns `{coverage, progress, throughput, eta, failed}`.

---

## 8. Stability: don't hand back a different set on every refresh

Once the timeline starts "looking different every time", the user stops trusting it. Rules:

1. **Published monthly snapshots are immutable**; they are recomputed only for new content, a version bump, or a user edit;
2. **Old months are frozen**: months > 30 days old are not reordered because the background filled in detail; a new moment replaces a selected one only when its value is more than **15% higher** (hysteresis);
3. **Determinism**: all ties are broken with `hash(user_id, month, moment_id)`, never randomness;
4. **User edits have the highest priority**: pin/hide/retitle/replace cover are written to `locked_fields`, and recomputes only add, never remove;
5. **Recomputes are reversible**: when `pipeline_version` changes, generate in the shadow first, and switch only after the user clicks "organize again";
6. **The read path reads only snapshots**: all analysis happens in the background, so a page never hangs on one bad image.

---

## 9. API

```http
GET /api/timeline?before=2025-06-01&months=2&tz=Asia/Shanghai
→ {
    "periods": [{
      "month": "2025-06-01",
      "file_count": 1284,
      "coverage": "analyzed",
      "generated_at": "...",
      "items": [
        {"type":"moment","id":"...","layout":"mosaic","title":"West Lake · hike","status":"published",
         "starts_at":"...","ends_at":"...","cost":5,
         "cover":{"path":"Photos/2025/06/15/a.jpg","thumb":"<signed>","w":1200,"h":900},
         "grid":[{"path":"...","thumb":"<signed>"}, ...],
         "more": 78, "item_count": 87, "sources":["photo","gps"],
         "why":"new place + 87 photos + 620m of ascent"},
        {"type":"ambient","metrics":[{"key":"resting_hr","label":"Resting HR","unit":"bpm",
         "values":[54,55,...],"baseline":54,"delta":"+2"}]},
        {"type":"gap","from":"2025-05-20","to":"2025-06-01","file_count":214}
      ]
    }],
    "next": "2025-04-01",
    "backfill": {"running":true,"from":"2014-01","progress":0.43,"eta_seconds":360}
  }

GET /api/timeline/moments/{id}        → moment detail (all files, sessions, evidence, source links)
GET /api/timeline/day/2025-06-15      → the full grid for that day (the completeness entry point)
GET /api/timeline/status              → coverage / progress / failures / version
PATCH /api/timeline/moments/{id}      → {title?, cover?, pinned?, hidden?}
POST /api/timeline/moments/{id}/merge → {ids:[]}
POST /api/timeline/moments/{id}/split → {day?}
POST /api/timeline/rebuild            → {from,to} (owner only)
GET  /api/timeline/rules             → source policies
PUT  /api/timeline/rules             → directory-level timeline|ambient|series|off
```

Key points:

- Paging is **by month** (`before` + `months`), because density is a monthly budget, and only month-based paging keeps every page the same height;
- Thumbnail URLs are presigned **at response time** (10 minutes); the snapshot stores only paths;
- Degraded failure: when a snapshot is missing, return `coverage: "indexed"` + the file heatmap so the frontend can show a skeleton instead of a 500;
- The public API (Bearer token) exposes only `GET /timeline` for now; writes come later.

---

## 10. Frontend

Route `/` (the stream), with the file browser demoted to `/files`; on mobile the stream is the main entry point. The UI never says "timeline / event / moment" — only months, cards, and the data band.

```
┌───────────────────────────────────────────────────────────┐
│  Everything About Me      [时间线] [文件] [搜索]    账号  │
├───────────────────────────────────────────────────────────┤
│  2025                                                     │
│  ├ 6 月  · 1,284 个文件 · 6 个片段            [整理中 ⏳] │
│  │  ┌─────────────────────────────────────────────────┐  │
│  │  │ [hero 封面]  西湖 · 09:12–17:40 · 9 张           │  │
│  │  │ [图][图] 另有 78 张相似 →                         │  │
│  │  └─────────────────────────────────────────────────┘  │
│  │  ┌──────────┐ ┌──────────┐  ← 横向紧凑卡（周报/异常）  │
│  │  └──────────┘ └──────────┘                            │
│  │  ┌─────────────────────────────────────────────────┐  │
│  │  │ [图][图][图]  家 · 生日 · 3 人                     │  │
│  │  └─────────────────────────────────────────────────┘  │
│  ├ 5 月  · 428 个文件 · 2 个片段                          │
│  └ 4 月  · 0 个文件（空白行，可点开）                     │
└───────────────────────────────────────────────────────────┘
```

- **Month headings stick**, with a year navigator on the right (a scrollbar minimap); `?m=2025-06` is shareable and survives refresh;
- **Skeleton**: during `indexed`, render the file density bars directly; `analyzed` replaces them month by month;
- **Card types**: hero / mosaic / series / metric / note / compact / gap; height is the cost;
- **Data band**: sticks to the left of the time axis on desktop, collapsible at the top on mobile; click to open a drawer with charts;
- **Interaction**: click through to moment detail (all files, sessions, map, health curves); long-press/menu to pin, hide, retitle, replace cover, merge;
- **"Why am I seeing this"**: a `?` in the corner of the detail page expands the scoring signals (transparency buys trust, and it doubles as a debug switch);
- **Recently organized**: a collapsible strip at the top of the page that lists only what was newly parsed in the last 7 days; it does **not** participate in the narrative layer's density budget;
- **Performance**: virtual scrolling by month, images with `loading="lazy"` + fixed aspect ratios (no jitter); the first screen signs URLs for only the first 6 covers of the current month;
- **Empty state/backfill**: show progress, estimated time, and an entry point for the 12 failed files; never leave a blank screen.

---

## 11. Implementation

| Item | Choice | Rationale |
|---|---|---|
| Migration | `sql/012_timeline.sql` | reuse the existing migration mechanism |
| Backend | `backend/internal/timeline/` (service / ingest / group / score / select / snapshot) | peer to `nodes` and `jobs`, independently testable |
| Worker | in-process goroutine pool + the `timeline_jobs` table | same single-instance assumption as `fs_jobs`; switch to heartbeats with multiple instances |
| Image metadata / thumbnails | prefer COS CI `imageInfo` + `imageMogr2/thumbnail/320x` | never download originals; the only realistic option at TB scale |
| Video | CI video info; fetch segments only when chapters are needed; `ffprobe` as the fallback | avoid local transcoding |
| Perceptual hash | 320px thumbnail + DCT pHash, pure Go | no GPU needed |
| Derived objects | `_derived/<user>/thumbs/<node_id>.jpg`, `mosaic/<moment_id>.jpg` | rebuildable, not in the `nodes` index, skipped by prefix during reconcile |
| Health parsing | streaming XML (Apple Health), CSV/JSON long tables; per-file cap + skip reason | ten years of export.xml can be gigabytes |
| Reverse geocoding | wire up a map API in P1; MVP uses folder/filename + manual titles | keep external dependencies in check |
| Titles | templates (place · time of day · activity); in P2 an LLM writes one sentence, cached | never blocks, always falls back |
| Tests | fixture library (1 video/day, 500 photos/day, daily health, a trip, an empty month) + golden snapshots | density parameters must be regression-tested |

Parameters start as constants; in P1 they move to config / user preferences:

```
TIMELINE_ENABLED=false         # off by default, canary
TIMELINE_WORKERS=2
TIMELINE_MONTH_BUDGET=24
TIMELINE_PROFILE=standard      # calm|standard|rich
TIMELINE_PIPELINE_VERSION=1
TIMELINE_MAX_FILES_PER_MIN=120
```

### Performance and cost

- Read: one snapshot query per two months + N presigned URLs (N ≤ 30); P99 should be < 50ms;
- Write: one monthly recompute = read that month's facts + DP + upsert; under 1s for 10,000 files;
- Analysis: billed per file (CI requests + thumbnail storage); always show "about to process N files" before starting;
- Storage: a 320px thumbnail is ≈ 20–40KB, so 10,000 of them ≈ 300MB — acceptable; `_derived` can be wiped and rebuilt as a whole directory.

---

## 12. Phases

| Phase | Scope | Effort (est.) | Acceptance |
|---|---|---|---|
| **P0** | `source_modified_at`, `asset_facts`, `moments`, `timeline_jobs`, EXIF/basic metadata, sessionizing + dedupe + pHash, scoring v1, monthly snapshots + DP selection, the `/timeline` page (hero/mosaic/gap/month headings), backfill status, file density heatmap | 1.5–2 weeks | import 10,000 photos and get a timeline within 10 minutes; 500 photos in a day = 1–2 moments |
| **P1** | series/video, ambient data band + health parsing, cross-day trip merging, source policies, user edits (pin/hide/retitle/merge), cross-source fusion (GPS+health), pre-composed mosaic | 2–3 weeks | a daily vlog takes 1 card; health data produces no cards |
| **P2** | face clustering, OCR/scene tags, LLM titles and summaries, anniversary recaps, sharing, public API | ongoing | titles are clearly more readable, and can be switched off in one click |

**The P0 trade-off**: get "structure + judgment + backfill" right; visually, heuristic titles (`West Lake · afternoon · 87 photos`) are enough.

---

## 13. Rejected approaches

| Approach | Why not |
|---|---|
| A per-day list truncated to the first N | at 500 photos the truncation is random, and with vlogs it's too empty; cross-day events are inexpressible |
| One global value threshold | density gets out of control: a month full of high scores explodes, a month full of low scores vanishes |
| A fixed N items per day | it forces the algorithm to "pad the numbers", and junk crowds out what is genuinely interesting |
| No snapshots, compute on read | slow, expensive, different on every refresh, and it makes backfill progress and user edits impossible |
| Lead with full ML/LLM | expensive and slow, and it cannot explain "why this one"; bring it in as a bonus in P2 |
| Make the timeline a running log of every file | 80% of a drive is collected material, which drowns the real memories; a running log is what the file browser is for |
| Generate a card per day of health data | 365 noise items a year; observations belong on a data band, not on cards |
| Show only what's "worth it", with no full entry point | the user suspects data loss; every layer must expand back to the original files |

---

## 14. Risks

1. **Untrustworthy time**: the mtime of WeChat/downloaded images is the download time, and EXIF may be stripped. → `time_confidence` + an unconfirmed list + manual time editing.
2. **Time zones/daylight saving**: traveling across zones drifts days. → store `local_date` at the shooting location and never recompute days for display.
3. **Duplicate imports**: the same batch uploaded once from the phone and once from the desktop (different paths, possibly different etags). → pHash dedupe + content hash; show only one copy.
4. **Screenshot noise**: phone screenshots are 30% of a camera roll. → filter by default + user-adjustable.
5. **Privacy**: health, location, and faces are all highly sensitive. → local processing, folder-level opt-out, no third parties by default, the timeline is private by default, and derived data can be deleted by directory.
6. **Runaway cost**: CI calls across a million files. → tiering (metadata before images), rate limits, estimates up front, pausable.
7. **Aesthetic drift**: the algorithm's "selections" don't match the user's taste. → user edits win, adjustable profile, transparent `why`, and in P1 favorites/hides nudge the weights.
8. **Multi-user fairness**: one big library drags down every worker. → round-robin queueing per user + a per-user rate cap.

---

## 15. Open questions

1. **Is the timeline the home page?** The home page is the file browser today. If the timeline becomes `/`, the file browser drops to a second entry point, and navigation and mental models both change; alternatively ship `/timeline` alongside it first.
2. **Content boundaries**: photos/videos + health only, or work documents too? The latter needs a "project/work" model, which is a different narrative from "experiences".
3. **Appetite for cloud AI**: titles, summaries, and aesthetic scoring would send photo content off-domain if they call an external API. Acceptable? Or stay with local heuristics only?
4. **Single-user or multi-user product**: it affects background scheduling fairness, quotas, and whether to build "share a timeline".
5. **Existing data scale**: is the bucket empty, a few thousand files, or already TB-scale? Backfill priority and cost control differ completely.
6. **Health data sources**: Apple Health exports, a band's CSV, or one manual file per day? The parser's scope varies enormously.

---

## 16. Implementation status (P0)

Shipped:

- `sql/012_timeline.sql`: `nodes.source_modified_at`, `timeline_assets`, `timeline_moments`, `timeline_snapshots`, `timeline_jobs`, `timeline_series` / `timeline_points`;
- `backend/internal/timeline/`: `HeuristicAnalyzer` (path dates + type + screenshot filtering), `SessionGrouper` (sessions split by day + 25 minutes, daily videos become series), `HeuristicScorer`, `TemplateNarrator`, `CSVAmbientExtractor`, monthly budget selection, snapshot reads and writes, three kinds of background job: `scan` / `backfill` / `month`;
- Pluggable interfaces: `Analyzer`, `Grouper`, `Scorer`, `Narrator`, `AmbientExtractor`. When wiring in multimodal/LLM, swap the fields on `Engine`; nothing above changes;
- API: `GET /api/timeline`, `GET /api/timeline/status`, `POST /api/timeline/rebuild`;
- Frontend: `web/src/pages/StreamPage.tsx` (monthly stream, hero/mosaic/series/ambient/gap, infinite scroll, organizing progress, mobile);
- Automatic `Notify` after upload/delete/rename; automatic backfill at startup; a sweeper as a safety net every 20 minutes.

Not done yet (P1/P2):

- EXIF capture time, GPS, scenes/faces/aesthetics, video key frames;
- cross-day trip merging, source policies (folder-level timeline/ambient/off), user edits (pin/hide/merge);
- LLM titles and summaries, budget profiles, pre-composed collage images;
- once multimodal lands, `pipelineVersion` must be bumped by 1 to trigger a recompute.

UI screenshots (synthetic data): [desktop](stream-desktop.jpg) · [mobile](stream-mobile.jpg).

---

## 17. Event cards (v0.3 · current direction)

> Status: 2026-10 · **shipped** (`web/src/pages/StreamPage.tsx`) · clickable mock: [mocks/timeline.html](mocks/timeline.html)
> Screenshots: [desktop](mocks/shots/timeline-desktop.png) · [text events + health change](mocks/shots/timeline-events.png) · [mobile](mocks/shots/timeline-mobile.png) · [dark](mocks/shots/timeline-dark.png)

Conclusion: **the timeline is one flat layer of event cards**. No dashboard, no year/month hierarchy.
The chronicle layer in §2, the budget in §4, and the scoring in §5 can stay in the backend, but they are no longer drawn in front of the user.

### 17.1 Page structure

- A single **flat list** from top to bottom, containing only event cards;
- Each card carries the full date (including the year), newest first;
- No year headings, no month headings, and no statistics like "N files / N events";
- Days without events take up no space.

### 17.2 Cards are typed

Four built-in types: `photo` (1–4 images + one sentence), `note` (text only), `metric` (one row of numbers + one sentence),
`html` (the app's own layout, rendered in a sandbox with design tokens injected, may follow the light/dark theme).
Only three things are shared: a title, a time, and an optional detail link. The type list and payloads are in [events.md](events.md).

### 17.3 Health: speak only when something changed

Having data every day does not mean looking at it every day. Health surfaces one card only when something **actually changed**,
for example "August averaged 6.7 hours, 42 minutes less than the previous three months".

No monthly charts, and no annual overview panel — an "annual overview" would bring the "year" hierarchy right back, conflicting with 17.1, so it is out for now.
Daily averages stay in the files; click through when you want them.

### 17.4 Two rejected versions

**v0.2 (dashboard)**: a year band + three month-head tracks (moment track / file track / data band) + a 31-cell density strip + an archive panel on the right + an archive heatmap.
The information was complete, but a page with four chart systems requires learning the charts before reading the page.

**The first v0.3 (hierarchical)**: year → month → event, three levels, a year band with one overview, month labels carrying file and event counts.
Once the hierarchy, the statistics, and the charts were gone, the page held nothing but cards — scrolling has no pauses, and the card is the only unit of content.

The cost: "which month was quiet" and "how much was there this year" are no longer visible at a glance. When you need that, solve it with search or filters, not with a chart.

### 17.5 Where events come from

Three sources write to the same table: the site itself (`native`), the file pipeline clustering automatically (`auto`), and third-party apps writing with a token (`app`).
Cards have a type (`photo` / `note` / `metric` / `html`); the source appears only on the date line.
**There is no editing**; there is no "view details" button either (when `detail_url` exists the title is the link, otherwise it isn't clickable).
The schema, write API, and lifecycle are in [events.md](events.md).

Still undecided: merging — should a day's photos and a run combine into one item (P2; only the `merged_into` field exists so far).

## Appendix: one-line summary for my future self

> Timeline = one flat layer of event cards (the lossy narrative layer) + detail (lossless, always at hand);
> no year/month headings, no statistics; health is not a permanent fixture, it speaks up only when something changed; no editing.
