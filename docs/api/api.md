# Everything About Me API

> 我们自己的接口。**语义和 S3 / Drive / Dropbox 是同一套**——同样是"对一棵路径树做增删改查"，
> 只是形式由我们定义。URL 上不带版本号，也不带 `/v1`。

---

## 0. 一句话

一个 Bearer token，对一棵路径树做增删改查，外加搜索和上传。**文件字节不经过服务器**：读写都是拿一个地址直接跟 COS 换。

```
POST /upload          → 拿上传地址（小文件只有一个分片）
PUT  <那个地址>        → 字节直连 COS
GET  /files/<path>    → 302 到下载地址

其他全部走我们：/list /search /mkdir /move /copy /delete /me /capabilities
```

没有一个概念需要先读文档才能猜对。`mkdir` 就是建目录，`copy` 就是复制，
`Prefer: respond-async` 就是"我允许你后台做"。

---

## 1. 为什么不是直接用 S3

语义上我们和 S3 做的是同一件事。但 S3 缺三样我们要的东西：

1. **没有"用户"**。它不知道这个 App 属于谁、能碰哪个目录——那是我们的用户系统。
2. **没有搜索**，也没有"谁写了什么"。
3. 要对三方 App 做细粒度控制，就得做一个 S3 门面（SigV4、XML、分片、307），
   服务端 ~2500 行，客户端还多出 region/bucket/endpoint 五个配置。

所以：**语义抄它，接口自己定。** 客户端只需要 `endpoint` 和 `token` 两个值。

---

## 2. 和常见 API 的语义对照

同一件事，大家叫法不同，做的事一样：

| 事情 | S3 | Google Drive | Dropbox | 我们 |
|---|---|---|---|---|
| 列目录 | `ListObjectsV2` | `files.list` | `files/list_folder` | `GET /list?path=` |
| 读文件 | `GetObject` | `files.get` | `files/download` | `GET /files/<path>` |
| 写文件 | `PutObject` | `files.create` | `files/upload` | `PUT /files/<path>` |
| 大文件 | `MultipartUpload` | resumable upload | `upload_session` | `POST /upload` |
| 元信息 | `HeadObject` | `files.get` | `files/get_metadata` | `HEAD /files/<path>` |
| 建目录 | —（没有目录） | `files.create` | `files/create_folder` | `POST /mkdir` |
| 删除 | `DeleteObject` | `files.delete` | `files/delete_v2` | `DELETE /files/<path>` |
| 移动/改名 | `CopyObject`+`Delete` | `files.update` | `files/move_v2` | `POST /move` |
| 复制 | `CopyObject` | `files.copy` | `files/copy_v2` | `POST /copy` |
| 搜索 | — | `files.list(q=)` | `files/search_v2` | `GET /search?q=` |

请求参数和返回值也尽量贴近：路径、`cursor` 分页、`etag`、`size`、`mime`、`modifiedAt`。
**没有一个是新概念。**

---

## 3. 约定

| 项 | 约定 |
|---|---|
| Base URL | `https://your-host` |
| 认证 | `Authorization: Bearer eam_live_xxx`（网页 cookie 会话同样可用） |
| 路径 | 逻辑路径，不带前导 `/`，按段 percent-encode（`/` 不编码） |
| 时间 | ISO-8601 UTC，`2026-10-24T02:11:07Z` |
| 分页 | `cursor` 是不透明字符串，`limit` 默认 1000、上限 5000 |
| 错误 | `{"error":"not_found","message":"..."}` + 正确的 HTTP 状态码；`message` 是英文短句，可直接展示或记日志 |
| 字符 | UTF-8；服务端统一 NFC |
| 大小 | 单次 `PUT` ≤ 32 MiB；更大的走 `/upload` |
| 异步 | 任何改状态的请求都可以带 `Prefer: respond-async[, wait=N]`，见 §4.12 |
| 能力 | `GET /capabilities` 免鉴权，客户端的上限从它读，**不要写死** |
| 父路径 | 父路径是文件时，写入/建目录返回 `409`（不会在文件底下长出一棵子树） |

**没有 `id`。** 路径就是身份（和 S3 的 key 一样）。服务端内部有行主键，
但不进 JSON——理由见 §8.4。

---

## 4. 接口

