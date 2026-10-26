# WebDAV / S3 到底够不够用：场景走查

> 状态：分析稿 ｜ 最终接口见 [api.md](api.md)
>
> **最终决定：用自己的 API。不做 WebDAV，也不做 S3 门面。**
> 语义照抄 S3 / Drive / Dropbox（同一套路径树操作），形式自己定，URL 不带版本号。
> WebDAV 被否是因为字节必须过我们的服务器；S3 被否是因为要给三方 App 做控制就得做门面，
> 而门面把复杂度全压在我们身上、客户端反而要多记五个配置。
>
> 本文保留作推理记录：走查、缺口表、Drive/Dropbox 对标都还有效。
> §5 的推论（nodeId、kind、变更流）和 §6–§9 的方案**没有采纳**。

## 0. 结论先说

1. **WebDAV 够用，而且对"AI-native + 松散数据"是最合适的一扇门。**
   它原生覆盖了读写、列目录、移动、锁、配额；缺的那部分不是"读写"，是
   **发现、身份、生命周期**——恰好都是可以在上面加一层薄东西解决的。

2. **S3 够用，但方向是反的。** 它假设"你已经知道要拿哪个 key"。
   批量搬运、备份、成熟 SDK 很强；但"用户按心意重组目录、App 自适应"这个模型，
   S3 表达不了——没有目录、没有 rename、元数据不能查、变更只能靠事件通知。

3. **两套都不够的地方，只有五件**，而且都不该由协议层解决：

   | 缺口 | 为什么标准解决不了 | 我们加什么 |
   |---|---|---|
   | 搜索 | DASL(RFC 5323) 事实上没人实现 | `/v1/search` |
   | 变更流 | RFC 6578 有标准，服务端/客户端支持都极少 | 实现 REPORT + `/v1/changes` |
   | 稳定身份 | 标准和 S3 都以路径/key 为身份 | `nodeId` |
   | 生命周期 | 回收站、版本、分享不存在于 S3，DeltaV 已死 | `/v1/trash`、`/v1/versions`、`/v1/shares` |
   | 面向人的授权 | WebDAV 只有 Basic+ACL，S3 只有 SigV4 | token + scope + prefix |

4. **所以不是"三套协议并列"，而是：WebDAV 是门，JSON 是补丁，S3 只做搬运。**

---

## 1. 怎么判断"够用"和"方便"

"够用" = 这个流程能不能表达出来。
"方便" = 客户端要写多少代码、一次流程要发多少请求、边界情况要处理多少。

两者经常背离：S3 表达力强但签名麻烦；WebDAV 表达力弱但生态现成。所以下面每条都分开评。

---

## 2. 总表

| # | 场景 | 需要的操作 | WebDAV | S3 | 判断 |
|---|---|---|---|---|---|
| 1 | 接入拿凭证 | 认证 + 探能力 | `OPTIONS` + Basic | SigV4 + 密钥 | 都不面向人 |
| 2 | 首次发现 | 全量列树 | `PROPFIND` 逐层（N 次请求） | `ListObjectsV2` 扁平（N/1000 次） | WebDAV 差，S3 中 |
| 3 | 增量发现 | 只要新的 | `REPORT sync-collection` | 无（要配事件通知） | 都不好用 |
| 4 | 语义查找 | "羽毛球视频" | 无 | 无 | **不够** |
| 5 | 读整个文件 | 下载 | `GET` | `GetObject` | 够 |
| 6 | 读片段 | Range | `GET` + `Range` | 同 | 够 |
| 7 | 读元信息 | size/mtime/etag | `HEAD` / `PROPFIND` | `HeadObject` | 够 |
| 8 | 写小文件 | 上传 | `PUT` | `PutObject` | 够 |
| 9 | 写大文件 | 断点续传 | 无标准 | `MultipartUpload` | WebDAV 不够 |
| 10 | 覆盖更新 | 条件写 | `If-Match` | 条件 Put | 够 |
| 11 | 并发保护 | 锁 | `LOCK/UNLOCK` | 无 | WebDAV 够 |
| 12 | 用户重组目录 | 移动/改名 | `MOVE` | 无（copy+delete） | WebDAV 够，S3 不够 |
| 13 | 删除与恢复 | 回收站 | `DELETE`（硬删） | `DeleteObject` | 都不够 |
| 14 | 版本/找回旧稿 | 版本 | DeltaV（已死） | `Versioning` | S3 够，WebDAV 不够 |
| 15 | 分享给别人 | 临时链接 | 无 | 预签名 URL | 都不面向人 |
| 16 | 多 App 共读 | 跨目录读 | 挂载后随便读 | 前缀列举 | 都够，但授权难 |
| 17 | 备份/退出 | 整盘搬走 | 挂载后 `rsync` | `rclone sync` | 都够 |
| 18 | 浏览器里的 App | CORS + 上传 | 基本不可用 | 要前端签名 | **不够** |
| 19 | AI agent 浏览 | 遍历 + 读 + 写 | 挂载成目录即可 | 需要自己封一层 | WebDAV 意外地好 |
| 20 | 审计"谁写的" | 归属 | 无 | 无 | **不够** |

