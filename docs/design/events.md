# 事件（Event）：结构与来源

> 状态：v1.0 · 2026-10 · **P0 已落地**（实现状态见 §12）
> 界面见 [`timeline.md` §17](timeline.md)；这里只回答：**事件长什么样、谁能写、写进来之后归谁。**
> 现在还没有事件。站内会写一部分，三方应用会写一部分，先把契约定下来。

---

## 0. 一句话

一条事件 = **一段时间 + 一张卡（类型 + payload）+ 一个可选的详情链接**；来源只是归因。

---

## 1. 四个决定

### 1.1 时间线拥有事件，应用只是投稿

三方写进来的事件存在用户的账号里，不是应用的一张外链：

- 用户断开应用、应用下线、token 被撤销 —— 事件都还在，只是来源标记失效（`来源已断开`）；
- 用户删除写墓碑（`state='deleted'`），内容清空、去重键保留——否则流水线重跑或应用重推会把它复活；
- 应用调删除只冻结（`detached`），内容保留，不带走。

「个人编年史」和「第三方动态墙」的分界线就在这里。反过来做（事件属于应用、断开就消失）会让时间线变成一个不稳定的聚合器。

### 1.2 卡片是类型化的，不是一套固定槽位

每种卡片有自己的 `type` 和 `payload`，服务端按类型校验、渲染器按类型出卡。内置四种：

| type | payload | 长什么样 | 典型 |
|---|---|---|---|
| `photo` | `{ media: 1–4, body?, meta? }` | 图片（1 张大图 / 2–4 张拼贴）+ 一句话 | 「青海湖 · 四天」 |
| `note` | `{ body, meta? }` | 纯文字，落在浅色底上 | 「搬家」 |
| `metric` | `{ metrics: 1–3, body? }` | 一行数字 + 一句话 | 「晨跑 · 12.4 km」 |
| `html` | `{ html, height }` | 应用自己的版式，沙箱渲染 | 票根、海报、小报告 |

所有类型共享的三个字段：`title`、时间、`detail_url`（可选）。类型决定内容和版式，共享字段决定它怎么进时间线。

**`html` 只是一类，不是例外。** 总有表达不了的东西，与其让应用把内容硬塞进三种模板，不如给它一个隔离的画布。

### 1.3 没有编辑

事件写完就是写完：

- 没有用户编辑 —— 没有改标题、换封面、拖拽排序，也就没有 `locked_fields`、没有「用户改过所以不覆盖」这一整类冲突；
- 生产者可以**重推**同一个 `external_id` 覆盖自己的事件（那是更新，不是编辑）；
- 用户可以删除（那不是编辑）。

上一版设计里的「用户编辑优先级最高」这一章可以整段删掉。少一个功能，少掉的是锁字段、合并冲突、撤销、影子版本四件事。

### 1.4 `html` 要隔离，不只是过滤

三方 HTML 直接进我们的 DOM，等于把版式、暗色模式、移动端一起交出去，还得防 XSS。所以：

- 渲染在 `<iframe sandbox>` 里（不给 `allow-scripts`），高度由 payload 指定、服务端钳制；
- 文档里注入一组设计 token（`--eam-bg` / `--eam-text` / `--eam-line` / `--eam-accent`），卡片**可以**用它们跟随明暗主题，也可以完全不用；
- 图片只允许 `https:` 和 `data:`，外链脚本一律不执行；
- 高度上限 640px，超出在卡片内部滚动。

---

## 2. 数据模型

```sql
create table if not exists events (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references users(id) on delete cascade,

  -- 归因
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
  title         text not null,                       -- 所有类型都有，列表/通知/无障碍用
  payload       jsonb not null,                      -- 随 type 变，见 §3
  detail_url    text,                                -- 可选：这条事件的详情页

  -- 状态
  state         text not null default 'published',   -- published | detached | deleted（墓碑）
  merged_into   uuid references events(id) on delete set null,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);

create unique index if not exists events_app_ext
  on events (user_id, token_id, external_id)
  where external_id is not null;

create unique index if not exists events_auto_key
  on events (user_id, producer_key)
  where producer_key is not null;

create index if not exists events_time
  on events (user_id, local_date desc, starts_at desc)
  where state = 'published';

-- 媒体：payload 里写路径，落库时解析成 node；删文件时卡片自己退化
create table if not exists event_media (
  event_id uuid not null references events(id) on delete cascade,
  idx      int not null,
  node_id  uuid not null references nodes(id) on delete cascade,
  primary key (event_id, idx)
);
create index if not exists event_media_node on event_media (node_id);
```