```
GET    /capabilities                      上限与能力（免鉴权）
GET    /me                                我是谁
GET    /list?path=                        列目录
GET    /files/<path>                      读文件
HEAD   /files/<path>                      看元信息
PUT    /files/<path>                      写小文件
DELETE /files/<path>                      永久删除
POST   /mkdir     {path}                  建目录
POST   /move      {from,to,overwrite}     移动 / 改名
POST   /copy      {from,to,overwrite}     复制
POST   /upload → GET /upload/{id} → POST /upload/complete
GET    /search?q=                         搜索
GET    /move/{id}  POST /move/{id}/cancel  POST /move/{id}/retry
GET    /copy/{id}  POST /copy/{id}/cancel  POST /copy/{id}/retry
```

### 4.1 列目录

```http
GET /list?path=Photos/2024&limit=200&cursor=
```

```json
{
  "path": "Photos/2024",
  "writable": true,
  "items": [
    {
      "name": "a.jpg",
      "path": "Photos/2024/a.jpg",
      "parent": "Photos/2024",
      "type": "file",
      "size": 248123,
      "mime": "image/jpeg",
      "etag": "9f2b…",
      "sha256": "…",
      "createdAt": "2026-10-24T02:11:07Z",
      "modifiedAt": "2026-10-24T02:11:07Z"
    },
    {"name": "raw", "path": "Photos/2024/raw", "parent": "Photos/2024", "type": "dir", "size": 0}
  ],
  "cursor": null
}
```

- 目录在前，再按 `lower(name)` 排。
- `cursor` 为 `null` 表示没有下一页。
- 空目录也会返回 `type: "dir"`（服务端建目录时写了占位对象 + 索引记录）。
- `writable` 是**这个目录**能不能写（只读令牌、或者目录在令牌前缀之外，都是 `false`）。
  Agent 用它决定要不要继续尝试写入，别靠撞 403 试错。
- `sha256` 是内容指纹，不是身份：内容没变就是一个值。拿它做缓存键比拿路径稳。

### 4.2 读文件

```http
GET /files/Photos/2024/a.jpg
Authorization: Bearer eam_live_xxx

→ 302 Location: https://<数据域名>/drive/<uid>/Photos/2024/a.jpg?q-signature=…
```

> **当前实现**指向 COS 域名（`<bucket>.cos.<region>.myqcloud.com`），它和 API 域名不同，
> 所以下面那条"跨域剥 Authorization"的要求已经满足。接入 CDN 后把数据域名换成
> `data.example.com` 即可，签名要用那个 host 重签。

这个 302 客户端不需要处理——所有 HTTP 客户端自动跟随：

```python
r = requests.get(f"{API}/files/Photos/2024/a.jpg", headers=auth)
open("a.jpg", "wb").write(r.content)      # 字节来自 CDN → COS
```

真正的路径是：

```
客户端 ──GET /files/a.jpg──▶ API（鉴权、记账）──302──▶ 客户端 ──▶ CDN ──▶ COS
```

- **Location 必须指向独立的数据域名**（如 `data.example.com`）。同域重定向不会剥掉
  `Authorization`，token 会被带到 CDN/COS 的日志里；跨域时主流客户端（requests、fetch、
  Go、curl）都会自动剥掉它。这条是硬要求。
- 支持 `Range`（断点续传、视频拖动）。
- 不能跟重定向的客户端：`GET /files/<path>?json=1` → `{"url":…,"name":…,"size":…,"mime":…}`，
  自己发 GET，**不要带 `Authorization`**。这是全 API 唯一一个改响应类型的查询参数。
- 路径是目录时返回 `400 is_directory`，请用 `/list`。

### 4.3 元信息

```http
HEAD /files/Photos/2024/a.jpg
→ 200
   Content-Length: 248123
   Content-Type: image/jpeg
   ETag: "9f2b…"
   Last-Modified: Fri, 24 Oct 2026 02:11:07 GMT
```

**只有 HEAD。** 早先的 `GET /files/<path>?meta=1` 已移除（同一个东西两个入口，
是 API 里最容易长出来的脂肪）；现在带 `meta=1` 会返回 `400`，并在 message 里指到 HEAD。

### 4.4 写小文件（一步快捷方式）

```http
PUT /files/Apps/badminton/analyses/a.json
Authorization: Bearer eam_live_xxx
Content-Type: application/json

{"score": 21}
```