---

## 3. 逐条走查

### 3.1 接入

App 要先知道"我能干什么"。WebDAV 的答案藏在 `OPTIONS` 里：

```http
OPTIONS /dav/
→ DAV: 1, 2
  Allow: OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK, REPORT
```

S3 没有"能力协商"，只能靠文档和 `ListBuckets`/`GetBucketLocation` 试。
我们的 `/v1/me` 直接返回 `capabilities` 数组，比两套都清楚。

**方便程度**：WebDAV 的 Basic 认证对脚本不友好（要先处理 401 challenge），
对成品客户端很友好（库都封装好了）。这就是为什么脚本门要单独存在。

### 3.2 首次发现：把一个盘扫一遍

假设用户有 5 万文件、3000 个目录。

- **WebDAV**：`PROPFIND Depth: 1` 只能拿一层，递归要 3000 次请求。
  `Depth: infinity` 理论上能一次拿完，但 RFC 4918 明确允许服务端拒绝
  （`403 propfind-finite-depth`），实际上也没人敢开——一次返回几十 MB XML。
- **S3**：`ListObjectsV2?prefix=&max-keys=1000`，扁平列举 50 页就完了，
  不需要按目录递归。**这一项 S3 明显更强。**
- **我们**：`/v1/list?recursive=1&prefix=Videos/` 直接查 Postgres，
  一页 1000 条、keyset 分页，和 S3 一样是扁平的，但不花钱、不依赖桶。

**结论**：这里其实混了三种比较，先拆清楚：

1. **WebDAV vs S3**（同样是"协议层列举"）：S3 强。它是扁平列举，50 页拿完；
   WebDAV 只能一层层 `PROPFIND`，3000 个目录就是 3000 次请求。
2. **协议列举 vs 查索引**（我们是"有数据库的"）：查索引强得多。一次 SQL、一页 1000 条，
   还能顺便按名字过滤。所以"都不理想"说的是：**跟索引比，这两种都很慢。**
3. **最终要不要专门优化**：不要。我们不是"扫不动"，是"扫得慢一点"——
   对一个真文件夹来说，这本来就是这个速度。所以不做 `recursive=1`、不做游标、
   不做变更流，逐层 `PROPFIND` 就是答案。需要快的人把目录分小一点，或者直接搜。

> 这一段的教训：把"能做到"和"值得专门做"混为一谈，就会长出一个没人用的 API。
> 最终版（[api.md](api.md)）没有为扫描单独做接口：`GET /list` 就是列目录，翻页就是了。

### 3.3 增量发现：只处理新东西

这是"数据分析类 App"（羽毛球分析、账本导入、相册整理）每天都要做的事。