要点：

- **`payload` 是 jsonb**：类型少、字段少，不值得为每种卡建表；校验在应用层，渲染器按 `type` 读。
- **`local_date` 写入时算好**，读的时候不转时区，跨时区旅行不串天。
- **媒体只能是盘里的文件**：三方要带图，先用自己的 token 传进自己的前缀（如 `Apps/strava/2025/0803.jpg`），事件里引用路径。不做外链图片——防盗链、失效、审核、SSRF 都是问题。
- **`detail_url` 只有一个**，就是「这条事件的详情页」。没有就是没有，不猜。
- `merged_into` 留给以后（§10）。

### 2.1 和现有表的关系

- `timeline_moments` → **由 `events` 取代**。迁移时按 `dedupe_key` 灌成 `origin='auto'`、`producer_key=dedupe_key`。
- `timeline_snapshots` → **取消**。没有预算、没有密度，读路径直接按 `local_date` 查 `events`；每次应用写入都要重算快照反而更贵。
- `timeline_assets`、`timeline_series` / `timeline_points` → 保留（文件时间、健康数据）。

---

## 3. 四种内置类型

所有 payload 都是纯数据，不含 HTML（`html` 类型除外）。

### 3.1 `photo`

```json
{
  "media": ["Photos/2025/06/12/a.jpg", "Photos/2025/06/12/b.jpg"],
  "body": "第三天翻到海拔 4,100，风把三脚架吹倒了。",
  "meta": "780 张 → 9 张 · 12.6 km",
  "cover": 0
}
```

- `media` 1–4 张；1 张是 3:2 大图，2–4 张自动拼贴；
- `cover` 是 `media` 的下标，不填就是 0；
- `meta` 是那行小灰字（张数、人数、距离），可选。

### 3.2 `note`

```json
{ "body": "搬到城西。第一晚只有一张床垫和一个箱子，窗外是高架，整夜有车过去。", "meta": "214 个文件" }
```

纯文字一张卡。`body` ≤ 500 字。

### 3.3 `metric`

```json
{
  "metrics": [
    { "label": "距离", "value": "12.4", "unit": "km" },
    { "label": "配速", "value": "5'02\"", "unit": "/km" },
    { "label": "心率", "value": "148", "unit": "bpm" }
  ],
  "body": "配速最后两公里掉到 5'30\"。"
}
```

1–3 个数字，渲染成一行；`value` 是字符串，允许 `5'02"`、`12.4` 这种写法。

### 3.4 `html`

```json
{
  "html": "<div class=\"ticket\">…</div>",
  "height": 172
}
```

- 渲染在 `<iframe sandbox>` 里，见 §1.4；
- `height` 40–640，服务端钳制；宽度永远是容器的 100%；
- 文档里会先注入设计 token 和 `<meta charset>`，其他什么都不注入；
- 不允许 `<script>`、`<iframe>`、`<object>`；`<style>`/`<link>` 只允许内联（外部样式表不加载）。

> 服务端仍然过一遍白名单（标签 + 属性），沙箱是第二道，不是唯一一道。

---

## 4. 详情链接

`detail_url` 是**可选**的，而且**不做一个按钮**：

- 有：标题本身就是链接（hover 下划线），打开新标签；
- 没有：标题不可点，整张卡也不可点；
- 我们的卡片暂时都没有 —— 站内还没有事件详情页；
- 三方自己决定加不加，指向它自己的活动页、文章页、账单页；
- `html` 卡片的链接由它自己的 HTML 决定，我们不管。

不做站内详情页。等有「多图 / 原文 / 来源文件」的真实需求再设计，不要凭想象先画一屏。

---

## 5. 三种来源

| origin | 谁写 | 去重键 | 典型 |
|---|---|---|---|
| `native` | 站内（自动生成或用户新建） | 无 | 「搬家」「青海湖」 |
| `auto` | 文件流水线 | `producer_key` | 照片会话、跨天旅行 |
| `app` | 三方应用（Bearer token） | `external_id` | 「晨跑」「票根」 |

