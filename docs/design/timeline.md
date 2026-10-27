# 时间线（Timeline）设计草案

> 状态：v0.1 · 2026-10 · 待讨论
>
> 一句话：时间线不是「按日期排的文件列表」，而是一份**有编辑取舍的个人编年史**。
> 文件是证据，片段（moment）才是时间线的单位。
>
> **2026-10 更新**：落地以 §17 与 [events.md](events.md) 为准。
> §3 的 `moments` / 快照、§4 的月预算、§8 的用户编辑，都没有实施、也不打算实施（见 §17.4）。

---

## 1. 定位与目标

### 1.1 它解决什么问题

用户把十年的照片、视频、健康导出、文档倒进网盘，得到的是一个**越来越难浏览的目录树**。
他想看的不是「2019/05/12 有 437 个文件」，而是「2019 年 5 月，我去了趟青海」。

时间线的职责：

1. **把文件还原成经历**：按事件时间聚合、去重、挑代表，变成可读的片段；
2. **控制叙述密度**：不是全量展示，也不是随机截断，而是「值得看的占版面，不值得的折起来」；
3. **永远可追溯**：任何被折叠的东西都能一层层展开回原始文件，不制造数据丢失的错觉；
4. **自动回流**：新上传和旧文件 backfill 都走同一条流水线，用户不用手动「整理」。

### 1.2 非目标

- 不做「所有文件的流水账」。那是文件浏览器的职责，时间线只保留编年摘要 + 完整入口。
- 不做社交 feed，不做打卡，不做「你已连续记录 87 天」这种催促。
- 不在读路径上做分析（缩略图、EXIF、聚类都必须预计算）。
- 第一版不追求 AI 审美 / 人脸识别；先把结构和取舍做对，ML 是增益项而不是地基。

### 1.3 最小单位不是「天」

这是整个设计里最关键的一个决定。

如果以「天」为单位，一天 500 张照片和一天 1 个视频都会变成一张日期卡，要么爆炸要么空洞。
所以时间线的最小单位是 **片段（moment）**：

- 一个片段有开始/结束时间，可以**跨天**（一趟 5 天的旅行 = 1 个片段）；
- 一个片段由若干文件 +（可选）其他来源的证据组成（健康、GPS、文档）；
- 时间线是「片段 + 空白」的序列，不是「日期 + 文件」的列表。

天只是编年层的桶；**没有片段的日子直接压缩成一段空白**，不占版面。

---

## 2. 三层结构

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

三条原则：

1. **叙事层有损，编年层和明细层无损。** 时间线可以只展示 6 个片段，但月标题必须写清楚这个月有多少文件；点进去必须能看到全部。
2. **片段可以跨天、可以多来源。** 一趟旅行、一次装修、一段连续的健康数据，都应该是一个叙事单元，而不是按天切碎。
3. **空白是信息。** 三个月没有值得看的东西，就显示一条压缩的空白行，而不是用低价值内容填满。

---

## 3. 数据模型

### 3.1 先补一个现有缺口：文件时间

现在 `nodes` 只有 `created_at / updated_at`（都是「写进索引的时间」）。
`reconcile` 导入十年前的文件时，`created_at` 全是今天 —— 时间线会以为所有历史文件都是今天上传的。

必须补：

```sql
-- 对象在存储侧的最后修改时间（COS List/HeadObject 的 LastModified）
-- 它不是"拍摄时间"，但是唯一可靠的文件自身时间，兜底用
alter table nodes add column if not exists source_modified_at timestamptz;
```

`reconcile` / 上传确认 / API PUT 都要写入。这是 P0 的前置改动，否则 backfill 出来的时间线全是错的。

### 3.2 表结构