```
201 Created
Content-Type: application/json

{"name":"a.json","path":"Apps/badminton/analyses/a.json","type":"file","size":12,
 "mime":"application/json","etag":"…","createdAt":"…","modifiedAt":"…"}
```

- **默认不覆盖**：路径已存在时返回 `409`。要覆盖就显式加 `?overwrite=1`，
  或者带匹配的 `If-Match`（更安全，能防止并发覆盖）。
- 覆盖是直接替换，**不保留历史版本**。
- `Content-Type` 落为 `mime`，缺省 `application/octet-stream`。
- `If-None-Match: *` 仍然支持（只在不存在时创建），失败返回 `412`。
- 超过 32 MiB 返回 `413`，并提示改用 `/upload`。
- **不想在"大小"上做判断的客户端，可以永远用 `/upload`——它对任何大小都成立。**

### 4.5 上传（所有大小，统一三步）

字节全程直连 COS，不经过服务器。小文件就是"只有一个分片"，客户端代码不需要分支。

```http
POST /upload
{"path":"Videos/a.mp4","size":18234122311,"mime":"video/mp4"}
```

```json
{
  "uploadId": "u_7f3a…",
  "mode": "multipart",
  "size": 18234122311,
  "partSize": 67108864,
  "parts": [
    {"partNumber": 1, "url": "https://data.example.com/…?signA", "headers": {"Content-Type": "video/mp4"}},
    {"partNumber": 2, "url": "https://data.example.com/…?signB", "headers": {"Content-Type": "video/mp4"}}
  ],
  "expiresAt": "2026-10-24T03:00:00Z"
}
```

```python
# 客户端：不分大小、不分片数，就这一套
for p in resp["parts"]:
    requests.put(p["url"], data=chunk(p["partNumber"]), headers=p["headers"])

requests.post(f"{API}/upload/complete", headers=auth, json={
    "uploadId": resp["uploadId"],
    "parts": [{"partNumber": p["partNumber"], "etag": p["etag"]} for p in done],
})
```

```http
POST /upload/complete
{"uploadId":"u_7f3a…","parts":[{"partNumber":1,"etag":"\"9f2b…\""}]}
→ 201 <file 对象>
```

- 分片数由 `size` 决定：≤ 64 MiB 只有一片，更大自动切片。**接口形状不变**，
  所以以后加并发、断点续传都不用改客户端。
- 分片 PUT 到 `data.example.com`（数据域名），不带 `Authorization`，靠 URL 里的签名。
- `complete` 会 `HeadObject` 校验大小，然后落索引。不调用也能用，只是索引晚一点。
- 单分片上传可以不传 `parts`，服务端自己 HEAD。
- 上传过程中重新 `POST /upload` 会作废旧的那次。

#### 断了先查，别从头再来

```http
GET /upload/u_7f3a…
→ 200
{
  "uploadId": "u_7f3a…",
  "path": "Videos/a.mp4",
  "mode": "multipart",
  "size": 18234122311,
  "partSize": 67108864,
  "expiresAt": "2026-10-24T03:00:00Z",
  "status": "pending",
  "received": [
    {"partNumber": 1, "size": 67108864, "etag": "\"9f2b…\""},
    {"partNumber": 2, "size": 12345678, "etag": "\"1a2c…\""}
  ]
}
```

`received` 是 COS 侧已经收到的分片（服务端转成 `ListParts`），客户端只补没传的。
`status` 是 `pending` / `completed` / `cancelled` / `expired`。
单分片模式返回空的 `received`——直接整片重传即可，PUT 本身是幂等的。

> **当前实现**：`size` ≤ 5 GiB（或 `UPLOAD_MULTIPART_THRESHOLD`）时给一个分片；
> 超过就自动切成多片（每片默认 64 MiB，片数 ≤ 1000，超出时自动放大分片）。

### 4.6 删除

```http
DELETE /files/Documents/old.pdf
→ 204
```

- **永久删除**：对象和索引一起删掉，没有回收站。
- `DELETE /files/<dir>` 需要 `?recursive=1`，否则 `409`——这个参数就是防手滑的提醒。
- 客户端删之前应该自己确认一次（网页上就是那个弹窗）。

### 4.7 移动 / 改名

```http
POST /move
{"from":"Notes/a.md","to":"Notes/b.md","overwrite":false}
→ 200 {"name":"b.md","path":"Notes/b.md", …}
```