- **WebDAV**：标准里真有——RFC 6578 `REPORT sync-collection`：

  ```http
  REPORT /dav/Videos/
  <D:sync-collection xmlns:D="DAV:">
    <D:sync-token>opaquelocktoken:...</D:sync-token>
    <D:sync-level>1</D:sync-level>
    <D:prop><D:getetag/><D:getcontentlength/></D:prop>
  </D:sync-collection>
  ```

  服务端回 `207 Multi-Status` + 新 token。**语义完全正确，但现实里服务端和客户端都极少实现。**
  我们自己实现是加分项（rclone 等少数客户端会用），但不能指望 App 都会。
- **S3**：没有 API。要么轮询 `ListObjectsV2` 自己 diff，要么配 Bucket Notification
  推到 SQS/SCF——这对个人 App 是不可承受的。

**结论**：增量发现必须由我们提供，且要同时给两条：
标准形状（`REPORT sync-collection`）+ 简单形状（`GET /v1/changes?since=`）。
这是 WebDAV/S3 都缺、而"数据汇总"场景最刚需的能力。

### 3.4 语义查找："找出所有羽毛球视频"

WebDAV 的搜索标准是 DASL（RFC 5323），基本没人实现；S3 完全没有（只有前缀）。

用户的数据是**按心意组织的**——他可能放在 `/运动/羽毛球/2026/`，也可能直接扔在
`/手机相册/VID_20261024.mov`。文件名可能是 `VID_20261024.mov`。

所以"找"这件事，不能靠路径，只能靠：

1. **文件名/路径模糊搜索**（已有 pg_trgm）；
2. **标签**（用户打的、App 打的）；
3. **内容**（后续接缩略图/封面/正文抽取/embedding）。

`/v1/search?q=羽毛球&mime=video/&tag=sport` 是这一层唯一的入口。
**结论：搜索是 AI-native 场景的第一入口，不是锦上添花。**
AI 不会遍历 TB 级目录，它会先 search 再 read。

### 3.5 读

```http
GET /dav/Videos/羽毛球/a.mov
Range: bytes=0-10485759
→ 206 Partial Content
```

WebDAV 和 S3 都原生支持 Range。**够，而且这是两套协议最扎实的部分。**

唯一要注意的是：WebDAV 门必须**代理字节**（客户端连的是我们的域名，带 Basic 认证），
不能 302 到 COS——否则 Range 头和预签名签名的交互很容易出问题。
所以 storage 层要加一个 `GetRange`，不能只有 `GetURL`。

### 3.6 写小文件 / 写大文件

小文件（< 32 MiB）：`PUT` / `PutObject`，都够。

大文件是分水岭：

- WebDAV **没有标准的断点续传**。`PUT` 断了就从头来。2 GB 的剪辑输出在
  家用网络上失败率不低，Finder 会重试整个文件。
- S3 的 `MultipartUpload` 就是为这个设计的，成熟稳定。

**结论**：WebDAV 门适合"挂载后日常读写"，不适合"传 20 GB 视频"。
大文件走 `/v1/uploads`（预签名 multipart），这也是 S3 唯一明显优于 WebDAV 的地方。
用户传 TB 级数据时，本来就应该用 `coscli`/`rclone` 直接进桶 + `reconcile`。

### 3.7 用户重组目录：这条决定了整个架构

用户今天把视频放在 `/Videos/羽毛球/`，下周整理成 `/运动/羽毛球/2026/`。

**WebDAV**：

```http
MOVE /dav/Videos/羽毛球/a.mov
Destination: /dav/运动/羽毛球/2026/a.mov
```

一次调用，客户端无感。服务端要做的是：改索引 + 把 COS 对象从旧 key 复制到新 key
（因为我们的 key 就是路径）。单文件 O(1)，一个 1000 文件的目录就是 1000 次 copy——
这是 WebDAV 门要面对的真实成本，得定策略（见 §7）。

**S3**：没有 MOVE。客户端只能 `CopyObject` + `DeleteObject`。
对一个"监听变化"的 App 来说，它在列表里看到的是**一个 key 消失、另一个 key 出现**，
它无法判断这是"移动"还是"删了又传了一个新的"。

后果很严重：App 会重新分析一遍、重新生成一份结果、把用户盘塞满重复数据。
**这就是为什么 S3 不能作为"App 日常读写"的门。**

