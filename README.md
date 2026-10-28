# everything about me

个人数据基座：**邮箱注册的账号系统 + 文件层 + 一条自动整理的内容流**。
账号、会话、目录树、属性、检索、内容流放 Postgres；文件字节放腾讯云 COS。

前后端分离：**Go 后端（net/http） + React 前端（Vite + React Router）**，没有 Next.js。

```
浏览器 ──登录态 cookie──▶ Go API ──▶ Postgres (users / sessions / nodes)
   │                        │
   │                        └──▶ COS (drive/<用户ID>/…)
   └──STS 临时凭证直传──▶ COS
```

## 技术栈与目录

| 路径 | 内容 |
|---|---|
| `backend/` | Go 1.26+。标准库 `net/http`（Go 1.22 路由模式）、pgx、腾讯云 COS SDK。 |
| `web/` | React 19 + Vite + React Router（data router / loader）。 |
| `sql/` | 按文件名顺序执行的建表迁移。 |
| `email-templates/` | SES 验证码邮件模板参考（登录 / 注册 / 改邮箱共用一份）。 |
| `docs/` | 设计草案与截图。内容流的设计见 [`docs/design/timeline.md`](docs/design/timeline.md)。 |

后端是一个二进制，同时提供 JSON API 和前端构建产物：

```bash
make build          # 构建 web/dist，再编译出 bin/eam
./bin/eam server    # 生产模式：/ 返回 SPA，/api/* 返回接口
```

开发时前端跑 Vite（5173，`/api` 代理到 8080），后端跑 Go（8080）：

```bash
make dev
```

## 用户系统

- **邮箱验证码登录 / 注册合并**：输入邮箱 → 收验证码 → 已有账号直接登录，没有账号自动创建。没有用户名，默认也不需要密码。
- 每个账号有一个**唯一且不变的 `users.id`（UUID）**作为内部身份；用户不需要关心它，界面上邮箱就是唯一标识。改邮箱时 id 和文件归属都不变。
- 密码是**可选的**：账户页可以设置、修改或移除；设置后用 `/login?mode=password` 登录（登录页不显示密码入口）。密码用 Go 的 `scrypt` 加盐哈希，**格式与改造前完全一致**（`scrypt$N$r$p$salt$hash`），老密码可以直接用。
- 登录态是 httpOnly cookie（`eam_session`），数据库只存 token 的 SHA-256；默认 30 天过期。token 格式也与旧版一致，迁移后已有会话不会失效。
- 文件按 `owner_id` 隔离，COS 里每个用户一个独立前缀 `drive/<用户ID>/`，不同账号可以有同名路径。
- `/account` 可以修改邮箱（需要**当前邮箱 + 新邮箱**两个验证码），也可以设置/移除密码；界面只显示邮箱和注册时间。
- 注册默认开放；个人实例注册完自己后，建议把 `ALLOW_REGISTRATION=false` 写进 `.env` 并重启。

## 为什么不是"只用 S3"

S3 只能按前缀列举、不能搜索、没有标签。这张 `nodes` 表就是补上这层：
它是**索引**，不是副本；对象字节永远只在 COS 里。

## 目录约定

| 前缀 | 用途 |
|---|---|
| `drive/<用户ID>/` | 用户可见文件，key 就是真实路径 |
| `_inventory/` | COS 清单输出（可选） |

key 直接落在路径上（`drive/<用户ID>/Photos/2024/a.jpg`），意味着：

- 用 `coscli` / `rclone` 直接往桶里放文件，跑一遍 `reconcile --owner` 就出现在网站里；
- 哪天不用这个站了，`coscli sync` 拉下来就是完整的目录结构，不需要跑任何程序；
- 删除账号时数据库会级联清掉索引，但 COS 对象需要自己按前缀清理。

## 界面