- **文件**：同步完成，直接返回移动后的对象。
- **目录**：N 个对象就是 N 次服务端复制。服务端最多同步尝试 30 秒：
  搬完了回 `200`，没搬完回 `202` + `Location`。

```http
POST /move
Prefer: respond-async, wait=2
{"from":"Videos/src","to":"Videos/dst"}

→ 202
   Location: /move/m_5f90…
   Retry-After: 2
   {"id":"m_5f90…","op":"move","from":"Videos/src","to":"Videos/dst",
    "status":"running","total":412,"done":180}
```

```http
GET /move/m_5f90…
→ 200 {"id":"m_5f90…","status":"running","total":412,"done":180,"cancelRequested":false}
→ 200 {"id":"m_5f90…","status":"done","total":412,"done":412}
```

- 状态：`pending → running → done`；失败是 `failed` 并带 `error`；`cancelRequested` 为真时
  当前这一项搬完就会停，然后变成 `cancelled`。
- `POST /move/{id}/cancel` 请求停下；`POST /move/{id}/retry` 重新扫一遍来源，接着没搬完的继续。
- 移动是"先复制、再删源、最后改索引"，所以搬完之前**源还在**，可以重复读。
- 目录没有"一次性改名"的原语，所以它天生可能是长操作；但对外没有"任务"这个概念——
  只有"这次移动还没搬完"。

### 4.8 复制

```http
POST /copy
{"from":"Photos/2024/a.jpg","to":"Backup/a.jpg","overwrite":false}
→ 201 <新文件对象>
```

- 文件和目录都支持。目录是递归复制，同样遵守 §4.12 的 `Prefer` 规则，
  进度在 `GET /copy/{id}`，取消/重试是 `POST /copy/{id}/cancel|retry`。
- 目标已存在且 `overwrite=false` → `409`；目录复制**不支持合并**，目标存在就是 `409`。
- 复制出来的对象是**新对象**：新的创建时间、首次索引就是新的一行。源不变。
- 目标在源内部（`copy a → a/b`）返回 `400`。

### 4.9 建目录

```http
POST /mkdir
{"path":"Photos/2024/东京"}
```

```json
201 {"name":"东京","path":"Photos/2024/东京","parent":"Photos/2024","type":"dir","size":0,
     "createdAt":"2026-10-24T02:11:07Z","modifiedAt":"2026-10-24T02:11:07Z"}
```

| 情况 | 返回 |
|---|---|
| 新建 | `201` + 目录对象 |
| 已存在且是目录 | `200` + 目录对象（**幂等**，agent 重试不会炸） |
| 已存在但是文件 | `409` |
| 父目录不存在 | 自动建（`mkdir -p`），不多一个参数 |
| 父路径是个文件 | `409` |

为什么是 `POST /mkdir` 而不是 `PUT /files/<dir>/`：目录是"没有字节的资源"，
和文件共用 `/files/<path>` 就只能靠**尾斜杠**区分，而尾斜杠是 WebDAV 里最大的坑。
用动词 + body，路径永远是路径，没有歧义。

### 4.10 搜索

```http
GET /search?q=羽毛球&path=Videos&mime=video/&type=file&limit=50
```

返回形状和 `/list` 一样（含 `writable`），游标规则也一致。匹配文件名和完整路径。
`type` 是 `file` 或 `dir`。

### 4.11 我是谁 / 这个服务有多大

```http
GET /me
```

```json
{
  "user": {"email": "you@example.com"},
  "token": {"name":"badminton","prefix": "Apps/badminton", "read": true, "write": true},
  "usage": {"bytes": 1823412231, "files": 20481}
}
```

给 Agent 自检用：确认自己的前缀和权限。

```http
GET /capabilities        # 免鉴权，token 都不用带
```

```json
{
  "maxPutBytes": 33554432,
  "maxUploadBytes": 5497558138880,
  "multipartThreshold": 5368709120,
  "partSize": 67108864,
  "maxParts": 1000,
  "list": {"defaultLimit": 1000, "maxLimit": 5000},
  "rateLimit": {"perMinute": 600},
  "quotaBytes": 0,
  "async": {"header": "Prefer", "value": "respond-async", "defaultWaitSeconds": 30, "maxWaitSeconds": 60},
  "features": ["range", "if-match", "overwrite", "recursive-delete", "copy", "search"]
}
```

客户端启动时读一次，别把 32 MiB / 64 MiB 写死在自己的源码里。

