# AGENTS.md

给在本仓库里工作的 AI agent / 新同事的动手须知。完整说明在 [README.md](README.md)，
接口设计在 [docs/api/api.md](docs/api/api.md)，事件模型在 [docs/design/events.md](docs/design/events.md)。
本文件只写“动手前必须知道的事”，不重复 README。

## 一句话

我个人数据基座：**邮箱账号 + 文件层 + 一条自动整理的内容流**。账号、目录树、属性、事件在
Postgres，文件字节在腾讯云 COS。Go 后端（标准库 `net/http`）+ React SPA（Vite + React Router），
前后端分离，没有 Next.js。

## 跑起来

前置：Go 1.26+、Node 20.19+、Postgres；本地开发还需要一个 COS 桶和腾讯云子账号密钥。

```bash
cp .env.example .env    # 填 DATABASE_URL / COS_*；.env 不要提交
make install            # go mod download + 前端 npm install
make migrate            # 按文件名顺序执行 sql/*.sql
make dev                # 后端 :8080 + Vite :5173（/api 已代理）
```

常用命令：

| 命令 | 作用 | 需要外部依赖 |
|---|---|---|
| `make dev` | Go 后端 + Vite 一起跑 | Postgres + COS |
| `make server` | 只起 Go（有 `web/dist` 时顺带托管前端） | 同上 |
| `make build` | 构建 `web/dist` 再编译 `bin/eam` | 无（需 node_modules） |
| `make test` | `go test ./...` + `tsc --noEmit` | 无 |
| `make fmt` | `gofmt -w` 后端 | 无 |
| `make migrate` | 建表 / 升级 | Postgres |
| `make check` / `make check-write` | `eam doctor` 自检（--write 会写探针对象再删） | Postgres + COS（+ SES） |

- 后端所有维护命令都收在一个二进制里：`cd backend && go run ./cmd/eam <cmd>`（或 `./bin/eam`）。
- `.env` 由 `internal/config` 自动从当前目录往上找三层加载，不用手动 export；Go 本身不会读 `.env`。
- 开发环境没配 SES 时，验证码会打印在后端日志并自动填进登录页——**不要把开发服务器暴露到公网**。

## 仓库地图

| 路径 | 内容 |
|---|---|
| `backend/cmd/eam/` | 唯一入口：`server` / `migrate` / `doctor` / `reconcile` / `claim` / `token` |
| `backend/internal/api/` | 路由与 handler：网页 `/api/*`、对外 API、`/events` |
| `backend/internal/nodes/` | 文件索引表 `nodes` 的读写；只存元数据，不存字节 |
| `backend/internal/storage/` | 对象存储接口；实现是 `internal/cos/`（腾讯云） |
| `backend/internal/timeline/` + `events/` | 内容流：分析 → 分组 → 评分 → 叙述，落成 `events` |
| `backend/internal/auth/` `emailcode/` `mail/` | 验证码、密码、会话、API token、SES |
| `backend/internal/jobs/` | 目录 move / copy 的异步任务 |
| `sql/` | 只进不退的迁移，按文件名顺序执行 |
| `web/src/` | React 19 + Vite + React Router 8；样式是 CSS Modules |
| `docs/` | 接口与设计文档；改语义前先读这里 |

## 后端约定

- 标准库 `net/http` + Go 1.22 方法路由（`mux.HandleFunc("GET /api/...")`），**不要引入 Web 框架**。
- Handler 统一 `func(http.ResponseWriter, *http.Request) error`，用 `s.wrap`（网页）或 `s.api`（对外 API）包装；
  业务错误用 `httpx.Error`（`httpx.BadRequest(...)` 等），会被映射成状态码 + `{ok:false,error}`。
- 响应信封固定：`{ok:true,data}` / `{ok:false,error}`（`internal/httpx`）。
- **注释和文档用中文；日志、CLI 输出、API 错误文案、测试失败信息用英文。** 界面文案走前端 i18n。
- 路径就是身份：改动只认逻辑路径（NFC 归一化），对外 API 不暴露 node id；COS key 是
  `drive/<用户ID>/<path>`。不要绕过 `internal/paths` 自己拼 key。
- 多用户隔离靠 `owner_id`（`users.id`，UUID）——新表 / 新查询必须带用户维度。
- 依赖在 `api.New` 里组装；新服务先定义接口（参考 `storage.Storage`、`timeline.Analyzer`），不要在 handler 里 new 客户端。
- **删除是永久的**（没有回收站）；目录删除必须显式 `?recursive=1`，不要在代码里替用户“顺手递归”。
- 两套鉴权不要混：对外 API（`/list`、`/files/...`、`/move`、`/upload` 等）用 Bearer token；
  `/api/*` 只给网页，用 httpOnly cookie `eam_session`。
