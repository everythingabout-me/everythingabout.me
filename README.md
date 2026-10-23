# everything about me

个人数据基座：**邮箱注册的账号系统 + 文件层**。
账号、会话、目录树、属性、检索放 Postgres；文件字节放腾讯云 COS。

```
浏览器 ──登录态 cookie──▶ Next.js API ──▶ Postgres (users / sessions / nodes)
   │
   └──STS 临时凭证直传──▶ COS (drive/<用户ID>/…)
```

## 用户系统

- **邮箱验证码登录 / 注册合并**：输入邮箱 → 收验证码 → 已有账号直接登录，没有账号自动创建。没有用户名，默认也不需要密码。
- 每个账号有一个**唯一且不变的 `users.id`（UUID）**作为内部身份；用户不需要关心它，界面上邮箱就是唯一标识。改邮箱时 id 和文件归属都不变。
- 密码是**可选的**：账户页可以设置、修改或移除；设置后登录页多一个「用密码登录」的入口。密码同样用 Node 内置 `scrypt` 加盐哈希。
- 登录态是 httpOnly cookie（`eam_session`），数据库只存 token 的 SHA-256；默认 30 天过期。
- 文件按 `owner_id` 隔离，COS 里每个用户一个独立前缀 `drive/<用户ID>/`，不同账号可以有同名路径。
- `/account` 可以修改邮箱（需要**当前邮箱 + 新邮箱**两个验证码），也可以设置/移除密码；界面只显示邮箱和注册时间。
- 注册默认开放；个人实例注册完自己后，建议把 `ALLOW_REGISTRATION=false` 写进 `.env` 并重启。

## 为什么不是"只用 S3"

S3 只能按前缀列举、不能搜索、没有回收站、没有标签。这张 `nodes` 表就是补上这层：
它是**索引**，不是副本；对象字节永远只在 COS 里。

## 目录约定

| 前缀 | 用途 |
|---|---|
| `drive/<用户ID>/` | 用户可见文件，key 就是真实路径 |
| `_trash/` | 被同名新文件顶掉的旧对象 |
| `_inventory/` | COS 清单输出（可选） |

key 直接落在路径上（`drive/<用户ID>/Photos/2024/a.jpg`），意味着：

- 用 `coscli` / `rclone` 直接往桶里放文件，跑一遍 `reconcile --owner` 就出现在网站里；
- 哪天不用这个站了，`coscli sync` 拉下来就是完整的目录结构，不需要跑任何程序；
- 删除账号时数据库会级联清掉索引，但 COS 对象需要自己按前缀清理。

## 界面

- 登录 / 注册（合并为一个页面）/ 账户三个页面，跟随系统深浅色
- 单页文件浏览器：面包屑、相对时间、类型图标、悬停操作
- 拖拽上传 + COS 直传进度提示；`/` 聚焦搜索，`Esc` 关闭弹层
- 搜索会同时匹配文件名和完整路径，结果右侧显示所在目录
- 新建目录是行内输入框，删除有确认弹窗（进回收站）
- 顶栏右侧显示当前账号，点进 `/account` 改邮箱、设置密码或退出

## 起步

```bash
cp .env.example .env      # 填 DATABASE_URL / COS_*
npm run db:init           # 建表：nodes / users / sessions / email_codes
npm run check             # 自检：数据库 / COS 鉴权 / STS 签发 / SES
npm run dev
```

打开 http://127.0.0.1:3000 ，输入邮箱收验证码；第一次验证会自动创建账号。
开发环境没配 SES 时，验证码会打印在 `npm run dev` 的终端里，也会自动填入登录页。

用 `127.0.0.1` 或局域网 IP 访问 dev server 时，Next 16 默认会拦掉 `/_next/hmr`，
表现为页面一直停在骨架屏。`next.config.ts` 里已经加了 `allowedDevOrigins`。

### 登录 / 注册验证码（腾讯云 SES）

登录、注册、改邮箱共用 6 位邮箱验证码。配好 SES 就真实发信；**没配 SES 时**，开发环境会把验证码打印到服务端日志并在登录页自动填入，生产环境会直接拒绝发送。

> 开发模式的验证码会直接返回给浏览器，所以不要把 `npm run dev` 暴露到公网。

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
npm run claim -- --email you@example.com --dry-run   # 先看会把哪些对象搬到哪里
npm run claim -- --email you@example.com             # 正式认领
```

它会把 COS 对象从 `drive/<路径>` 复制到 `drive/<用户ID>/<路径>`，改写数据库里的 key 和
`owner_id`，然后删掉旧对象（`--keep-source` 可保留）。数据量大时可以先用 coscli 分目录搬，
再跑 `npm run reconcile -- --owner you@example.com`。

### COS 侧要配三样

1. **CORS**（允许浏览器直传）
   - 来源：`http://localhost:3000`、你的正式域名
   - 方法：`PUT, GET, POST, HEAD`
   - 头部：`*`，暴露头部：`ETag, x-cos-request-id`
2. **子账号 + STS 权限**：主账号密钥只放在服务端，`qcloud-cos-sts` 会按单个对象的 key 下发临时凭证（30–60 分钟）。
3. **（可选）数据万象**：开了之后图片可以走 `?imageMogr2/thumbnail/300x` 出缩略图，不用自己跑 Sharp。

## 首次灌 TB 级数据

别走网页上传。用官方 CLI 直接进桶：