### 4.12 长操作：`Prefer: respond-async`

**所有改状态的请求都接受它**，包括 `PUT`、`DELETE`，不只 move/copy：

```http
Prefer: respond-async            # 别等，立刻给我 202
Prefer: respond-async, wait=10   # 最多等 10 秒，没完就 202
（不传）                          # 服务端自己最多等 30 秒，没完就 202
```

- 回 `202` 时带 `Location: /<动作>/{id}` 和 `Retry-After`，轮询那个地址即可。
- 回 `200`/`201` 表示**已经做完了**，客户端要同时处理两种情况——一行分支：

```python
resp = post("/move", json={"from": a, "to": b}, headers={"Prefer": "respond-async, wait=10"})
if resp.status_code == 202:
    while (job := get(resp.headers["Location"])).json()["status"] in ("pending", "running"):
        time.sleep(2)
```

- 服务端**可以忽略**这个头（RFC 7240 允许）。现在真正会回 202 的只有目录 `move` / `copy`；
  `mkdir`、单文件操作、`DELETE` 都毫秒级完成，一定回 200/201/204。
  哪天某个操作变慢了，它会自己开始回 202，**客户端那行代码不用改**。
- 202 的响应体和 `GET /<动作>/{id}` 的响应体**同形**，一个解析器。
- 进度地址永远是"它自己的动作路径 + id"，没有 `/tasks`、没有队列名、没有 `type` 字段。

### 4.13 事件（时间线）

文件 API 之外的第二类东西。**一个事件 = 一段时间 + 一张卡 + 一个可选的详情链接**，
设计见 [`docs/design/events.md`](../design/events.md)。

事件不是文件：文件用路径当身份，事件有 `id`；对外稳定句柄是你自己的 `external_id`。

令牌需要 `can_events` 能力位：

```bash
eam token create --email you@example.com --name Strava --prefix Apps/strava --events
```

#### 创建 / 更新（幂等）

```http
POST /events

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
      {"label": "距离", "value": "12.4", "unit": "km"},
      {"label": "配速", "value": "5'02\"", "unit": "/km"}
    ],
    "body": "出门时 24 度。"
  }
}
```

```json
201 { "id": "0f1c…", "state": "published", "created": true }
```

- 同一个 `(token, external_id)` 再来一次是**整体替换**，返回 `200` + `created:false`；
- 用户删掉的事件再推 → `410 gone`，请停止重试；
- 媒体写路径（`payload.media: ["Apps/strava/2025/0803/a.jpg"]`），必须在 token 的前缀内，
  先通过文件 API 上传；`photo` 最多 4 张，不做外链图片；
- `tz` 缺省用服务端配置的 `TIMELINE_TZ`（默认 `Asia/Shanghai`）。

四种 `type` 与 payload：

| type | payload | 限额 |
|---|---|---|
| `photo` | `media[1–4]` + 可选 `body` / `meta` | 标题 80 字，正文 500 字 |
| `note` | `body` + 可选 `meta` | 同上 |
| `metric` | `metrics[1–3]` + 可选 `body` | label / value 必填 |
| `html` | `html` + `height` | html ≤ 32 KB，height 40–640 |

`html` 卡片由我们放在 `sandbox` iframe 里渲染（脚本禁用、CSP 限制），并注入设计 token
`--eam-bg` / `--eam-surface` / `--eam-text` / `--eam-text2` / `--eam-text3` / `--eam-line` / `--eam-accent`。

#### 删除 / 读取

```http
DELETE /events/{id}          → {"state":"detached"}   内容保留，外链清空
GET    /events?from=&to=     → 对账：你自己写过的事件（含 external_id）
GET    /events/{id}          → 单条完整 JSON
```

没有 `PATCH`：重推就是更新。

### 4.14 保存链接（给 agent）

连接不需要先"配对"：用户把一条 URL 发给 agent，URL 本身就是凭证和说明书。

```http
GET /s/abcd2345efgh2345
```

- `Accept: text/markdown`（或 `curl` 默认）返回给 agent 的 prompt：token、POST 地址、内容格式、返回形状。
- 浏览器打开返回一页说明。
- `?format=json` 返回 `{token, url, save, prompt, ...}`，给不能读 prompt 的 App。