- 长操作（move / copy）统一走 `Prefer: respond-async`，返回 `202 + Location`；不要新增一套“任务”概念。
- 限流是单进程内存的（`internal/ratelimit`），多实例部署会各算各的；知道这个前提再改。
- 时间线后台整理器在服务启动后单独 `StartTimeline(ctx)` 启动；sweeper 约每 20 分钟扫一次增量，
  `POST /api/timeline/rebuild` 是手动全量重建。

## 数据库 / 迁移

- 新迁移命名 `sql/0NN_xxx.sql`，数字递增、一个文件一个主题；文件头写清楚改了什么、为什么。
- **没有版本表，`make migrate` 每次都重跑全部文件**，所以：
  - 用 `create table if not exists`、`add column if not exists`；
  - 破坏性 DDL 要加保护或单独的过渡步骤（参考 `sql/010_drop_soft_delete.sql`）；
  - 数据回填写在同一个文件里，并且可重复执行。
- `nodes` 是索引不是副本：**永远不要把文件字节写进数据库**；`path` / `key` 的映射只在 `internal/nodes`、`internal/paths`。
- 老数据的 `source_modified_at` 要跑过 `reconcile` 才完整；没填之前时间线会退回 `updated_at`。

## 前端约定

- 纯 SPA，没有 SSR。路由是 React Router data router（`router.tsx`），页面数据在 `loader` 取，未登录 `throw redirect("/login")`。
- 所有请求走 `src/api/client.ts`（唯一出口），它负责信封解包和 `ApiError`；不要在页面里直接 `fetch`。
  例外是拿到 STS 后浏览器直传 COS（`cos-js-sdk-v5`，按需加载）。
- 样式：全局 token 在 `src/styles/globals.css`（暖白纸感 + 蓝色强调 + `prefers-color-scheme` 自动深浅色），
  页面布局用 `*.module.css`；**不引入 Tailwind / CSS-in-JS**。
- i18n：模块加载时按浏览器语言二选一（zh / en），没有切换入口。新增文案必须同时补 `messages.ts` 的 en 和 zh，
  漏 key 会在 `tsc` 报错；不要硬编码界面中文。
- 字体、间距、圆角、阴影从 `globals.css` 的变量取；`--danger` 只给不可逆操作。
- 保持首屏体积：大依赖按需动态 import。

## 改完之后

1. `make test` 必须通过（Go 测试 + 前端 typecheck）；只改前端也跑。
2. Go 代码 `gofmt`（或 `make fmt`）；TS 保持现有风格（2 空格、双引号、分号）。
3. 改了接口 / 字段：同步 `docs/api/api.md`、README 的接口表，以及 `web/src/api/client.ts` 的类型。
4. 改了表结构：追加 `sql/` 迁移，并确认 `make migrate` 可以重复执行。
5. 新增界面文案：en / zh 两份都补。
6. 提交信息用 Conventional Commits + 中文摘要，例如 `fix(cos): STS 策略资源按段编码，修文件名带 + 时直传 403`。

## Git 工作流

原则：**主工作区只保留 `main`，所有改动在 worktree 的任务分支上做，rebase 后线性合回 `main`。**
`main` 不能同时在两个 worktree 里 checkout，所以“直接提交到 main”落地为：
任务分支提交 → rebase 到最新 main → 在主工作区 `merge --ff-only` →（有 remote 时）push。
除用户明确说“直接改主工作区”外，所有改动都走 worktree。

### 1. 开 worktree

```bash
git fetch origin                                    # 有 remote 时；没有就跳过
git worktree add .worktrees/<type>-<slug> -b <type>/<slug> main
```

- 目录固定在仓库内的 `.worktrees/`（已加入 `.gitignore`）；不要放到仓库外，沙箱只允许写仓库内。
- 分支名 `<type>/<slug>`，type 用 `feat` / `fix` / `chore` / `docs`；重名时加日期或随机后缀。
- 一个任务一个 worktree；并行任务各开各的，不要复用或改动别人的分支。
- 新 worktree 里没有 `node_modules` 和 `.env`（都在 ignore 里），在 worktree 根目录补链接：

  ```bash
  ln -s ../../../web/node_modules web/node_modules   # make test 的 tsc 需要
  ln -s ../../.env .env                              # 只有要起服务/连库时才需要
  ```

  `.gitignore` 用的是 `node_modules`（无斜杠），所以这个软链不会出现在 `git status` 里。
- 沙箱里默认的 Go 缓存在 `~/Library/Caches/go-build`（仓库外，可能不可写）；在 worktree 根目录用仓库内的：

  ```bash
  GOCACHE=$PWD/../../.gocache make test
  ```

- 主工作区里用户未提交的改动**不属于这个任务**：不要 stash、不要 reset、不要带进 worktree。
  任务要改同一批文件时先问用户。

### 2. 做、测、提交