```bash
coscli cp -r ~/Pictures cos://your-bucket-1250000000/drive/<用户ID>/Pictures
npm run reconcile -- --owner you@example.com --dry-run   # 先看会写多少条
npm run reconcile -- --owner you@example.com
```

百万对象时改用 COS **清单（Inventory）**，避免全量 List 的请求费：

```bash
npm run reconcile -- --owner you@example.com --inventory ./inventory.csv
```

`reconcile` 做三件事：补新增、更新变动的（size/etag）、把桶里已消失的标成删除。
它只扫描指定用户的前缀，不会碰别的账号；也不删任何对象——删除权只在你手里。

## 回收站

删除是软删除：`deleted_at` 打标记，对象留在原地。

```bash
npm run purge -- --days 30 --dry-run
npm run purge -- --days 30
```

## 接口

所有 `/api/fs/*` 都要求登录，并且只操作当前账号自己的文件（未登录返回 401）。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/auth/code` | 发送登录/注册验证码；开发环境未配置 SES 时返回 `devCode` |
| POST | `/api/auth/verify` | 验证码登录；邮箱不存在且注册开放时自动创建无密码账号 |
| POST | `/api/auth/login` | 密码登录（仅设置过密码的账号） |
| POST | `/api/auth/password` | 设置/修改密码；已有密码时需当前密码 |
| DELETE | `/api/auth/password` | 移除密码（需当前密码） |
| POST | `/api/auth/logout` | 注销当前会话 |
| GET | `/api/auth/me` | 当前用户（含 `hasPassword`）；未登录返回 `user: null` |
| POST | `/api/auth/email/current-code` | 发送改邮箱验证码到**当前邮箱**（需登录，用于身份确认） |
| POST | `/api/auth/email/code` | 发送改邮箱验证码到**新邮箱**（需登录） |
| POST | `/api/auth/email` | 修改邮箱（需当前邮箱 + 新邮箱两个验证码） |
| GET | `/api/fs/list?path=` | 列当前层（查 Postgres，不查桶） |
| GET | `/api/fs/search?q=` | 按名称/路径模糊搜索 |
| POST | `/api/fs/upload/sign` | 拿 STS 临时凭证，浏览器直传 |
| POST | `/api/fs/upload/confirm` | HeadObject 校验后落库 |
| GET | `/api/fs/download?path=` | 302 到预签名 URL（`&json=1` 返回 JSON） |
| POST | `/api/fs/mkdir` | 建目录（写零字节占位对象） |
| POST | `/api/fs/rename` | 单文件改名/移动；目录请用 coscli |
| POST | `/api/fs/delete` | 软删除（子树上所有记录） |

认证写接口会校验 `Origin`/`Host`，并做了单进程内存限流（验证码发送 30 次/小时/IP、
5 次/小时/邮箱、同一邮箱 60 秒冷却；验证码校验 30 次/15 分钟/IP；
密码登录 30 次/15 分钟/IP、10 次/15 分钟/邮箱）。

## 环境变量

| 变量 | 说明 |
|---|---|
| `DATABASE_URL` | Postgres 连接串 |
| `COS_SECRET_ID` / `COS_SECRET_KEY` | 服务端密钥，不要给前端 |
| `COS_BUCKET` / `COS_REGION` | 桶必须带 APPID |
| `SESSION_TTL_DAYS` | 登录态有效期，默认 30 天 |
| `COOKIE_SECURE` | 不设置时生产环境自动加 Secure；本地 http 调试可设 false |
| `ALLOW_REGISTRATION` | 设为 false 关闭注册；默认开放 |
| `SES_REGION` | 腾讯云 SES 区域，默认 `ap-guangzhou` |
| `SES_FROM_EMAIL_ADDRESS` | 已在 SES 验证的发件地址；可带显示名，如 `Everything About Me <noreply@yourdomain.com>` |
| `SES_TEMPLATE_ID` | 验证码模板 ID |
| `SES_TEMPLATE_VAR` | 模板变量名，默认 `code` |
| `SES_SUBJECT` | 验证码邮件标题 |
| `SES_REPLY_TO_ADDRESS` | 可选回复地址 |
| `SES_SECRET_ID` / `SES_SECRET_KEY` | 可选；不填时复用 COS 密钥 |

## 已知取舍

- **SES 未配置时生产环境不能验证码登录/注册**：开发环境走日志验证码；上线前必须配好 SES 和模板。已经设置密码的账号仍然可以用密码登录。
- **修改邮箱需要当前邮箱 + 新邮箱两个验证码**：只拿到 session 改不了邮箱；旧邮箱不会收到变更通知（后续可以补一封通知邮件）。
- **密码是可选的**：设置后可用密码登录；没有密码的账号完全依赖邮箱可达性。
- **忘记密码不致命**：可以用邮箱验证码登录后在账户页重设；但邮箱本身丢了就没有恢复渠道（没有备用邮箱/恢复码）。
- **限流是单进程内存的**：多实例部署时每个实例各算各的，严格限流要换 Redis。
- **目录重命名**要逐个对象做服务端复制，TB 级会很久。先用 coscli 改，再 reconcile。
- **秒传**没做：TB 级算全量 SHA256 等于读一遍数据。要做的话用 `size + 首尾 1MB hash`。
- **旧数据要手动认领**：`npm run claim` 是 O(对象数) 的服务端复制；跑之前先 `--dry-run`。
- **删除账号**只级联数据库记录；COS 里 `drive/<用户ID>/` 的对象需要按前缀自行清理。