```markdown
# everythingabout.me

Connected to this drive.

Token: abcd2345efgh2345
Save: POST https://everythingabout.me/s/abcd2345efgh2345
Content-Type: text/markdown (or image/jpeg, application/pdf, video/mp4, ...)

  <content or file bytes>

Optional query: ?title=...&filename=photo.jpg
Response: {"path": "...", "url": "...", "mime": "...", "size": ...}

Files over 32 MiB:
  POST https://everythingabout.me/upload
  {"path": "Inbox/2026/10/28/photo.jpg", "size": 12345678, "mime": "image/jpeg"}
  PUT the bytes to each returned part URL, then POST https://everythingabout.me/upload/complete

Full API: Authorization: Bearer abcd2345efgh2345
```

保存就是往同一个 URL POST 内容：

```http
POST /s/abcd2345efgh2345
Content-Type: text/markdown

今天聊出来的那段……
```

```json
201 {"path":"Inbox/2026/10/27/关于简单与信任-6f3a9c2b.md","url":"https://…/drive?path=…","title":"…","mime":"text/markdown","size":42,"duplicate":false}
```

- 短码是 16 位小写 base32，**不带 `eam_` 前缀**；`Authorization: Bearer <短码>` 也能直接走完整文件 API。
- 保存链接和用户本人同权：不设前缀、不设能力位、默认不过期。
- **不按文件类型分支**：`POST /s/<code>` 的 body 原样存盘，`Content-Type` 决定 mime；没有 `?filename=` 时按 mime 推扩展名。文本、图片、PDF 都一样，32 MiB 以内一个请求完成。
- 超过 32 MiB 的文件（常见是视频）用同一把 token 调 `/upload` 拿预签名地址，字节直传 COS，不经过服务器。
- 文件名由标题 + 内容 SHA-256 决定：同一天重复提交同一段内容返回 `200` + `duplicate:true`，不会存两份。
- 落盘为 `Inbox/YYYY/MM/DD/<title>-<hash>.<ext>`，**文件里只有提交的内容，没有 frontmatter**；标题、来源、时间写在 `nodes.meta` 里。
- 每次保存都写 `audit_log`（action=`capture`），和文件 API 的写操作一样可追溯。
- 账户页的 `/api/save-links`（cookie 会话）负责创建、列表、撤销；撤销后 `/s/<code>` 立即 404。
- 短码里的 MAC 由服务端密钥派生；换掉 `COS_SECRET_KEY` 会让已有保存链接失效。

---

## 5. 服务端偷偷做的事

调用方不需要知道，但用户需要：

| 行为 | 对调用方的表现 |
|---|---|
| 对象 key 就是真实路径 | 用任何 S3 工具都能直接看到 |
| 维护目录索引 | 就是列得出来 |
| 计算 sha256、记审计 | 账户页的“最近活动”里能看到 |
| 读时修复（桶里有、索引没有） | 第一次 HEAD/GET 就自动补上 |

分享、配额、账号管理都在**网页**上，不进这份 API。

---

## 6. 状态码

| 码 | 含义 |
|---|---|
| 200 / 201 / 204 | 成功（201 = 新建，204 = 删掉了、没有响应体） |
| 202 | 收下了，去 `Location` 看进度 |
| 206 | Range |
| 302 | 下载跳转 |
| 304 | 未修改 |
| 400 | `invalid_path`、请求体不合法、`meta=1` 已移除 |
| 401 | token 缺失/无效 |
| 403 | 前缀越界、只读 token 尝试写 |
| 404 | 不存在 |
| 409 | 同名冲突、非空目录、移动/复制对象过多 |
| 412 | 条件写失败 |
| 413 | 超过 32 MiB，请用 `/upload` |
| 429 | 限流，带 `Retry-After` |
| 500 | 服务端错误；`job_failed` 时带 `id`，可以 `POST /{动作}/{id}/retry` |
| 507 | 配额用尽 |

---

## 7. 实现计划（Go）

| 阶段 | 内容 | 状态 |
|---|---|---|
| P1 | `api_tokens` 表、Bearer 认证、`/list` `/files` `/move` `/me`、预签名下载、CORS | ✅ |
| P2 | `/upload` + `/upload/complete`（单分片） | ✅ |
| P3 | `/search`、目录移动（异步）、配额与限流、审计 | ✅ |
| P4 | 真分片（> 5 GiB） | ✅ |
| P5 | `mkdir`、`copy`、`Prefer: respond-async` + cancel、`/upload/{id}` 续传、`/capabilities` | ✅ |
| P6 | —（明确不做版本历史、回收站、`id`、`/tasks`） | — |
| P7 | 保存链接 `/s/<code>`、账户页链接与最近活动 | ✅ |