**我们比两套都强的地方**：给每个文件一个稳定的 `nodeId`（`nodes.id` 已经有 UUID），
`MOVE` 之后 id 不变，变更流里发一条：

```json
{"seq": 1288, "op": "move", "id": "nod_7f3a…", "from": "Videos/羽毛球/a.mov", "to": "运动/羽毛球/2026/a.mov"}
```

App 的索引指向 id，路径变了就更新路径，永远不用重算。
**"路径是用户的，身份是稳定的"** —— 这是松散数据能被多个 App 复用的前提。

### 3.8 删除、恢复、版本

- WebDAV 的 `DELETE` 按规范是**永久删除**；S3 的 `DeleteObject` 也是（除非开 versioning）。
- 我们把它实现成软删除，客户端无感（照样 204），但数据进回收站。
- 恢复、列回收站、按时间清理，都需要额外接口：`/v1/trash`。
- 版本：S3 的 bucket versioning 直接可用；WebDAV 的 DeltaV 基本是纸面标准。
  我们的实现建议是覆盖前把旧对象 `copy` 到 `_versions/<id>/<seq>`，API 用 `/v1/versions` 暴露。

**结论**：这两件事都不该指望协议，必须自己补。而它们恰恰是"长期价值"的核心承诺。

### 3.9 分享、审计、配额

- **分享**：S3 的预签名 URL 是半成品（能给别人看，但要自己发链接、自己管过期）；
  WebDAV 完全没有面向人的分享。`/v1/shares` 补。
- **审计**："这个文件是哪个 App 写的"——两套都没有。写 `audit_log`。
  这也是让用户敢把 token 交给第三方的唯一理由。
- **配额**：WebDAV 有 RFC 4331（`quota-available-bytes` / `quota-used-bytes`），
  Finder 会显示"还能放多少"；S3 没有。**这一项 WebDAV 赢，而且值得实现。**

### 3.10 备份与退出

```bash
rclone sync eam:/ ~/backup            # WebDAV
rclone sync cos:bucket/drive/<uid>/ ~/backup   # S3 直连
```

两条都通。前提是：**key 就是路径**（不是哈希 blob）、`getlastmodified` 返回真实时间。
这两条我们在架构上已经满足了，这是"随时跑路"的底气。

### 3.11 浏览器里的 App

WebDAV 在浏览器里基本不可用（自定义方法要过 CORS 预检、Basic 认证没法优雅处理）。
S3 要前端做 SigV4 签名，等于把密钥发给浏览器，不可接受（预签名可以，但要后端配合）。

**只有一条路**：`/v1` + Bearer + 预签名直传。这是 JSON 门存在的第一个硬理由。

---

## 4. 能力缺口矩阵

| 能力 | 标准 | 现实支持 | 我们的补法 |
|---|---|---|---|
| 列目录 | RFC 4918 PROPFIND | 好 | 直接用 |
| 扁平递归列举 | 无 | — | `/v1/list?recursive=1` |
| 读写 / Range | RFC 4918 / HTTP | 好 | 直接用 |
| 移动 / 复制 | RFC 4918 MOVE/COPY | 好 | 直接用 |
| 锁 | RFC 4918 LOCK | 客户端好、服务端要自己实现 | Postgres 锁表 + TTL |
| 配额 | RFC 4331 | 客户端好 | 直接用 |
| 变更流 | RFC 6578 sync-collection | **两端都极少** | 实现 REPORT + `/v1/changes` |
| 搜索 | RFC 5323 DASL | **基本死了** | `/v1/search` |
| 版本 | RFC 3253 DeltaV | **基本死了** | `/v1/versions` |
| 回收站 | 无 | — | `DELETE` 软删 + `/v1/trash` |
| 大文件断点续传 | 无 | — | `/v1/uploads` multipart |
| 分享 | 无 | — | `/v1/shares` |
| 结构化元数据 | PROPPATCH dead props | **客户端支持差** | `PATCH /v1/fs` + 把 tags 投影成 dead props |
| 面向人的授权 | RFC 3744 ACL | 复杂、少用 | token + scope + prefix |
| 审计 | 无 | — | `audit_log` + UI |
| 稳定身份 | 无 | — | `nodeId`（已有 UUID） |