```sql
-- 每个文件的时间与可展示事实。媒体、文档、数据文件都有一行。
create table if not exists asset_facts (
  user_id        uuid not null references users(id) on delete cascade,
  node_id        uuid not null references nodes(id) on delete cascade,
  event_time     timestamptz,          -- 事件时间：EXIF > 文件名日期 > mtime > 入库时间
  local_date     date,                 -- 拍摄地本地日期，分桶用（跨时区不串天）
  tz             text,                 -- 拍摄地时区，如 Asia/Shanghai
  time_source    text not null,        -- exif | filename | mtime | ingest
  time_confidence real not null default 0.5,
  media_type     text,                 -- photo | video | audio | doc | data | other
  width int, height int, duration_ms bigint,
  gps_lat double precision, gps_lon double precision,
  quality        real,                 -- 清晰度/曝光/构图启发式，0..1
  phash          bit(64),              -- 感知哈希，去重用
  is_screenshot  boolean not null default false,
  duplicate_of   uuid references nodes(id) on delete set null,
  content_sig    text,                 -- etag + size + pipeline_version，幂等用
  analyzed_at    timestamptz,
  pipeline_version int not null default 1,
  primary key (user_id, node_id)
);
create index asset_facts_day on asset_facts (user_id, local_date);
create index asset_facts_time on asset_facts (user_id, event_time);

-- 时间线的原子单元
create table if not exists moments (
  id             uuid primary key default gen_random_uuid(),
  user_id        uuid not null references users(id) on delete cascade,
  dedupe_key     text not null,        -- 重算时用来认领旧片段，保持 id / 深链稳定
  kind           text not null,        -- trip | outing | session | series | metric | milestone | note
  starts_at      timestamptz not null,
  ends_at        timestamptz not null,
  local_date     date not null,        -- 叙事锚点日（starts_at 的本地日）
  title          text,
  subtitle       text,
  cover_node_id  uuid references nodes(id) on delete set null,
  item_count     int not null default 0,
  source_kinds   text[] not null default '{}',  -- {photo,health,gps,doc} 多源证据
  score          real not null,        -- 价值 0..1
  cost           int not null,         -- 展示成本（单位见 §4）
  layout         text not null,        -- hero | mosaic | strip | series | metric | note | compact
  reason         jsonb not null default '{}',   -- 评分依据，可解释 + 可调试
  evidence       jsonb not null default '{}',   -- 融合了哪些别的来源
  state          text not null default 'provisional', -- provisional|published|hidden|user_edited
  locked_fields  text[] not null default '{}',  -- title/cover/pin/hide，重算不覆盖
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

-- 目录级来源策略：照片走叙事、健康走数据带、证件照不上时间线
create table if not exists source_rules (
  id          uuid primary key default gen_random_uuid(),
  user_id     uuid not null references users(id) on delete cascade,
  path_prefix text not null default '',
  mode        text not null,           -- timeline | ambient | series | off
  options     jsonb not null default '{}',
  created_at  timestamptz not null default now(),
  unique (user_id, path_prefix)
);

-- 环境数据（健康、步数、体重、花费……先说健康）
create table if not exists metric_series (
  id       uuid primary key default gen_random_uuid(),
  user_id  uuid not null references users(id) on delete cascade,
  source   text not null,              -- health | apple-health | csv:xxx
  metric   text not null,              -- resting_hr | sleep_hours | steps
  unit     text,
  cadence  text,                       -- daily | hourly | irregular
  pinned   boolean not null default true,  -- 是否上数据带
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

-- 已发布的月快照：读路径只读这张表。
-- 它是 moments 的反规范化读模型（选中的片段 + 排序 + 卡片形状 + gap），
-- moments 才是事实源；快照可以随时从 moments 重新生成。
create table if not exists timeline_snapshots (
  user_id    uuid not null references users(id) on delete cascade,
  month      date not null,            -- 当月 1 号
  payload    jsonb not null,           -- 选中的片段 + 空白 + 数据带摘要
  coverage   text not null,            -- indexed | analyzed | curated
  file_count int not null default 0,
  moment_count int not null default 0,
  pipeline_version int not null,
  generated_at timestamptz not null default now(),
  primary key (user_id, month)
);
```

### 3.3 片段的重算 ≠ 删了重建

`moments` 是持久实体，不是每次重算都清空：

- 每个片段有一个 `dedupe_key`（通常是「锚点节点 id + 日期 + kind」的哈希）；
- 重算时按 `dedupe_key` 认领旧片段，只更新变化的字段；
- `locked_fields` 里的字段（用户改过的标题、封面、置顶、隐藏）不覆盖；
- 消失的片段标 `state='hidden'` 而不是删除，保留深链和用户操作记录。

这样时间线的 URL、收藏、用户编辑在重算和 backfill 之后都还是有效的。

---

## 4. 密度：恒定带宽 + 弹性分配

### 4.1 把「密度」变成一个可计算的预算

用户的直觉是「有价值的就多给点地方，没价值的少给点」。
落到实现上是两条约束：

- **全局带宽恒定**：不管这个月拍了 50 张还是 5000 张，一个月的展示高度上限差不多；
- **局部密度弹性**：带宽给谁，由价值决定，允许从一个月的安静日子借给高价值事件。

定义单位 `u = 72px`（一个紧凑行）：

| 卡片 | 成本 | 典型高度 | 用在哪 |
|---|---|---|---|
| hero | 6u | 432px | 单视频、旅行封面、大事件 |
| mosaic | 4u | 288px | 一次出游的 5–9 张精选 |
| series | 3u | 216px | 每日 vlog、连续剧集 |
| 普通 | 2u | 144px | 单张照片、一般片段 |
| 紧凑 | 1u | 72px | 周报、小里程碑 |
| 数据带 | 2u/月 | 一条边栏 | 健康等环境数据 |

每月预算 `B` 默认 24u（约 2.2 屏），提供 `calm 14 / standard 24 / rich 36` 三档。
**预算是上限，不是目标** —— 没有值得看的东西就空着。

### 4.2 选择算法

对每个月做一次小型背包：

```
输入：候选片段 m_i，价值 v_i ∈ [0,1]，成本 c_i ∈ {1,2,3,5,6}
约束：
  Σ c_i ≤ B_month
  单日 ≤ 3 个片段，且 ≤ 12u
  v_i ≥ τ（τ = 0.35；整月为空时放宽到 0.25，最多 1 张紧凑卡）
  v_i ≥ 0.75 时最多从相邻两个月借 12u（本月上限 1.5B）
目标：最大化 Σ v_i（同源/同时段做多样性抑制，见 §5.3）
```