已实现的细节：

- 令牌 `eam_<12 位 id>_<secret>`，库里只存 `sha256(secret)`；校验用常量时间比较。
- 权限：`prefix`（空 = 整个盘）+ 只读/读写；越界返回 `403`，不依赖调用方自觉。
- 列目录用 keyset 分页（`is_dir desc, lower(name), id`），游标是不透明 base64。
- `PUT` 支持 `If-Match` / `If-None-Match: *`；超过 32 MiB 返回 `413` 并提示用 `/upload`。
- 移动/复制是"先复制、再删源、最后改索引"：复制失败时源还在，不会丢数据。
- 大文件走上传会话：`POST /upload` 建会话（上限 5 TiB）并下发预签名 PUT 地址；
  同一路径的新上传会作废旧会话；`complete` 用 `HeadObject` 校验实际大小。
- 长操作不建"任务中心"：每个动作自己带进度地址（`/move/{id}`、`/copy/{id}`），
  作业表只有一张（`fs_jobs`，带 `op` 列），纯内部实现。
- 搜索支持 `q`（文件名/路径）、`path`、`mime`、`type` 过滤，游标分组和 `/list` 一致。
- 配额：`USER_QUOTA_BYTES`（0 = 不限），写入前按"已用 − 旧文件 + 新文件"检查，超了返回 `507`。
- 限流：每个令牌（没有令牌按 IP）600 次/分钟，超限返回 `429` + `Retry-After`、`X-RateLimit-Limit`。
- 审计：写操作记进 `audit_log`（action / path / status / ip / token / detail），写失败只打日志。
- 分片上传：超过阈值自动切片，每片一个预签名 URL；`complete` 时把 `{partNumber, etag}` 交给 COS 合并。
- 错误体：`{"error":"not_found","message":"..."}`，HTTP 状态码同步给；`message` 统一是英文（小写、无句号），`error` 是稳定的机器可读码。
- 保存链接：16 位 base32 短码，GET 出 prompt、POST 存正文；按正文 hash 去重，落 `Inbox/` 下的 Markdown。

网页用的 `/api/fs/*` 照旧，不动。对外 API 在 `backend/internal/api/api_*.go`，
认证在 `backend/internal/auth/token.go`，作业在 `backend/internal/jobs`。

---

## 8. 四个刻意的取舍

### 8.1 不做版本历史，也不做回收站

覆盖必须显式 `?overwrite=1`，删除就是永久删除。文件夹就该是文件夹：
少一层"隐藏状态"，用户和 Agent 都少一个要理解的概念。

### 8.2 URL 不带版本号

这是给程序用的稳定接口：加字段不破坏，删字段提前公告。
真要断代，用 `Sunset` 响应头 + 公告，而不是换路径。

### 8.3 没有 SDK 也够用

一共十来个端点，全是 HTTP 动词和 JSON；二十行 `requests` 就能封装完。
以后要 CLI 或 MCP server，都是在这层之上加，不改协议。

### 8.4 不暴露 id，也不暴露"任务"

两条都是"把实现细节留在实现里"：

- **id**：S3 没有对象 id，WebDAV 没有 id，有 id 的（Drive `fileId`、Dropbox `id:`）
  都是自己铸的。我们内部有行主键（改名/移动时不变），但**不进 JSON**。
  对外身份 = 路径，内容身份 = `sha256`。代价只有一个：在本地维护索引的 App，
  用户重组目录后认不出"同一批文件"——那就让它把索引跟着数据走（存在被分析的目录下面），
  而不是跨树引用。这样也少一类"路径在、id 不在"的 404 语义。
- **任务**：用户要的是"这个目录搬完了没有"，不是"什么是异步任务"。
  所以没有 `/tasks`、没有队列名、没有 `type` 字段，只有每个动作自己的 `{id}`。

参考 OpenList（AList 分支）时**只抄能力，不抄形状**：它的 `fs/mkdir`、`fs/copy`、
`fs/multipart/*`、`fs/task/*` 我们抄了语义；它的 `POST 一切`、`{code,message,data}`
包封、`page/per_page` 偏移分页、84 条 task 子路由，一条都不抄。
