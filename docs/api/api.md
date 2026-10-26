# Everything About Me API

> 我们自己的接口。**语义和 S3 / Drive / Dropbox 是同一套**——同样是"对一棵路径树做增删改查"，
> 只是形式由我们定义。URL 上不带版本号。

---

## 0. 一句话

一个 Bearer token，对一棵路径树做九件事。**文件字节不经过服务器**：读写都是拿一个地址直接跟 COS 换。

```
POST /upload          → 拿上传地址（小文件只有一个分片）
PUT  <那个地址>        → 字节直连 COS
GET  /files/<path>    → 302 到下载地址

其他全部走我们：/list /search /move /delete /me
```

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
| 删除 | `DeleteObject` | `files.delete` | `files/delete_v2` | `DELETE /files/<path>` |
| 移动/改名 | `CopyObject`+`Delete` | `files.update` | `files/move_v2` | `POST /move` |
| 搜索 | — | `files.list(q=)` | `files/search_v2` | `GET /search?q=` |

请求参数和返回值也尽量贴近：路径、`cursor` 分页、`etag`、`size`、`mime`、`modifiedAt`。
**没有一个是新概念。**

---

## 3. 约定

| 项 | 约定 |
|---|---|
| Base URL | `https://your-host` |
| 认证 | `Authorization: Bearer eam_live_xxx` |
| 路径 | 逻辑路径，不带前导 `/`，按段 percent-encode（`/` 不编码） |
| 时间 | ISO-8601 UTC，`2026-10-24T02:11:07Z` |
| 分页 | `cursor` 是不透明字符串，`limit` 默认 1000、上限 5000 |
| 错误 | `{"error":"not_found","message":"..."}` + 正确的 HTTP 状态码 |
| 字符 | UTF-8；服务端统一 NFC |
| 大小 | 单次 `PUT` ≤ 32 MiB；更大的走 `/upload` |

---

## 4. 接口

### 4.1 列目录

```http
GET /list?path=Photos/2024&limit=200&cursor=
```

```json
{
  "path": "Photos/2024",
  "items": [
    {
      "name": "a.jpg",
      "path": "Photos/2024/a.jpg",
      "type": "file",
      "size": 248123,
      "mime": "image/jpeg",
      "etag": "9f2b…",
      "sha256": null,
      "createdAt": "2026-10-24T02:11:07Z",
      "modifiedAt": "2026-10-24T02:11:07Z"
    },
    {"name": "raw", "path": "Photos/2024/raw", "type": "dir", "modifiedAt": "2026-10-20T08:00:00Z"}
  ],
  "cursor": null
}
```

- 目录在前，再按 `lower(name)` 排。
- `cursor` 为 `null` 表示没有下一页。
- 空目录也会返回 `type: "dir"`（服务端建目录时写了占位对象 + 索引记录）。

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
- 不能跟重定向的客户端：`GET /files/<path>?json=1` → `{"url":…,"name":…,"size":…}`，
  自己发 GET，**不要带 `Authorization`**。
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

要 JSON 形状：`GET /files/<path>?meta=1`。

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

> **当前实现**：`size` ≤ 5 GiB（或 `UPLOAD_MULTIPART_THRESHOLD`）时给一个分片；
> 超过就自动切成多片（每片默认 64 MiB，片数 ≤ 1000，超出时自动放大分片）。
> 响应里 `mode` 会是 `single` 或 `multipart`，客户端代码完全一样：
> 循环 `parts` 逐个 PUT，最后把 `{partNumber, etag}` 一起提交给 `/upload/complete`。

### 4.6 删除

```http
DELETE /files/Documents/old.pdf
→ 204
```

- **永久删除**：对象和索引一起删掉，没有回收站。
- `DELETE /files/<dir>` 需要 `?recursive=1`，否则 `409`——这个参数就是防手滑的提醒。
- 客户端删之前应该自己确认一次（网页上就是那个弹窗）。

### 4.7 移动 / 改名

**文件**：同步完成，直接返回结果。

```http
POST /move
{"from":"Notes/a.md","to":"Notes/b.md","overwrite":false}
→ 200 {"name":"b.md","path":"Notes/b.md",...}
```

**目录**：后台任务，立刻返回 `202` 和一个任务 ID。

```http
POST /move {"from":"Videos/src","to":"Videos/dst"}
→ 202 {"id":"m_5f90…","status":"pending","total":0,"done":0}
```

```http
GET /move/m_5f90…
→ 200 {"id":"m_5f90…","status":"running","total":412,"done":180}
```

任务状态：`pending → running → done`，失败是 `failed` 并带 `error`。
失败后可以 `POST /move/{id}/retry`，它会重新扫一遍来源，接着没搬完的继续。

- 目录没有"一次性改名"的原语：N 个文件就是 N 次服务端复制，所以放后台。
- 文件移动是"先复制、再删源、最后改索引"；目录任务同样是逐项这个顺序。
- 目标已存在且 `overwrite=false` 时返回 `409`；目录移动不支持 `overwrite`。

### 4.8 搜索

```http
GET /search?q=羽毛球&path=Videos&mime=video/&limit=50
```