---

## 5. AI-native 的推论

你说的那套逻辑我完全同意，而且它会**反向改写接口设计**。具体五条：

### 5.1 不做 schema，做可发现性

我们不规定 App 的数据格式——但"松散"不等于"不可读"。协议层要保证三件事：

1. 路径是**人类可读**的（不要 `a1b2c3.json`）；
2. 文件**自解释**（`manifest.json`、JSON 顶层的 `_type`/`_schema`、Markdown front-matter）；
3. 元数据**可查**（tags、mime、时间）。

只要这三条成立，一个有 AI 能力的 App 就能自己理解一份陌生数据。
反过来，如果路径是哈希、内容是二进制裸块，再聪明的 App 也读不懂。

> 这条直接支持"不做格式适配"：我们不替 App 理解数据，但要让数据**可被理解**。

### 5.2 身份稳定，路径可变

用户会重组目录，App 却要维护自己的索引。唯一可靠的锚点是 `nodeId`。
没有它，"用户整理一次目录"就等于"所有 App 的缓存全部作废"。

### 5.3 搜索优先于遍历

AI App 的工作方式是：`search` → `read` → 推理 → `write`。
它不会 `find . -name` 扫 5 万个文件。所以 `/v1/search` 要和 `GET` 一样重要，
标签和 MIME 过滤要比路径前缀更常用。

### 5.4 区分"原始"与"派生"

这是你和大多数网盘都没想清楚的一层。App 会往盘里写很多**中间产物**：
缩略图、切好的片段、索引文件、分析 JSON。

如果这些和用户的原始视频同等对待，几年后这个盘就没法看了。
建议在写入时标一个 `kind`：

| kind | 含义 | 用户清理时 |
|---|---|---|
| `primary` | 用户的原始数据，唯一副本 | **永不自动删** |
| `derived` | 从原始数据算出来的，可重建 | 可一键清理 |
| `cache` | 完全可丢弃的缓存 | 可随时清 |

```http
PUT /v1/fs/Apps/badminton/analyses/nod_7f3a.json
X-EAM-Kind: derived
```

这一条几乎零成本，但它让"多 App 共享一个盘"在多年后仍然干净。

### 5.5 中间结果留在 App 自己那边

同意，而且协议应该把它写成**写入准则**而不是技术强制：

> 只有"用户会想再看一眼、或者十年后还想要"的东西才写进 EAM。
> 索引、缓存、临时切片、模型权重——放 App 自己的存储里。

技术上配合三件事：配额（写满会说）、审计（能看到哪个 App 写得多）、
`kind`（派生数据可整批清理）。这就够了，不需要复杂策略引擎。

### 5.6 顺带一个机会：给 agent 一个工具面

如果 App 本身是 AI agent，最自然的接口不是 REST 文档，而是四个工具：
`ls` / `read` / `search` / `write`。一个几十行的 MCP server 就能让任何
MCP-capable agent 直接把这个盘当工作目录用。这比让每个 App 自己实现客户端便宜得多。

---

## 6. 那到底要不要做 S3？

我的建议：**v1 不做，留一个明确的"以后可能做"的位置。**

理由：

- S3 最强的三个场景（批量搬运、备份、成熟 SDK）里，前两个 `rclone` + WebDAV 已经覆盖，
  第三个（"我要用 boto3"）在 AI-native 的场景里很少见；
- 做 S3 兼容要么依赖 COS 的 S3 端点（索引滞后、要 `reconcile`），
  要么自己实现 SigV4 + 一套子集（工作量 1–2 周），收益和成本不成比例；
- 真正需要 S3 的时刻是"用户想用现成的备份工具/对象存储生态"，
  到那时再把 COS 的 S3 端点 + STS 前缀凭证接上，一天就能做完。

**定位**：S3 是"运维/搬运通道"，不是"App 接入通道"。

---