- **中英双语**：跟随系统/浏览器的首选语言自动选择（`zh` → 中文，其余 → 英文），没有语言设置入口；内容流的月份、卡片副标题和相对时间也会跟着变
- 登录 / 注册（合并为一个页面）/ 账户三个页面，跟随系统深浅色
- `/` 是一条扁平的事件流：一张卡就是一个事件（照片 / 一句话 / 一组数字 / 应用自己的 HTML），没有年、月标题，也没有统计数字
- `/drive` 是文件浏览器：面包屑、相对时间、类型图标、悬停操作
- 点文件名就地开预览弹层（`Esc` 关闭）：图片、PDF、Office（docx / xlsx / pptx）、纯文本与源码、音视频；
  预览器（[open-file-viewer](https://github.com/xushanpei/open-file-viewer)）连同 pdfjs 走 `React.lazy`，
  首屏不下载，点开才拉一块（约 191KB gzip），具体格式的渲染器再按格式二次加载；认不出的类型仍走新标签下载
- 拖拽 / 多选上传：占位行即时出现，带队列、进度与速度，可取消，失败可重试；`/` 聚焦搜索，`Esc` 关闭弹层
- 搜索会同时匹配文件名和完整路径，结果右侧显示所在目录
- 新建目录是行内输入框，删除有确认弹窗（永久删除）
- 顶栏右侧显示当前账号，点进 `/account` 改邮箱、设置密码或退出
- 目录用 URL 的 `?path=` 表示，可以直接分享/刷新到某一层
- 前端路由：`/`（内容流）、`/drive`（文件）、`/login`（验证码登录，`?mode=password` 直达密码登录）、`/account`

## 起步

前置：**Go 1.26+**、**Node 20.19+**（只用来构建前端）、一个 Postgres。

```bash
cp .env.example .env      # 填 DATABASE_URL / COS_*
make install              # go mod download + 前端 npm install
make migrate              # 建表：nodes / users / sessions / email_codes
make check                # 自检：数据库 / COS 鉴权 / STS 签发 / SES
make dev                  # 后端 :8080 + 前端 :5173（/api 已代理）
```

打开 http://127.0.0.1:5173 ，输入邮箱收验证码；第一次验证会自动创建账号。
开发环境没配 SES 时，验证码会打印在 `make dev` 的后端终端里，也会自动填入登录页。

也可以分两个终端跑：

```bash
cd backend && go run ./cmd/eam server   # API + 静态文件
cd web && npm run dev                   # Vite dev server
```

> 后端命令会自动读取仓库根目录的 `.env`（从当前目录往上找三层），不用手动 export。
> `make build` 之后用 `./bin/eam server` 单进程就能跑完整站点：Go 会把 `web/dist` 作为 SPA 托管，未知路径回退到 `index.html`。

### 登录 / 注册验证码（腾讯云 SES）

登录、注册、改邮箱共用 6 位邮箱验证码。配好 SES 就真实发信；**没配 SES 时**，开发环境会把验证码打印到服务端日志并在登录页自动填入，生产环境会直接拒绝发送。

> 开发模式的验证码会直接返回给浏览器，所以不要把开发服务器暴露到公网。
> `APP_ENV=production` 时才会启用生产行为（cookie 加 Secure、SES 未配置时拒绝发码）。

1. 在腾讯云 SES 控制台完成发件域名 / 发件地址验证（含 SPF、DKIM）。
2. 新建一个验证码模板，内容例如：

   ```
   您的验证码是 {{code}}，10 分钟内有效。如果不是你本人操作，请忽略这封邮件。
   ```

   记下模板 ID。SES API 发信需要模板（`Simple` 字段已废弃）。
3. 在 `.env` 填：

   ```bash
   SES_REGION=ap-guangzhou
   SES_FROM_EMAIL_ADDRESS="Everything About Me <noreply@yourdomain.com>"
   SES_TEMPLATE_ID=123456
   SES_TEMPLATE_VAR=code
   SES_SUBJECT=Everything About Me 验证码
   # 不填 SES_SECRET_ID / SES_SECRET_KEY 时复用 COS 的密钥
   ```

   模板正文建议用通用文案（例如 `email-templates/sign-in-code.html`），不要在正文里写死“注册”；否则登录和改邮箱也会收到“注册”措辞。
   验证码邮件也**不区分登录和注册**：后端发码时能判断邮箱是否已注册，但把这个判断写进邮件等于对外暴露「这个邮箱有没有账号」，所以统一用一份模板。

4. 复用 COS 的子账号密钥时，给该子用户关联一个**只含发信权限**的策略（CAM → 用户 → 关联策略；或新建自定义策略 → 按策略语法创建）：

   ```json
   {
     "version": "2.0",
     "statement": [
       {
         "effect": "allow",
         "action": ["ses:SendEmail"],
         "resource": ["*"]
       }
     ]
   }
   ```

   如果这个 SecretId 是主账号密钥，本来就有权限，不需要再加策略；但主账号密钥权限过大，建议换成子账号。SES 预设的 `QcloudSESFullAccess` 也能用，只是权限更大。

模板变量名要和 `SES_TEMPLATE_VAR` 一致；`SES_SUBJECT` 可以改邮件标题。
验证码 10 分钟过期、最多尝试 5 次，发新码时旧码自动作废。

### 从旧版本升级（已有文件认领）

多用户改造之前的数据 `owner_id` 为空，不会自动出现在任何账号里。用验证码登录（会自动创建账号）后运行：

```bash
cd backend
go run ./cmd/eam claim --email you@example.com --dry-run   # 先看会把哪些对象搬到哪里
go run ./cmd/eam claim --email you@example.com             # 正式认领
```

它会把 COS 对象从 `drive/<路径>` 复制到 `drive/<用户ID>/<路径>`，改写数据库里的 key 和
`owner_id`，然后删掉旧对象（`--keep-source` 可保留）。数据量大时可以先用 coscli 分目录搬，
再跑 `reconcile --owner you@example.com`。

**从 Next.js 版本迁移不需要动数据**：表结构、COS key、密码哈希、会话 token 都没变，
直接用新的 `eam` 二进制启动即可，用户不用重新登录。

### COS 侧要配三样

1. **CORS**（允许浏览器直传，也允许预览直接读）
   - 来源：`http://localhost:5173`、`http://127.0.0.1:5173`、你的正式域名
   - 方法：`PUT, GET, POST, HEAD`
   - 头部：`*`，暴露头部：`ETag, x-cos-request-id`
   - `GET` 不只是下载：文件预览用 `fetch` 跟着 `/api/fs/download` 的 302 落到 COS，桶上没有 GET 的
     跨域规则时，预览会拿不到字节（浏览器控制台报 CORS，弹层里显示加载失败）
2. **子账号 + STS 权限**：主账号密钥只放在服务端，`eam` 会按单个对象的 key 下发临时凭证（30–60 分钟）。
3. **（可选）数据万象**：开了之后图片可以走 `?imageMogr2/thumbnail/300x` 出缩略图，不用自己跑 Sharp。

## 首次灌 TB 级数据

别走网页上传。用官方 CLI 直接进桶：

```bash
coscli cp -r ~/Pictures cos://your-bucket-1250000000/drive/<用户ID>/Pictures
cd backend
go run ./cmd/eam reconcile --owner you@example.com --dry-run   # 先看会写多少条
go run ./cmd/eam reconcile --owner you@example.com
```

百万对象时改用 COS **清单（Inventory）**，避免全量 List 的请求费：

```bash
go run ./cmd/eam reconcile --owner you@example.com --inventory ./inventory.csv
```

`reconcile` 做三件事：补新增、更新变动的（size/etag）、把桶里已消失的记录删掉。
它只扫描指定用户的前缀，不会碰别的账号；也不删任何对象——删除权只在你手里。

## 删除

删除是永久删除：对象和索引一起删掉，没有回收站。网页上会先弹确认框；
API 里删目录要显式带 `?recursive=1`，这是防手滑的提醒。

> 从旧版本升级：如果库里还有 `deleted_at` 非空的记录，先用旧版本跑一次
> `eam purge --days 0` 把对象也删掉，再执行 `migrate`。迁移会把这一列彻底去掉。

## 接口

### 对外 API（给第三方 App / 脚本 / AI agent）

一套自己的接口：一个 Bearer token，对一棵路径树做增删改查。设计和取舍见
[docs/api/api.md](docs/api/api.md) 与 [docs/api/flows.md](docs/api/flows.md)。

```bash
# 先给某个 App 发一个只能碰 Apps/badminton 的令牌
cd backend && go run ./cmd/eam token create \
  --email you@example.com --name badminton --prefix Apps/badminton

# 然后就是普通 HTTP
curl -H "Authorization: Bearer eam_..." https://your-host/list?path=Apps/badminton
```

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/capabilities` | 免鉴权：上限、能力、限流（客户端从这里读，别写死） |
| GET | `/list?path=&limit=&cursor=` | 列目录（keyset 分页，带 `writable`） |
| GET | `/files/<path>` | 302 到预签名下载地址；`?json=1` 拿地址 |
| HEAD | `/files/<path>` | 元信息（长度 / 类型 / ETag / 修改时间） |
| PUT | `/files/<path>` | 写文件（≤ 32 MiB；默认不覆盖，要覆盖加 `?overwrite=1`） |
| DELETE | `/files/<path>` | 永久删除（目录需 `?recursive=1`） |
| POST | `/mkdir` | `{"path"}` 建目录（自动建父目录；已存在返回 200，幂等） |
| POST | `/move` | `{"from","to","overwrite"}` 移动 / 改名 |
| POST | `/copy` | `{"from","to","overwrite"}` 复制（文件和目录） |
| GET | `/move/{id}` · `/copy/{id}` | 目录操作的进度（`POST .../cancel` · `.../retry`） |
| GET | `/search?q=&path=&mime=&type=` | 搜文件名和路径 |
| POST | `/upload` | 建上传会话，返回预签名 PUT 地址（超过阈值自动分片；覆盖需 `overwrite: true`） |
| GET | `/upload/{id}` | 查到哪了（已收到的分片），断了接着传 |
| POST | `/upload/complete` | 传完确认，校验大小后落索引 |
| GET | `/me` | 当前账号、令牌权限、用量 |
| GET/POST | `/s/<code>` | 给 agent 的保存链接：GET 出 token+prompt，POST 存正文 |

大文件流程：`POST /upload` 拿地址 → 直接 PUT 到 COS（不经过本服）→ `POST /upload/complete`。
写操作会记进 `audit_log`；限流是每令牌 600 次/分钟。
超过阈值的上传自动走分片（每片一个预签名 URL，`complete` 时合并）。

长操作统一用标准的 `Prefer: respond-async[, wait=N]`：服务端做完就回 `200`，
没做完回 `202` + `Location`（指向动作自己的 `/{id}`），客户端一行分支即可。
对外没有“任务”这个概念，也没有对象 `id`——路径就是身份。

保存链接（给 agent）：

```text
https://your-host/s/abcd2345efgh2345
```

这条 URL 本身就是 token 和说明书。agent 打开它拿到 prompt，往同一个 URL POST 内容，
就会存成 `Inbox/YYYY/MM/DD/<title>-<hash>.<ext>`，文件内容就是提交的原文。文本、图片、PDF 都走同一个入口，
32 MiB 以内的文件一个请求完成；更大的视频用同一把 token 走 `/upload` 直传 COS，字节不经过服务器。
账户页可以生成多条、看最近使用、随时撤销；
撤销后链接立即失效。短码不带 `eam_` 前缀，也可以直接作为 Bearer token 调用完整文件 API。
账户页的“最近活动”会按时间列出谁在什么时候改了哪个路径；保存链接和事件投稿都记进 `audit_log`。

令牌管理：

```bash
eam token list   --email you@example.com
eam token revoke --email you@example.com --id <令牌 ID>
```

### 网页用的接口

所有 `/api/fs/*` 都要求登录，并且只操作当前账号自己的文件（未登录返回 401）。
接口地址和响应信封（`{ok, data}` / `{ok, error}`）与改造前完全一致。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/code` | 发送登录/注册验证码；开发环境未配置 SES 时返回 `devCode` |
| POST | `/api/auth/verify` | 验证码登录；邮箱不存在且注册开放时自动创建无密码账号 |
| POST | `/api/auth/login` | 密码登录（仅设置过密码的账号） |
| POST | `/api/auth/password` | 设置/修改密码；已有密码时需当前密码 |
| DELETE | `/api/auth/password` | 移除密码（需当前密码） |
| POST | `/api/auth/logout` | 注销当前会话 |
| GET | `/api/auth/me` | 当前用户（含 `hasPassword`）；未登录返回 `user: null` |
| GET | `/api/account` | 账户页摘要（当前用户 + 未认领的旧文件数） |
| POST | `/api/auth/email/current-code` | 发送改邮箱验证码到**当前邮箱**（需登录，用于身份确认） |
| POST | `/api/auth/email/code` | 发送改邮箱验证码到**新邮箱**（需登录） |
| POST | `/api/auth/email` | 修改邮箱（需当前邮箱 + 新邮箱两个验证码） |
| GET | `/api/timeline?cursor=&limit=` | 读时间线：扁平事件流，按时间倒序 keyset 分页，含预签名缩略图 |
| DELETE | `/api/events/{id}` | 删除一条事件（墓碑，防复活） |
| GET | `/api/timeline/status` | 整理进度（未完成任务数、已整理文件数、最早/最新月份） |
| POST | `/api/timeline/rebuild` | 强制重新整理当前账号（删掉分析结果后从头回填） |
| GET | `/api/fs/list?path=` | 列当前层（查 Postgres，不查桶） |
| GET | `/api/fs/search?q=` | 按名称/路径模糊搜索 |
| POST | `/api/fs/upload/sign` | 拿 STS 临时凭证，浏览器直传 |
| POST | `/api/fs/upload/confirm` | HeadObject 校验后落库 |
| GET | `/api/fs/download?path=` | 302 到预签名 URL（`&json=1` 返回 JSON） |
| POST | `/api/fs/mkdir` | 建目录（写零字节占位对象） |
| POST | `/api/fs/rename` | 单文件改名/移动；目录请用 coscli |
| POST | `/api/fs/delete` | 永久删除（子树上所有记录） |
| GET | `/api/health` | 存活探针 |

认证写接口会校验 `Origin`/`Host`，并做了单进程内存限流（验证码发送 30 次/小时/IP、
5 次/小时/邮箱、同一邮箱 60 秒冷却；验证码校验 30 次/15 分钟/IP；
密码登录 30 次/15 分钟/IP、10 次/15 分钟/邮箱）。

## 环境变量

| 变量 | 说明 |
|---|---|
| `DATABASE_URL` | Postgres 连接串 |
| `COS_SECRET_ID` / `COS_SECRET_KEY` | 服务端密钥，不要给前端 |
| `COS_BUCKET` / `COS_REGION` | 桶必须带 APPID |
| `APP_ENV` | `development`（默认）或 `production`；决定 Secure cookie 和未配 SES 时的行为 |
| `SESSION_TTL_DAYS` | 登录态有效期，默认 30 天 |
| `TIMELINE_ENABLED` | 是否启用事件的后台整理（把照片聚成事件），默认 `true`；设为 `false` 只保留读接口 |
| `TIMELINE_WORKERS` | 后台整理并发数，默认 2 |
| `TIMELINE_MONTH_BUDGET` | 一个月最多占多少展示单位（1 单位 ≈ 72px），默认 24 |
| `TIMELINE_TZ` | 把事件时间归到哪一天，默认 `Asia/Shanghai` |
| `COOKIE_SECURE` | 不设置时生产环境自动加 Secure；本地 http 调试可设 false |
| `ALLOW_REGISTRATION` | 设为 false 关闭注册；默认开放 |
| `ADDR` / `PORT` | 监听地址；默认 `:8080`（`ADDR` 优先） |
| `WEB_DIR` | 前端构建产物目录；不填时自动找仓库里的 `web/dist` |
| `USER_QUOTA_BYTES` | 每个账号的存储上限（字节），0 或留空表示不限 |
| `UPLOAD_MULTIPART_THRESHOLD` | 超过多少字节走分片上传，默认 5 GiB |
| `UPLOAD_PART_SIZE` | 分片大小，默认 64 MiB（片数超过 1000 会自动放大） |
| `SES_REGION` | 腾讯云 SES 区域，默认 `ap-guangzhou` |
| `SES_FROM_EMAIL_ADDRESS` | 已在 SES 验证的发件地址；可带显示名，如 `Everything About Me <noreply@yourdomain.com>` |
| `SES_TEMPLATE_ID` | 验证码模板 ID |
| `SES_TEMPLATE_VAR` | 模板变量名，默认 `code` |
| `SES_SUBJECT` | 验证码邮件标题 |
| `SES_REPLY_TO_ADDRESS` | 可选回复地址 |
| `SES_SECRET_ID` / `SES_SECRET_KEY` | 可选；不填时复用 COS 密钥 |

## 运维命令

后端所有维护脚本都收在一个二进制里（`make` 只是快捷方式）：

```bash
cd backend && go run ./cmd/eam --help          # 或 ./bin/eam --help
go run ./cmd/eam migrate                       # 建表 / 升级
go run ./cmd/eam doctor [--write]              # 自检（--write 会写一个探针对象再删掉）
go run ./cmd/eam reconcile --owner … [--inventory …] [--dry-run]
go run ./cmd/eam claim --email … [--dry-run] [--keep-source]
go run ./cmd/eam token create --email … --name … [--prefix …] [--write=false]
go run ./cmd/eam token list   --email …
go run ./cmd/eam token revoke --email … --id …
```

`make` 里也有对应目标：

| 目标 | 作用 |
|---|---|
| `make install` | 首次安装依赖（Go modules + 前端 npm） |
| `make dev` | 同时起 Go 后端和 Vite |
| `make build` | 构建前端 + 编译 `bin/eam` |
| `make migrate` / `make check` / `make check-write` | 对应 `eam migrate` / `eam doctor` |
| `make test` | Go 测试 + 前端类型检查 |

## 已知取舍

- **SES 未配置时生产环境不能验证码登录/注册**：开发环境走日志验证码；上线前必须配好 SES 和模板。已经设置密码的账号仍然可以用密码登录。
- **修改邮箱需要当前邮箱 + 新邮箱两个验证码**：只拿到 session 改不了邮箱；旧邮箱不会收到变更通知（后续可以补一封通知邮件）。
- **密码是可选的**：设置后可用密码登录；没有密码的账号完全依赖邮箱可达性。
- **忘记密码不致命**：可以用邮箱验证码登录后在账户页重设；但邮箱本身丢了就没有恢复渠道（没有备用邮箱/恢复码）。
- **限流是单进程内存的**：多实例部署时每个实例各算各的，严格限流要换 Redis。
- **目录重命名**要逐个对象做服务端复制，TB 级会很久。先用 coscli 改，再 reconcile。
- **秒传**没做：TB 级算全量 SHA256 等于读一遍数据。要做的话用 `size + 首尾 1MB hash`。
- **旧数据要手动认领**：`eam claim` 是 O(对象数) 的服务端复制；跑之前先 `--dry-run`。
- **删除账号**只级联数据库记录；COS 里 `drive/<用户ID>/` 的对象需要按前缀自行清理。
- **前端是纯 SPA**：首屏要等 JS 加载完（约 110KB gzip，COS SDK 按需再加载）；没有 SSR，但也不需要在服务端渲染用户数据。
- **自动事件目前是启发式版本**：只看索引里的路径/大小/类型/时间，没读 EXIF/GPS/画面内容；分析、分组、评分、命名都是接口（`internal/timeline` 里的 `Analyzer` / `Grouper` / `Scorer` / `Narrator`），接多模态或大模型时替换实现即可。健康 CSV 是内置的一个 `AmbientExtractor`。
- **事件的结构见 [`docs/design/events.md`](docs/design/events.md)**：一条事件 = 时间 + 一张卡（`photo` / `note` / `metric` / `html`）+ 可选详情链接；三方应用用带 `can_events` 的 token 通过 `POST /events` 投稿。
- **旧库要补文件时间**：`nodes.source_modified_at` 是这次新加的，老数据需要跑一次 `reconcile` 才会填上；没填之前时间线会退回索引更新时间。
- **"每日视频"靠启发式识别**：同一目录、跨 ≥7 天、≥7 个视频会被合成一张系列卡；判断不准时可以后续做手动覆盖，或换成模型。
- **移动/复制目录不会立刻刷新时间线**：后台 sweeper 每 20 分钟扫一次最近变更；急着看可以用 `POST /api/timeline/rebuild`。
- **事件还没有站内编辑器**：现在只有自动（照片会话）和三方投稿两种来源；手写事件、健康变化卡、合并都还没做。
- **升级到事件模型要跑一次迁移**：`sql/013_events.sql` 会建 `events` / `event_media`，并把已有月快照标记为过期；后台会在下一次 sweeper（或 `POST /api/timeline/rebuild`）把每个月的片段重算成事件。迁移之前时间线是空的。
- **`cos-js-sdk-v5` 的 XML 解析依赖有审计告警**：官方 SDK 固定了 `fast-xml-parser@4.5.0`，`npm audit` 会报 critical。它只解析 COS 返回的 XML（走 TLS），目前没有无损升级路径；等官方 SDK 升级后再跟进。