- 实际实现是**贪心 + 递减的最小间隔**（2 天 → 1 天 → 0 天）：先按价值选，普通内容尽量散开，高价值内容（≥0.75）允许挨着；
- 成本档位少、预算小，也完全可以换成精确 DP；现在优先保证"同质内容不会挤在一起"，因为时间线的节奏比多塞一条更重要；
- 多样性抑制在选择前做：同一会话的候选先合并成一个，近重复的直接剔除；
- 顺序永远是时间序，选择只决定「谁出现、以多大成本出现」。

### 4.3 它自然产生的行为

| 情况 | 结果 |
|---|---|
| 普通月份（每周零星照片） | 2–4 个普通片段，+ 月标题，不硬凑 |
| 旅行月（5 天拍 2000 张） | 3 个 hero/mosaic（含跨天 trip 卡），其余日期压缩 |
| 每天一个 vlog | 1 张 series 卡（3u）+ 可能 1 个精选单集，不占 30 张卡 |
| 每天 500 张照片 | 按会话聚成 1–3 个片段，每片段 1 张 mosaic，不是 500 行 |
| 每天健康数据 | 1 条数据带（2u/月）+ 周报 + 少量异常卡 |
| 三个月没内容 | 一条压缩空白行 + 文件计数，点开看全部 |

### 4.4 保底与反转

- **保底**：一个索引里有内容、但没有任何片段达到 τ 的月份，最多显示 1 张紧凑卡（标题用「N 个文件」），避免时间线看起来坏了；
- **借带宽**：高价值事件（婚礼、毕业、长途旅行）允许超过当月预算，从前后月份借，最多 1.5 倍；
- **当月豁免**：最近 14 天的片段在「最新」区可以全部出现（用户刚上传，要能看到），但叙事层仍按预算走。这两个东西在 UI 上分开（见 §8）。

---

## 5. 价值评分：什么算「有趣」

### 5.1 评分框架

```
v = clamp( 0.9·内容 + 0.5·结构 + 0.7·情境 + 0.5·人际 + 0.8·人工 + 0.5·多源加成 - 惩罚 )
```

每项先归一化到 0..1，权重先拍脑袋，用固定 fixtures 校准（见 §12）。

| 组 | 信号（第一版可算的） | 说明 |
|---|---|---|
| 内容 | 分辨率、清晰度、曝光、时长、文档页数/字数、视频音量起伏 | 不追求审美，先排除废片 |
| 结构 | 会话规模、连拍比例、时段罕见度、文件夹语义（旅行/婚礼/毕业）、文件名语义 | 500 张本身就是一个信号 |
| 情境 | 离家距离、新地点（相对个人历史）、海拔爬升、与健康数据共现、距上次同类事件的时间 | 「第一次/很久没做」权重高 |
| 人际 | 人脸数量（不做识别）、合影比例、自拍比例 | P2 再上人脸聚类 |
| 人工 | 收藏、标签、备注、置顶、分享；隐藏/「不上时间线」为负 | 用户显式信号优先级最高 |
| 多源 | 同一时段有照片 + GPS + 心率 + 文档 → ×(1 + 0.15·(n-1)) | 时间线是「生活事实的 join」 |
| 惩罚 | 截图、表情包、收据、安装包、备份、node_modules、缓存、近重复 | 直接压到 τ 以下 |

每条片段把 `reason` 存下来（哪些信号、各多少分），用途有三：调试密度、向用户解释「为什么看到这个」、以及 P2 让 LLM 写标题时作为输入。

### 5.2 分类先于评分

评分之前，先判断**这个文件/目录属于哪一类叙事**，因为不同类的处理方式完全不同：

| 类型 | 例子 | 时间线处理 |
|---|---|---|
| 经历 episodic | 照片、视频、旅行、聚会 | 聚成片段，参与预算 |
| 周期 ambient | 健康、步数、睡眠、位置轨迹、行车记录仪 | **不进片段**，进数据带；只出聚合和异常 |
| 系列 series | 每日 vlog、连续剧、播客 | 1 张系列卡，不按集数占版面 |
| 收藏 collected | 下载的 PDF、安装包、代码、备份、证件 | 默认不上叙事层；只计入编年层文件数 |
| 作品 work | 项目目录、设计稿、文档迭代 | P2：按「项目活跃期」成一个里程碑 |

「收藏」和「经历」的区分很重要：网盘里 80% 的文件是收藏，把它们塞进时间线会让真正的记忆被淹没。
默认规则可以从路径/扩展名/大小推断，用户可以在文件夹上右键改（`source_rules`）。

### 5.3 会话与去重

**照片（P0 核心）**：

1. 按 `event_time` 排序；
2. 切会话：`Δt > 25min` 或 位移 > 3km 或（P2）场景向量距离过大；
3. 会话内去重：`Δt < 3s` 且 phash 汉明距离 ≤ 8 → 同组；只保留质量最高的那张，其余记 `duplicate_of`；
4. 代表帧：封面 = 质量 × 人脸数 × 中心度最高；网格用 MMR 选 4–9 张（λ=0.7，避免 9 张一样的）；
5. 跨天合并成 trip：连续几天都在「离家 > 50km 且同一方向」，间隔 < 24h → 一个 `trip` 片段，内部按天分章节（明细层）。