同一张表、同一套读路径。**不新增「应用事件」这种二等公民**：它只是 `origin` 不同，归因那一行不同。

**订阅源（P1）**：不是所有应用都会写 API。给一个导入地址（RSS / Atom / iCal / JSON feed），后台每天拉一次，按条目 id 生成 `origin='app'` 的事件。数据模型不用改。

---

## 6. 写入 API

遵循 [`docs/api/api.md`](../api/api.md)：Bearer、UTC、`{"error","message"}`、路径即身份。
**例外**：事件有 `id`。文件用路径当身份，事件没有天然路径；对应用来说稳定的句柄始终是它自己的 `external_id`。

### 6.1 创建 / 更新（幂等）

```http
POST /events
Authorization: Bearer eam_live_xxx
Idempotency-Key: strava:run-8921

{
  "external_id": "run-8921",
  "starts_at": "2025-08-03T07:12:00Z",
  "ends_at":   "2025-08-03T08:02:00Z",
  "tz": "Asia/Shanghai",
  "precision": "minute",
  "title": "晨跑 · 北岸",
  "detail_url": "https://www.strava.com/activities/8921",
  "type": "metric",
  "payload": {
    "metrics": [
      { "label": "距离", "value": "12.4", "unit": "km" },
      { "label": "配速", "value": "5'02\"", "unit": "/km" }
    ],
    "body": "出门时 24 度。"
  }
}
```

```json
201 { "id": "0f1c…", "url": "https://your-host/e/0f1c…", "state": "published" }
```

- 以 `(user, token, external_id)` 去重：同一个 `external_id` 再来一次是**整体替换**，返回 `200` 和同一个 `id`；
- 带 `Idempotency-Key` 就按「同 key 同结果」处理，重试安全；
- 用户删掉的事件再推回来 → `410 gone`，应用应当停止重试。

### 6.2 其它

```http
DELETE /events/{id}      # 应用侧删除 → state=detached（内容保留）
GET    /api/events?from=&to= # 对账：返回自己写过的 external_id 列表
GET    /events/{id}      # 单条（完整 JSON，供应用渲染分享页）
```

没有 `PATCH`：重推就是更新，少一个接口。

> **路径约定**：和文件 API 一样，对外接口的路径就是功能本身，不带 `/v1` 或 `/api` 前缀（见 [api.md](../api/api.md)）。
> 网页用的读接口仍然是 `GET /api/timeline`（cookie 会话）。

### 6.3 权限

```sql
alter table api_tokens add column if not exists can_events boolean not null default false;
```

- 用户建 token 时勾「写事件」，名字就是归属名（`Strava`、`Readwise`）；
- 应用只能写自己创建的、只能引用 token 前缀内的媒体（复用现有 403 越界逻辑）；
- 没有应用注册表、没有 OAuth。真要开放成平台，再加 `apps` 表和 `app_id`，`events.token_id` 不用动。

### 6.4 限额

| 项 | 值 |
|---|---|
| `title` | ≤ 80 字 |
| `body` | ≤ 500 字 |
| `metrics` | ≤ 3 |
| `media` | ≤ 4 |
| `html` | ≤ 32 KB，height 40–640 |
| 单次请求体 | ≤ 64 KB |
| 频率 | 每 token 60 次/分钟，单日 2,000 条 |

限额进 `GET /capabilities`，客户端不要写死。

---

## 7. 归因与展示

只在日期那一行露出谁写的：

```
8 月 3 日 · Strava ↗        ← 三方
8 月 15 日                   ← native / auto（不显示）
```

- 名称取 token 的 `name`；
- token 被撤销后显示 `Strava（已断开）`，`detail_url` 失效，内容不动；
- 不做应用 logo、不做品牌色；也不做「只看某个应用」的筛选器 —— 真需要了，那是搜索该管的事。

---

## 8. 生命周期