## 7. WebDAV 的坑（"方便"的真实答案）

这些是真正决定"能不能用"的细节，不是理论问题：

| 坑 | 现象 | 处理 |
|---|---|---|
| `PROPFIND Depth: infinity` | 有人一次拉整棵树，服务端爆 | 返回 `403 propfind-finite-depth` |
| 目录 `MOVE` 是 O(n) 拷贝 | 1000 个文件要 1000 次服务端 copy | 同步做 + 上限告警；超过阈值返回 202 + 后台作业（先做同步版） |
| macOS 发 NFD 文件名 | `é` 存进去再查查不到 | 入口统一 NFC（已有 `paths.Normalize`） |
| Finder 写 `.DS_Store` / `._*` | 盘里全是垃圾文件 | 接收但标记 hidden，列表默认过滤 |
| 大小写 | macOS 客户端可能建出 `A.jpg` 和 `a.jpg` | 默认区分；可开 `STRICT_CASE=off` 时拒绝大小写冲突 |
| ETag 语义 | COS 分片上传的 ETag 不是 MD5 | 当不透明字符串用，绝不承诺 MD5 |
| Windows 资源管理器 | 默认不允许 http + Basic | 文档写明必须 HTTPS；`WebClient` 服务要启动 |
| Office 打开文档 | 没有 LOCK 会变成只读 | 实现 LOCK/UNLOCK（Postgres 表 + TTL） |
| 大 PUT 超时 | 反代/客户端 30s 超时 | 反代超时调大；大文件走 `/v1/uploads` |
| 尾斜杠 | `GET /dav/a` 是文件还是目录 | 目录必须带 `/`，集合的 href 一律以 `/` 结尾 |
| 认证失败格式 | 客户端不弹密码框 | 返回 `401` + `WWW-Authenticate: Basic realm="eam"` |

**验证方式**：用 `litmus`（webdav.org 的官方一致性测试）跑 `basic` / `copymove` / `props` / `locks`
四组，再用 `cadaver` 手工过一遍，最后 Finder + rclone 各挂一次。
这三步过不了，就不要宣称"支持 WebDAV"。

---

## 8. 对 v1 协议的修订

基于上面的走查，原设计要改七处：

1. **WebDAV 升为第一门**，`/v1` 重新定位成"WebDAV 补不了的那部分 + 脚本便利通道"。
2. **`nodeId` 成为一等公民**：`/v1/list`、`/v1/search`、`/v1/changes` 都返回 `id`；
   `MOVE`/重命名后 id 不变；App 的索引应当以 id 为主键。
3. **变更流增加 `move` 事件**（`from`/`to`/同一个 `id`），并实现 RFC 6578 `sync-collection`。
4. **增加 `kind`（primary / derived / cache）**，写入时可标，用于清理与配额统计。
5. **`/v1/list` 支持 `recursive=1`**，扁平 + keyset 分页，补 WebDAV 的短板。
6. **token 的读写 scope 分离**：读可以是 `read:*`（用户显式授权全盘），写永远限制在 `write:<prefix>`。
7. **大文件写只走 `/v1/uploads`**，WebDAV 的 `PUT` 设一个上限（如 2 GiB），超了返回 413 并提示。

---

## 9. 修订后的实施顺序（Go 版）

| 阶段 | 内容 | 估时 | 产出 |
|---|---|---|---|
| P1 | token + `/v1` 核心（fs/list/search/me）+ 错误体 | 2–3 天 | 脚本能用 |
| P2 | WebDAV（`x/net/webdav` + 自定义 FileSystem）+ LOCK 表 + quota props | 3–4 天 | Finder 能挂载 |
| P3 | `nodeId` 透出、变更流、RFC 6578、版本、回收站 | 2–3 天 | 多 App 增量同步 |
| P4 | `/v1/uploads`（multipart）、`/v1/shares`、审计 UI | 2–3 天 | 大文件与分享 |
| P5 | MCP server + CLI + `manifest.json` 约定 | 按需 | agent 直接可用 |