**视频**：

- 短视频（< 3min）：按普通片段处理，封面 = 首帧/关键帧 + 时长；
- 长视频：按会话聚类，卡片显示时长和章节数（章节从场景切换里切，P1）；
- 每天固定一个、时长相近、机位固定 → **ambient**（监控/行车记录仪），出「视频带 + 运动/人声高光」，不是 365 张卡；
- 每天固定一个、时长和内容变化 → **series**，1 张系列卡 + 精选单集。
  P0 已实现基础判断：同一目录、跨 ≥7 天、≥7 个视频 → 一张系列卡（`SessionGrouper`）。

**健康数据**：

- CSV / JSON / Apple Health `export.xml` / FIT / TCX → 解析到 `metric_points`（按天聚合）；
- 同一份数据重复导入：按 content hash + 日期范围幂等 upsert；
- 文件本身**永远不出现在时间线上**，只在月标题的「N 个数据文件」里可点开。

---

## 6. 三个重点场景

### 6.1 一天一个视频

```
2025 年 6 月 · 30 个视频 · 1 个系列
┌─────────────────────────────────────────────────┐
│ [封面 16:9]  每日一镜 · 6 月                     │
│ 30 集 · 平均 4 分 12 秒 · 最近 6/30 第 181 天    │
│ ▸ 6/30 海边   [▣][▣][▣][▣][▣]        查看 30 集 → │
└─────────────────────────────────────────────────┘
```

- 先判「系列」还是「环境」：主题/时长/机位变化 → series；几乎不变 → ambient。
- 系列卡只占 3u，一整年的日更也只占一个月的 1/8 带宽。
- 单集不逐条出现；「本月最佳一集」可以额外占 1 张普通卡（按价值选），给一个进入点。
- 如果用户就是想逐条看：点进系列 → 剧集列表（月份分组、时长、封面），或者直接用文件浏览器。

### 6.2 一天 500 张照片

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

- 500 张不是「500 个条目」，也不是「随便取前 9 张」：先会话化（上午西湖 / 晚上家），会话内去重 + MMR 选代表。
- 一天最多 3 个片段（12u），超出的会话折叠进明细层，用「另有 N 张」明确告知。
- 卡片上永远有 `N 张 → 9 张` 的诚实标注，点击进「这天」的全量网格，被选中的有标记。
- 近重复（连拍、同一场景）默认合并，显示「78 张相似」而不是散落 78 行。
- （P1）服务端预合成一张 mosaic 封面图，移动端只发 1 个图片请求，而不是 9 个预签名 URL。

### 6.3 每天的健康数据

健康数据是**观测**，不是**事件**。它不进叙事层，而是成为时间轴上的「数据带」：

```
2025 年 6 月 ────────────────────────────────
静息心率  ▁▂▁▂▃▂▁▂▁▁▂▁▂▁▂  54 bpm  ×
睡眠      ▅▄▅▃▄▅▄▃▅▄▄▅▄▄▅  7.1 h   ×
步数      ▃▅▂▇▄▃▅▂▄▅▃▆▄▃▅  6.8 k   ×

┌ 6/12 · 连续 3 天静息心率 > 60（基线 54） → 看看 ┐   compact
└ 6/23 · 月报：睡眠 -0.4h，步数 +12%              ┘   compact
```

- **数据带**：固定在时间轴左侧的 24–40px 窄栏（移动端折叠成顶部可展开条），按周/月画迷你 sparkline，颜色用**相对个人基线的 z-score**，不是绝对值；
- **周报/月报**：只在「有变化」时出现（变化 < ε 就不出现），1u；
- **异常卡**：连续 3 天偏离基线、破纪录、数据中断 > 3 天，才升格成一张普通片段，带证据和「看看」；
- 原始日文件永远不单独成卡；月标题里有「N 个数据文件」入口；
- 导入十年的数据时，数据带应该**静默补齐历史**，不产生「你新增了 3,650 个文件」的通知；
- 数据带是通用机制，以后可以接体重、花费、屏幕时间、听歌记录，不需要改时间线本身。

**跨源融合才是终局**：同一天的照片 + GPS + 心率，可以合成一个普通卡做不到的片段：

```
┌ 6/15 · 西湖徒步 · 12.6km · 爬升 620m ────────────┐
│ [3 张精选]  平均心率 132 · 最高 168 · 4 小时 12 分 │
│ 照片 87 张 · 轨迹 1 条 · 健康 3 项                │
└──────────────────────────────────────────────────┘
```

这也是这个产品该有的样子：时间线不是一个相册，而是**把一个人散落在各类文件里的生活事实 join 起来的地方**。
照片提供画面，GPS 提供地点和轨迹，健康数据提供强度，文档提供上下文。单源也能用，多源时价值乘 1.15^(n-1)。

### 6.4 其它几个容易踩的场景