| 动作 | 结果 |
|---|---|
| 生产者创建 | `state='published'` |
| 生产者重推同一个 `external_id` | 整体替换 payload（事件 id 不变） |
| 生产者删除 | `state='detached'`，内容保留，`detail_url` 清空 |
| 用户删除 | `state='deleted'`（墓碑）+ 清空内容 + 删 `event_media`；生产者重推会被忽略 |
| 撤销 token | 事件保留；来源显示「已断开」 |
| 用户删除某应用的全部事件 | 批量墓碑（设置里提供，未实现） |

没有「用户修改」这一行，所以也没有锁定字段、没有合并冲突。

---

## 9. 分期

| 阶段 | 内容 |
|---|---|
| **P0** ✅ | `events` / `event_media` 迁移；四种类型；auto 流水线写 `origin='auto'`；`POST` / `DELETE` / `GET /events`；来源那一行；`detail_url`；网页读接口换成扁平事件流 |
| **P1** | feed 导入（RSS / iCal）；html 的服务端白名单清洗（现在只靠 sandbox）；应用事件的对账 UI |
| **P2** | 合并；更多类型（清单 / 引用 / 地点）；应用注册表 |

`html` 放 P1 是因为它要先把沙箱、token 注入、高度钳制做对；`photo` 那三种先跑通，时间线就能用。

---

## 10. 合并（P2，只留结构）

同一天的照片和一次跑步，现在是两条事件。以后可以合并成一条，卡片上挂两个来源：`merged_into` 指向主事件，子事件不显示。

**P2 之前不做任何自动合并。** 宁可两条，也不要算法把「同一天」硬说成「同一件事」。

---

## 11. 未定

1. **站内要不要事件详情页**：现在所有卡片都没有 `detail_url`，点不了。等「多图 / 原文 / 来源文件」真有需求再做。
2. **事件的删除粒度**：现在只能一条条删（且网页上还没入口）。要不要「删掉这个应用的全部事件」？批量墓碑的成本很低，但入口放哪儿没定。
3. **`html` 的审核**：P1 放开后，谁来判断一张 HTML 卡是不是钓鱼？先靠来源标记 + 用户可删，还是先只对白名单应用开放？
4. **自动事件和应用的边界**：同一天的照片（auto）和跑步（app）要不要在时间线上互相提及？现在互相不知道对方存在。
5. **一条事件几张图**：暂定 4。旅行那种 9 张是放宽 `photo`，还是等详情页？

---

## 12. 实现状态

| 东西 | 位置 |
|---|---|
| 表结构 | [`sql/013_events.sql`](../../sql/013_events.sql) |
| 领域逻辑（校验 / 读 / 写 / 墓碑） | [`backend/internal/events/events.go`](../../backend/internal/events/events.go) |
| 网页读接口 + 对外 API | [`backend/internal/api/events.go`](../../backend/internal/api/events.go) |
| 自动事件的生产者 | [`backend/internal/timeline/service.go`](../../backend/internal/timeline/service.go)（`autoEvents` / `PublishAuto`） |
| 前端 | [`web/src/pages/StreamPage.tsx`](../../web/src/pages/StreamPage.tsx) |
| 令牌能力位 | `api_tokens.can_events`；CLI `eam token create --events` |

已落地：

- `events` / `event_media` 两张表；令牌的第三个能力位 `can_events`；
- 四种卡片类型的写入校验（限额集中在 `events.ValidateApp`）；
- 幂等 upsert（`external_id`）、墓碑（用户删除）、detached（应用删除）；
- 媒体解析：路径 → `node`，必须在 token 前缀内；读的时候预签名缩略图；
- 网页：`GET /api/timeline` 返回扁平事件流（keyset 游标），卡片按类型渲染，html 走 sandbox iframe；
- 流水线：每个月的选中片段写成 `origin='auto'` 的事件。

还没做（按设计暂缓，不是遗漏）：

- **原生编辑器**：站内还不能手写一条事件（native 目前没有生产者）；
- **feed 导入**（RSS / iCal）；
- **健康变化卡**：`timeline_series` 还在，但还没有"发现异常 → 生成一张卡"的生产者；
- **合并**（`merged_into` 只有字段）；
- **删除的 UI**：`DELETE /api/events/{id}` 和 `DeleteOwned` 都在，网页上没有入口；
- 服务端对 html 的白名单清洗（现在只有 sandbox + CSP）。