返回形状和 `/list` 一样，外加 `parent`。匹配文件名和完整路径。

### 4.9 我是谁

```http
GET /me
```

```json
{
  "user": {"email": "you@example.com"},
  "token": {"prefix": "Apps/badminton", "read": true, "write": true},
  "usage": {"bytes": 1823412231, "files": 20481}
}
```

给 Agent 自检用：确认自己的前缀和权限。

---

## 5. 服务端偷偷做的事

调用方不需要知道，但用户需要：

| 行为 | 对调用方的表现 |
|---|---|
| 对象 key 就是真实路径 | 用任何 S3 工具都能直接看到 |
| 维护目录索引 | 就是列得出来 |
| 计算 sha256、记审计 | 看不见 |
| 读时修复（桶里有、索引没有） | 第一次 HEAD/GET 就自动补上 |

分享、配额、账号管理都在**网页**上，不进这份 API。

---

## 6. 状态码

| 码 | 含义 |
|---|---|
| 200 / 201 / 204 | 成功 |
| 206 | Range |
| 302 | 下载跳转 |
| 304 | 未修改 |
| 400 | `invalid_path`、请求体不合法 |
| 401 | token 缺失/无效 |
| 403 | 前缀越界、只读 token 尝试写 |
| 404 | 不存在 |
| 409 | 同名冲突、非空目录、移动对象过多 |
| 412 | 条件写失败 |
| 413 | 超过 32 MiB，请用 `/upload` |
| 429 | 限流，带 `Retry-After` |
| 507 | 配额用尽 |

---

## 7. 实现计划（Go）

| 阶段 | 内容 | 状态 |
|---|---|---|
| P1 | `api_tokens` 表、Bearer 认证、`/list` `/files` `/move` `/me`、预签名下载、CORS | ✅ 已实现 |
| P2 | `/upload` + `/upload/complete`（单分片，响应形状已按多分片设计） | ✅ 已实现 |
| P3 | `/search`、目录移动（异步）、配额与限流、审计 | ✅ 已实现 |
| P4 | 真分片（> 5 GiB） | ✅ 已实现 |
| P5 | —（明确不做版本历史和回收站） | — |

已实现的细节：

- 令牌 `eam_<12 位 id>_<secret>`，库里只存 `sha256(secret)`；校验用常量时间比较。
- 权限：`prefix`（空 = 整个盘）+ 只读/读写；越界返回 `403`，不依赖调用方自觉。
- 列目录用 keyset 分页（`is_dir desc, lower(name), id`），游标是不透明 base64。
- `PUT` 支持 `If-Match` / `If-None-Match: *`；超过 32 MiB 返回 `413` 并提示用 `/upload`。
- 移动是"先复制、再删源、最后改索引"：复制失败时源还在，不会丢数据。
- 大文件走上传会话：`POST /upload` 建会话（上限 5 GiB）并下发预签名 PUT 地址；
  同一路径的新上传会作废旧会话；`complete` 用 `HeadObject` 校验实际大小，不匹配就拒绝。
- 搜索支持 `q`（文件名/路径）、`path`、`mime`、`type` 过滤，游标分组和 `/list` 一致。
- 目录移动是后台任务（`move_jobs`），进度可查、失败可重试；服务重启会把没跑完的标成失败。
- 配额：`USER_QUOTA_BYTES`（0 = 不限），写入前按"已用 − 旧文件 + 新文件"检查，超了返回 `507`。
- 限流：每个令牌（没有令牌按 IP）600 次/分钟，超限返回 `429` + `Retry-After`、`X-RateLimit-Limit`。
- 审计：写操作记进 `audit_log`（action / path / status / ip / token / detail），写失败只打日志。
- 分片上传：超过阈值自动切片，每片一个预签名 URL；`complete` 时把 `{partNumber, etag}` 交给 COS 合并，
  再 `HeadObject` 校验最终大小。旧的 / 过期的会话会 `AbortMultipartUpload`，不留碎片。
- 错误体：`{"error":"not_found","message":"..."}`，HTTP 状态码同步给。
- 同一套接口也接受网页的 cookie 会话（全权限），便于以后前端直接复用。

现有 `/api/fs/*` 继续服务网页，不动。新接口在 `backend/internal/api/api_files.go` 和
`api_upload.go`，认证在 `backend/internal/auth/token.go`，复用 `nodes` / `storage` / `paths`。

---

## 8. 三个刻意的取舍

1. **不做版本历史，也不做回收站。** 覆盖必须显式 `?overwrite=1`，删除就是永久删除。
   文件夹就该是文件夹：少一层"隐藏状态"，用户和 Agent 都少一个要理解的概念。

2. **URL 不带版本号。** 这是给程序用的稳定接口：加字段不破坏，删字段提前公告。
   真要断代，用 `Sunset` 响应头 + 公告，而不是换路径。

3. **没有 SDK 也够用。** 九件事，全是 HTTP 动词和 JSON；十行 `requests` 就能封装完。
   以后要 CLI 或 MCP server，都是在这层之上加，不改协议。