Go 的好处是 P2 从"要不要另起一个服务"变成"同一个二进制里加一个 handler"，
`/dav`、`/v1`、`/api` 共享同一套 storage / nodes / auth，没有跨进程一致性要处理。

> 注意：上表的 P1–P5 是"重方案"的顺序，**已被 [api.md](api.md) 的精简版取代**。

---

## 10. 对标：Google Drive 和 Dropbox 的 API 够不够

### 它们长什么样

**Google Drive v3** —— 一个带查询语言的"文件数据库"：

- 每个对象是 `files/*`，字段有 `id`、`name`、`mimeType`、`parents[]`、`size`、
  `md5Checksum`、`modifiedTime`、`trashed`。文件夹本身也是一个 file（特殊 mimeType）。
- **它不是路径树**：文件靠 `id` 引用，靠 `parents` 找位置。所以必须有 fileId——
  id 和位置是两件事。
- 列目录 / 找文件都走 `files.list`，用一个 `q` 查询语言过滤：
  `q=name contains '羽毛球' and mimeType contains 'video/' and trashed=false`。
- 增量靠 `changes.getStartPageToken` + `changes.list`（pageToken）。
- 大文件 `resumable upload`；版本 `revisions`；权限 `permissions`；分享 `permissions.create`。
- 认证是 OAuth 2.0 + scope（最细的一档是 `drive.file`：只碰 App 自己创建的文件）。

**Dropbox v2** —— 一个路径树 + 游标：

- 路径是 `/folder/file.txt`（大小写不敏感），也有 `id`，但日常用路径。
- 列目录 `files/list_folder` 返回一个 `cursor`；`files/list_folder/continue` 拿下一页——
  **增量就是"拿旧 cursor 继续列"**，没有单独的事件流概念。
- 大文件 `upload_session`（start/append/finish）。
- 有 `content_hash`（不是 MD5）、`revisions`、shared link。
- 认证也是 OAuth 2.0 + scope。

### 对我们的目标，够不够

**对它们自己的生态够用；对"让现成 App 把数据存到用户自己的盘里"这个目标，不够。** 三条：

1. **私有协议，没有复用价值。**
   能写 Drive 的 App 不会因此能写我们。而我们要的是"现成 App 填个地址就能用"——
   Finder、rclone、Obsidian、Zotero、KeePassXC 都已经写好了 **WebDAV** 客户端，
   不会为我们写 Drive 客户端。这是选 WebDAV 最实在的理由。
2. **概念更多，正好踩在你要避免的"驱动成本"上。**
   Drive 至少要懂 fileId、parents、`q` 语法、pageToken、resumable、scope；
   Dropbox 至少要懂 path、cursor、content_hash、upload_session、scope。
   WebDAV 只要求懂"文件"和"HTTP 动词"——这两样模型的训练数据里有几十年。
3. **不能挂载。** 两家都得靠官方客户端或第三方适配器才能变成一个"盘"。
   而挂载对 AI App 是决定性的：挂上之后它就能用 `ls`/`cat`/`grep`，
   不需要为我们的接口写任何工具层。

| | Drive | Dropbox | WebDAV |
|---|---|---|---|
| 定位 | 文件数据库 + 查询语言 | 路径树 + 游标 | 路径树 + HTTP 动词 |
| 必懂概念 | 6–8 个 | 4–5 个 | 2 个 |
| 能否挂载 | 需适配器 | 需官方客户端 | **原生** |
| 客户端复用 | 无 | 无 | Finder / rclone / 一堆 App |

### 值得抄的两点（都不增加概念）

- **Dropbox 的 `cursor`**：如果哪天真要做增量，"拿旧游标继续列"就是增量，
  不用引入"事件流"。这是唯一一种零新增概念的增量形态。
- **Drive 的 `drive.file` scope**：App 只看得见自己创建的文件。
  对应到我们就是"token 限定在 `/Apps/<app>/`"，用户不需要理解任何新东西。

一句话：**Drive/Dropbox 是"拿一套 SDK 接它们的云"，WebDAV 是"给你一个文件夹"。**
我们要的是后者。