| 场景 | 处理 |
|---|---|
| 一天横跨多个城市/景点 | 按会话切，最多 3 个片段；同类会话合并成「A 和 B」 |
| 旅行 5 天 | 1 个 trip 片段（跨天），明细层按天分章节 |
| 婚礼拍了 3000 张 | 1 个 hero + 1 个 mosaic（仪式/宴席），其余进明细 |
| 截图 / 表情包 / 收据 | 默认 filtered，不计入叙事层，但保留在编年层计数 |
| 证件、合同、安装包 | collected，默认不上时间线 |
| 每月一次的对账单 | ambient 或 series（按文件形态判断），不逐张 |
| 十年的照片一次性导入 | 先出「文件密度热力图」的编年层（秒级），再逐月精细化 |
| 同一天上传旧文件 | 事件时间归到旧日期，同时「最近整理」里有一条轻提示 |
| 时区 / 跨天 | 用拍摄地本地日期分桶；EXIF 没有时用 mtime 转用户主时区，标低置信度 |
| 文件时间不可信（下载的旧图、微信图片） | `time_confidence` 低 → 不进叙事层，进「时间待确认」列表 |

---

## 7. Backfill：自动、增量、可解释

### 7.1 触发点

| 来源 | 触发 |
|---|---|
| 网页上传确认 | `upload/confirm` 成功后入队 |
| 对外 API PUT | 写入索引后入队 |
| `reconcile`（COS 清单 / coscli 灌库） | 批量入队，按 etag 去重 |
| 用户改文件夹策略 | 相关月份重算 |
| 流水线版本升级 | 后台惰性重算，不阻塞读 |

### 7.2 三级推进，「先粗后细」

用户在导入 2TB 数据时，不能等分析完才看到时间线：

| 阶段 | 输入 | 产出 | 成本 |
|---|---|---|---|
| **indexed**（秒级） | `nodes` + `source_modified_at` | 每天的**文件密度热力图** + 月计数 + 编年层 | 纯 SQL |
| **analyzed**（分钟–小时） | 图片/视频元数据、缩略图 | 片段、封面、评分 | 按文件数，受速率限制 |
| **curated**（持续） | 用户编辑、跨源融合、ML | 稳定的精选结果 | 增量 |

所以「backfill 自动发生」的正确体验是：**先看到十年的轮廓（哪里有东西、哪里是空的），再看到细节逐月长出来**。而不是一个转圈。

### 7.3 任务队列

```sql
create table if not exists timeline_jobs (
  id bigserial primary key,
  user_id uuid not null,
  kind text not null,              -- probe | group | fuse | publish
  node_id uuid,                    -- probe 用
  month date,                      -- publish 用
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

- Go 后台 worker（2–4 并发），`FOR UPDATE SKIP LOCKED` 抢任务，`lease_until` 防卡死；
- 失败指数退避，超过 5 次进 dead-letter（保留原因，UI 显示「12 个文件无法解析」）；
- 全局速率限制（COS / 万象 QPS），单用户公平调度，避免一个大库饿死其他用户；
- 默认**最近 12 个月优先**（默认视图先完整），然后从近到远回填；用户可以对某个月点「优先整理」；
- 上传风暴去抖：一次 500 张上传只在最后一个事件后 30s 触发一次月重算。

### 7.4 幂等与可重算

- `content_sig = etag + size + pipeline_version`；处理结果写回 `asset_facts`，重复任务直接跳过；
- 月快照整体替换（一个事务里 upsert `timeline_snapshots`），读路径永远看到完整旧版或完整新版，不会读到半成品；
- 重跑必须得到**逐字节相同**的快照（同样输入 + 同版本），用 golden test 保证；
- 用户删除文件 → 级联删 `asset_facts` / `moment_items`，下一次月重算会自动调整；derived 缩略图按 node_id 删除。

### 7.5 覆盖状态要能看见

每个月、每个来源都要有覆盖状态。稀疏的 2013 年和「还没整理」的 2013 年，在 UI 上必须长得不一样，否则用户会以为数据丢了：

```
2025 年 6 月 · 1,284 个文件 · 6 个片段        ✅ 已整理
2013 年 3 月 · 2,914 个文件 · 整理中 43%      ⏳ 预计 6 分钟
2013 年 2 月 · 0 个文件                       — 没有内容
```

`GET /api/timeline/status` 返回 `{coverage, progress, throughput, eta, failed}`。

---

## 8. 稳定性：不要每次刷新都换一批

时间线一旦开始「每次看都不一样」，用户就不会信任它。规则：

1. **已发布的月快照不可变**，只在新内容 / 版本升级 / 用户编辑时重算；
2. **陈旧月份冻结**：> 30 天的月份不因为后台补细节而重排；只有当新片段价值比已选的边际片段高 **15% 以上**（hysteresis）才替换；
3. **确定性**：所有并列打分用 `hash(user_id, month, moment_id)` 决胜，不引入随机；
4. **用户编辑是最高优先级**：置顶/隐藏/改标题/换封面写进 `locked_fields`，重算只增不减；
5. **重算可撤销**：`pipeline_version` 变了先影子生成，用户点「重新整理」再切换；
6. **读路径只读快照**：任何分析都在后台完成，页面不会因为一张坏图卡住。

---

## 9. 接口

```http
GET /api/timeline?before=2025-06-01&months=2&tz=Asia/Shanghai
→ {
    "periods": [{
      "month": "2025-06-01",
      "file_count": 1284,
      "coverage": "analyzed",
      "generated_at": "...",
      "items": [
        {"type":"moment","id":"...","layout":"mosaic","title":"西湖 · 徒步","status":"published",
         "starts_at":"...","ends_at":"...","cost":5,
         "cover":{"path":"Photos/2025/06/15/a.jpg","thumb":"<signed>","w":1200,"h":900},
         "grid":[{"path":"...","thumb":"<signed>"}, ...],
         "more": 78, "item_count": 87, "sources":["photo","gps"],
         "why":"新地点 + 87 张 + 海拔爬升 620m"},
        {"type":"ambient","metrics":[{"key":"resting_hr","label":"静息心率","unit":"bpm",
         "values":[54,55,...],"baseline":54,"delta":"+2"}]},
        {"type":"gap","from":"2025-05-20","to":"2025-06-01","file_count":214}
      ]
    }],
    "next": "2025-04-01",
    "backfill": {"running":true,"from":"2014-01","progress":0.43,"eta_seconds":360}
  }