```bash
GOCACHE=$PWD/../../.gocache make test   # 必须通过；只改前端也要跑
git status                # 确认只有自己改的文件
git add <具体文件>
git commit -m "feat(scope): 中文摘要"
```

- 一个逻辑变更一个 commit；信息用 Conventional Commits + 中文摘要，正文写“为什么”。
- 不要 `git add -A` / `git commit -a`：主工作区可能有用户自己的改动。
- 测试不过就修；修不动就停下来说明，不要把红色状态提交到 main。
- **有开放问题时不要提交**（需求不清、需要用户决策、测试失败待定）：保留 worktree + 分支，
  在最后的消息里写清楚停在哪、需要什么。
- 用户说“先不提交 / 先看 diff”时照做。

### 3. 合回 main

```bash
git fetch origin                # 有 remote 时
git rebase main                 # 有 remote 时用 origin/main
# 冲突：逐个解决 → git add → git rebase --continue → 重新 make test

git -C <主工作区> merge --ff-only <type>/<slug>
git -C <主工作区> push origin main          # 有 remote 时
```

- 合并必须在主工作区做（main 在那里 checkout），`--ff-only` 保证历史线性；不要产生 merge commit。
- 主工作区的未提交改动挡住 merge 时：先问用户，不要强来，也不要在脏工作区上 `pull --rebase`。
- **永远不要** `push --force` 到 main，也不要 `reset --hard` / `clean -fd` 主工作区。
- 有 remote 且 push 被拒（别人先推了）：`git -C <主工作区> pull --rebase origin main` →
  重跑 `make test` → 再 push；循环仍失败就停下来报告。
- 当前仓库还没有 remote：只做本地合并，最后的消息写“本地提交，未推送”。加了 origin 后按上面的 fetch/push 走。

### 4. 清理

只在两种情况清理：已合并进 main，或用户明确说放弃这次改动。

```bash
# 先删掉第 1 步补的链接，清理更干净
rm -f .worktrees/<type>-<slug>/web/node_modules .worktrees/<type>-<slug>/.env

git -C <主工作区> worktree remove .worktrees/<type>-<slug>
git -C <主工作区> branch -d <type>/<slug>    # 未合并时会被拒绝，这是保护，别急着 -D
git -C <主工作区> worktree prune
git -C <主工作区> worktree list              # 确认只剩主工作区
```

- 已合并 → worktree 和分支都删；明确放弃 → 删 worktree，**保留分支**并说明（避免丢工作）。
- **还在等用户回答的任务不要清理**，否则下次要从头再来；在消息里给出 worktree 路径。
- 确实要强删（比如链接已删但仍有残留）时，确认没有未提交改动再用 `worktree remove --force`；
  不要对主工作区用 `--force`。

### 5. 最后一条消息的格式

提交/清理情况放在消息**最末尾**，一行说完：

```text
已提交 main@abc1234（已推送）· 3 个文件 · worktree 已清理
```

变体：`本地提交 main@abc1234，未推送` / `未提交（测试失败：xxx）` /
`未提交（等你确认：xxx）` / `已合并 main@abc1234，worktree 保留在 .worktrees/xxx（等你确认）`。
没有改动也要写 `无改动，未提交`。

## 不要做

- 不要提交 `.env`、密钥、`bin/`、`.gocache/`、`web/dist/`（已在 `.gitignore`）。
- 不要让服务端中转文件字节：下载用 302 预签名，上传用 STS 直传 / 分片。
- 不要删除或改写已有迁移文件，只能追加。
- 不要改 `web/vite.config.ts` 的 5173 端口或 `/api` 代理——COS CORS 和开发流程都依赖它。
- 不要把两套鉴权混在同一个路由上（`/api/*` 加 Bearer，或对外 API 加 cookie）。

## 已知坑（踩过）

- `cos-js-sdk-v5` 固定了有审计告警的 `fast-xml-parser`，`npm audit` 报 critical，目前无升级路径。
- STS 会先把策略里的资源路径解码再比对，`+` 会被当空格；`internal/cos` 已按路径段编码，
  改签名 / 策略时别回退（否则文件名带 `+` 直传 403）。
- macOS 文件名是 NFD，不归一化会出现“同名两个文件”；`paths.Normalize` 负责转 NFC。
- 目录改名 / 移动是逐个对象服务端复制，TB 级很慢；大目录先用 coscli 再 `reconcile`。
- 修改邮箱需要**当前邮箱 + 新邮箱**两个验证码；只拿到 session 改不了。
- 移动 / 复制后时间线不会立刻刷新：等 sweeper 或手动 `POST /api/timeline/rebuild`。
- 从旧版本升级：`sql/013_events.sql` 会清空快照水位并排队重算（只在还没有任何 events 时执行，
  避免 `migrate` 每次重跑都把时间线重置）；后台处理完，时间线才会出现事件。