GET /api/timeline/moments/{id}        → 片段详情（全部文件、会话、证据、来源链接）
GET /api/timeline/day/2025-06-15      → 这一天的全量网格（完整性入口）
GET /api/timeline/status              → 覆盖 / 进度 / 失败 / 版本
PATCH /api/timeline/moments/{id}      → {title?, cover?, pinned?, hidden?}
POST /api/timeline/moments/{id}/merge → {ids:[]}
POST /api/timeline/moments/{id}/split → {day?}
POST /api/timeline/rebuild            → {from,to}（owner only）
GET  /api/timeline/rules             → 来源策略
PUT  /api/timeline/rules             → 目录级 timeline|ambient|series|off
```

要点：

- 以**月**为分页单位（`before` + `months`），因为密度是月级预算，按月取才能保证每页高度一致；
- 缩略图 URL 在**响应时**预签名（10 分钟），快照里只存 path；
- 失败降级：快照缺失时返回 `coverage: "indexed"` + 文件热力图，前端显示骨架，而不是 500；
- 对外 API（Bearer token）先只给 `GET /timeline`，写操作以后再说。

---

## 10. 前端

路由 `/`（内容流），文件浏览器退到 `/drive`；移动端内容流就是主入口。界面上不出现"时间线/事件/片段"，只有月份、卡片和数据带。

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

- **月分组吸顶**，右侧年份导航（滚动条 minimap），`?m=2025-06` 可分享/刷新；
- **骨架**：`indexed` 阶段直接渲染文件密度热力条，`analyzed` 逐月替换；
- **卡片类型**：hero / mosaic / series / metric / note / compact / gap，高度就是成本；
- **数据带**：桌面贴左侧时间轴，移动端顶部可折叠；点开抽屉看图表；
- **交互**：点击进片段详情（所有文件、会话、地图、健康曲线）；长按/菜单置顶、隐藏、改标题、换封面、合并；
- **「为什么看到这个」**：详情页角落一个 `?`，展开评分信号（透明度换信任，也是调试开关）；
- **最近整理**：页面顶部一条可折叠条，只列最近 7 天新解析出的内容，**不参与**叙事层的密度预算；
- **性能**：按月虚拟滚动，图片 `loading="lazy"` + 固定宽高比（避免抖动）；首屏只签当前月前 6 个封面的 URL；
- **空态/backfill**：显示进度、预计时间、失败的 12 个文件入口，永远不留白屏。

---

## 11. 技术实现

| 项 | 选择 | 理由 |
|---|---|---|
| 迁移 | `sql/012_timeline.sql` | 沿用现有迁移机制 |
| 后端 | `backend/internal/timeline/`（service / ingest / group / score / select / snapshot） | 与 `nodes`、`jobs` 平级，独立可测 |
| Worker | 进程内 goroutine pool + `timeline_jobs` 表 | 与 `fs_jobs` 同样的单实例假设；多实例时换心跳 |
| 图片元数据 / 缩略图 | 优先 COS 数据万象 `imageInfo` + `imageMogr2/thumbnail/320x` | 不下载原图，TB 级才现实 |
| 视频 | 万象视频信息；需要章节时才取片段；`ffprobe` 作为兜底 | 避免本地转码 |
| 感知哈希 | 320px 缩略图 + DCT pHash，纯 Go | 不需要 GPU |
| 派生对象 | `_derived/<user>/thumbs/<node_id>.jpg`、`mosaic/<moment_id>.jpg` | 可重建、不进 `nodes` 索引、reconcile 时按前缀跳过 |
| 健康解析 | 流式 XML（Apple Health）、CSV/JSON 长表；单文件上限 + 跳过原因 | 十年 export.xml 可能上 GB |
| 反向地理编码 | P1 再接地图 API，MVP 用文件夹/文件名 + 手工标题 | 控制外部依赖 |
| 标题 | 模板（地点 · 时段 · 活动），P2 用 LLM 生成一句话 + 缓存 | 不阻塞、可回退 |
| 测试 | fixture 库（1 视频/天、500 照片/天、健康日更、旅行、空月）+ golden 快照 | 密度参数必须可回归 |

参数先做成常量，P1 再抽到配置 / 用户偏好：

```
TIMELINE_ENABLED=false         # 默认关，灰度
TIMELINE_WORKERS=2
TIMELINE_MONTH_BUDGET=24
TIMELINE_PROFILE=standard      # calm|standard|rich
TIMELINE_PIPELINE_VERSION=1
TIMELINE_MAX_FILES_PER_MIN=120
```

### 性能与成本

- 读：每两个月 1 条快照查询 + N 个预签名（N ≤ 30），P99 应 < 50ms；
- 写：一次月重算 = 读当月 facts + DP + upsert，1 万文件量级 < 1s；
- 分析：按文件计费（万象请求 + 缩略图存储），先给用户看「预计处理 N 个文件」再开跑；
- 存储：缩略图 320px ≈ 20–40KB/张，1 万张 ≈ 300MB，可接受；`_derived` 可整目录清理重建。

---

## 12. 分期

| 阶段 | 内容 | 工作量（估） | 验收 |
|---|---|---|---|
| **P0** | `source_modified_at`、`asset_facts`、`moments`、`timeline_jobs`、EXIF/基本元数据、会话化 + 去重 + pHash、评分 v1、月快照 + DP 选择、`/timeline` 页（hero/mosaic/gap/月标题）、backfill 状态、文件密度热力图 | 1.5–2 周 | 导入 1 万张照片，10 分钟内出时间线；一天 500 张 = 1–2 个片段 |
| **P1** | series/视频、ambient 数据带 + 健康解析、trip 跨天合并、来源策略、用户编辑（置顶/隐藏/改标题/合并）、跨源融合（GPS+健康）、预合成 mosaic | 2–3 周 | 每天一个 vlog 只占 1 张卡；健康数据不产生卡片 |
| **P2** | 人脸聚类、OCR/场景标签、LLM 标题和摘要、周年回顾、分享、对外 API | 持续 | 标题可读性明显提升，且可一键关掉 |

**P0 的取舍**：先把「结构 + 取舍 + backfill」做对，视觉上用启发式标题（`西湖 · 下午 · 87 张`）就够。

---

## 13. 被否决的方案

| 方案 | 为什么不做 |
|---|---|
| 按天列表 + 截断前 N 个 | 500 张时截断是随机的，vlog 时又太空；无法表达跨天事件 |
| 全局一个价值阈值 | 密度不可控：某个月全是高分内容会爆炸，某个月全是低分会消失 |
| 每天固定 N 条 | 会逼着算法「凑数」，垃圾内容挤掉真正有趣的 |
| 不做快照，读时现算 | 慢、贵、每次刷新不一样，且无法做 backfill 进度和用户编辑 |
| 全量 ML/LLM 先上 | 贵且慢，而且解释不了「为什么选这张」；P2 再作为增益 |
| 把时间线做成所有文件的流水账 | 网盘 80% 是收藏，会把真正的记忆淹没；流水账用文件浏览器 |
| 给健康数据每天生成一张卡 | 一年 365 条噪音；观测应该用数据带，不是事件卡 |
| 只展示「值得」的，不给全量入口 | 用户会怀疑丢数据；必须每层都能展开回原始文件 |

---

## 14. 风险

1. **时间不可信**：微信/下载图片的 mtime 是下载时间，EXIF 可能被抹掉。→ `time_confidence` + 待确认列表 + 手动改时间。
2. **时区/夏令时**：跨时区旅行会串天。→ 存拍摄地 `local_date`，展示不跨天重算。
3. **重复导入**：同一批照片从手机和电脑各传一次（路径不同、etag 可能不同）。→ phash 去重 + content hash，两只保留一份展示。
4. **截图噪音**：手机截图占相册 30%。→ 默认过滤 + 用户可调。
5. **隐私**：健康、位置、人脸都是高敏感。→ 本地处理、文件夹级关闭、不默认第三方、时间线默认私有、可整目录删除派生数据。
6. **成本失控**：百万文件的万象调用。→ 分级（先元数据后图片）、速率限制、预估提示、可暂停。
7. **审美偏移**：算法选出的「精选」不合用户口味。→ 用户编辑优先、可调 profile、`why` 透明、P1 用收藏/隐藏反馈微调权重。
8. **多用户公平**：一个大库拖垮所有 worker。→ 排队按用户轮转 + 单用户速率上限。

---

## 15. 开放问题

1. **时间线是不是首页？** 现在的首页是文件浏览器。如果时间线成为 `/`，文件浏览器退成第二入口，导航和心智都会变；也可以先 `/timeline` 并存。
2. **内容边界**：只做照片/视频 + 健康，还是也要工作文档？后者需要「项目/作品」模型，和「经历」是两种叙事。
3. **云端 AI 的接受度**：标题、摘要、审美打分如果调用外部 API，涉及照片内容出域；可接受吗？还是只做本地启发式？
4. **单用户还是多用户产品**：影响后台调度的公平性、配额、以及要不要做「分享时间线」。
5. **现有数据规模**：现在桶里是空的、几千张，还是已经 TB 级？backfill 的优先级和成本控制会完全不同。
6. **健康数据来源**：Apple Health 导出、手环 CSV、还是每天一个手工文件？解析器的范围差别很大。

---

## 16. 实现状态（P0）

已落地：

- `sql/012_timeline.sql`：`nodes.source_modified_at`、`timeline_assets`、`timeline_moments`、`timeline_snapshots`、`timeline_jobs`、`timeline_series` / `timeline_points`；
- `backend/internal/timeline/`：`HeuristicAnalyzer`（路径日期 + 类型 + 截图过滤）、`SessionGrouper`（按天 + 25 分钟切会话、每日视频成系列）、`HeuristicScorer`、`TemplateNarrator`、`CSVAmbientExtractor`、按月预算选择、快照读写、`scan` / `backfill` / `month` 三种后台任务；
- 可插拔接口：`Analyzer`、`Grouper`、`Scorer`、`Narrator`、`AmbientExtractor`。接多模态/LLM 时替换 `Engine` 里的字段，上层不用改；
- 接口：`GET /api/timeline`、`GET /api/timeline/status`、`POST /api/timeline/rebuild`；
- 前端：`web/src/pages/StreamPage.tsx`（月度流、hero/mosaic/series/ambient/gap、无限滚动、整理进度、移动端）；
- 上传/删除/改名后自动 `Notify`；启动时自动回填；sweeper 每 20 分钟兜底。

还没做（P1/P2）：

- EXIF 拍摄时间、GPS、场景/人脸/审美、视频关键帧；
- 跨天 trip 合并、来源策略（文件夹级 timeline/ambient/off）、用户编辑（置顶/隐藏/合并）；
- LLM 标题和摘要、预算 profile、预合成拼贴图；
- 多模态接入后 `pipelineVersion` 需要 +1 触发重算。

界面截图（合成数据）：[桌面](stream-desktop.jpg) · [移动端](stream-mobile.jpg)。

---

## 17. 事件卡（v0.3 · 当前方向）

> 状态：2026-10 · **已落地**（`web/src/pages/StreamPage.tsx`）· 可点稿：[mocks/timeline.html](mocks/timeline.html)
> 截图：[桌面](mocks/shots/timeline-desktop.png) · [文字事件 + 健康变化](mocks/shots/timeline-events.png) · [移动](mocks/shots/timeline-mobile.png) · [深色](mocks/shots/timeline-dark.png)

结论：**时间轴是一层扁平的事件卡**。没有仪表盘，也没有年 / 月的层级。
§2 的编年层、§4 的预算、§5 的评分仍然可以留在后台，但不再画到用户面前。

### 17.1 页面结构

- 一条从上往下的**扁平列表**，只有事件卡；
- 每张卡自带完整日期（含年份），按时间倒序；
- 没有年标题、月标题，也没有「N 个文件 / N 个事件」这类统计；
- 没有事件的日子不占版面。

### 17.2 卡片是类型化的

内置四种：`photo`（1–4 张图 + 一句话）、`note`（纯文字）、`metric`（一行数字 + 一句话）、
`html`（应用自己的版式，沙箱渲染，注入设计 token，可以跟随明暗主题）。
共享的只有三样：标题、时间、一个可选的详情链接。类型清单和 payload 见 [events.md](events.md)。

### 17.3 健康：变了才说话

每天都有数据，不等于每天都要看。健康只在**真的变了**的时候出现一张卡，
例如「8 月平均 6.7 小时，比前三个月少 42 分钟」。

没有每月图表，也没有年度总览面板——「年度总览」会把「年」这个层级重新带回来，和 17.1 冲突，先撤。
日均数据留在文件里，要看再点进去。

### 17.4 被否决的两版

**v0.2（仪表盘）**：年带 + 月头三条轨道（片段轨 / 文件轨 / 数据带）+ 31 格密度条 + 右侧档案图 + 档案热力图。
信息是全的，但一页里有四套图表，读它之前得先学会图表。

**v0.3 初版（有层级）**：年 → 月 → 事件三级，年带一张总览，月标签带文件数和事件数。
层级、统计、图表都清掉之后，页面只剩卡片——滚起来没有停顿，卡片是唯一的内容单位。

代价：「哪个月安静」「这一年有多少」不再一眼可见。需要的时候用搜索或筛选解决，不要用图解决。

### 17.5 事件从哪来

三种来源写同一张表：站内自己写（`native`）、文件流水线自动聚（`auto`）、三方应用用 token 写（`app`）。
卡片有类型（`photo` / `note` / `metric` / `html`），来源只出现在日期那一行。
**没有编辑**；也没有「查看详情」按钮（有 `detail_url` 时标题才是链接，没有就不可点）。
表结构、写入 API、生命周期见 [events.md](events.md)。

还没定：合并——同一天的照片和跑步要不要合成一条（P2，只留了 `merged_into` 字段）。

## 附：一句话总结给未来的自己

> 时间线 = 一层扁平的事件卡（有损的叙事层）+ 明细（无损、随手可查）；
> 没有年/月标题，没有统计数字；健康不常驻，变了才说一句；没有编辑。
