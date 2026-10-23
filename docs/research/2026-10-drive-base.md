# 个人数据平台：网盘基座选型调研

> 调研时间：2026-10　｜　目标：以网盘为基座，长期叠加自定义功能，承载"我的全部个人数据"

## 0. 一句话结论

没有"最全能"的网盘，只有两种基座形态：

- **产品型基座**（Cloudreve / Nextcloud / Seafile）：拿现成网盘做文件层，额外功能外挂。
- **自建薄基座**（S3 + Postgres + 自己的 API）：自由但工作量大。

个人项目默认推荐 **Cloudreve 4 做文件基座 + 自己的服务做业务层**；
若要"全平台双向同步 + 照片/日历/联系人全家桶 + 裸文件可迁移"，选 **Nextcloud**。

---

## 1. "网盘当基座"其实要选三层

| 层 | 职责 | 可选 |
|---|---|---|
| 存储层 | 字节落在哪 | 云 OSS/S3、本地磁盘、SeaweedFS / Garage / RustFS |
| 文件层 | 目录、版本、分享、权限、同步、检索 | Cloudreve / Nextcloud / Seafile / Pydio |
| 业务层 | 你的"额外功能" | 自己写（推荐独立服务，不要塞进网盘） |

**最容易踩的坑**：把业务逻辑塞进网盘。网盘负责"文件 + 权限 + 分享"，结构化数据（账本、健康、地点、人脉、时间线）应该在自己的 Postgres 里，用文件 URI 关联。

---

## 2. 评估维度（针对"基座 + 长期扩展"重新定义）

普通网盘评测比的是"好不好用"；基座要比的是：

1. **扩展点**：API 是否完备、有没有事件、能不能换前端、能不能写插件
2. **元数据与检索**：后续所有功能都要靠"给文件挂属性 + 查属性"
3. **文件落地形态**：裸文件（可 rsync / 随时跑路）还是 blob（依赖 DB 解释）
4. **客户端覆盖**：Web / 桌面同步 / 手机 / WebDAV
5. **运维成本**：几个进程、几个中间件、升级频率
6. **许可与商业风险**：Pro 门槛、公司是否稳定、前端源码授权

---

## 3. 候选对比（2026-10 实测数据）

| 项目 | 版本 | 技术栈 | 文件落地 | 扩展方式 | 元数据/检索 | 客户端 | 许可 | 运维 |
|---|---|---|---|---|---|---|---|---|
| **Cloudreve** | 4.19.1 | Go + React | blob（S3/本地/OneDrive/远程节点），原名不存 | REST `/api/v4` + SSE 事件 + WebDAV + WOPI + 可替换前端 | 元数据命名空间 `key:value`，可直接作为查询条件；Meilisearch 全文 + Tika 正文抽取 + AI 语义检索 | Web、iOS、Windows（Pro）、WebDAV | GPL-3.0 + Pro 付费 | ★ 单二进制 + DB（+可选 Redis/Meilisearch） |
| **Nextcloud** | 35.0.2 | PHP + Vue | **裸文件** `data/<user>/files` | PHP App + **AppAPI ExApp（后端可用任意语言，容器化微服务）** | 标签/评论/版本；全文检索靠 fulltextsearch + ES 插件 | 全平台官方客户端（Win/mac/Linux/iOS/Android）+ WebDAV | AGPL | ★★★ PHP-FPM + nginx + DB + Redis + cron |
| **Seafile** | 13.0（14 测试中） | C + Go | 块存储（库/commit 模型），加密库 | REST API + WebDAV，插件生态弱 | 13 起有"文件属性 + 视图"；全文检索偏 Pro | 全平台，**同步最强**（块级增量、断点续传、冲突副本） | AGPL / 商业双版 | ★★ 需 seafile + seahub + DB + 缓存 |
| **Pydio Cells** | 5.1.0 | Go 微服务 + gRPC | 本地/S3 抽象 | REST / gRPC / 多语言 SDK | 有 | Web + 同步客户端 | AGPL | ★★★ 微服务多进程，偏企业 |
| **FileBrowser Quantum** | 活跃 | Go 单二进制 | 直接映射目录（裸文件） | 基本无扩展点 | 索引搜索 | Web | Apache-2.0 | ★ 极轻 |
| **OpenList**（AList 继任） | 活跃 | Go | 虚拟 FS，聚合 30+ 云盘 | API / WebDAV | 继承云盘元数据 | Web + WebDAV | AGPL | ★★ |

社区热度参考：Nextcloud ~37.2k★，SeaweedFS ~35.7k★，Cloudreve ~28.9k★，OpenList ~25.2k★，Seafile ~15.4k★，FileBrowser Quantum ~8.9k★，Pydio Cells ~2.2k★。

---

## 4. 逐个点评

### Cloudreve 4 —— "API 优先"的现代网盘，最适合自己写上层

**强项**
- 数据模型干净：`File` / `File Blob`（不可变）/ `Metadata` / `Storage Policy`，文件有版本、缩略图、Live Photo；
- **元数据即查询条件**：`meta_music:album=Deluxe`、`exact_meta_*`，还能自定义 `props:<key>`——等于白送一个"给文件挂结构化属性"的能力；
- 全文检索（Meilisearch + Apache Tika 抽 PDF/Office 正文）+ **AI 语义检索**（embedding 打给 OpenAI/HF）；
- File Apps：内置 PDF/ePub/视频/音乐/Markdown，可加 WOPI（OnlyOffice）和嵌入式网页应用；
- 有 SSE 文件变更事件、OIDC 登录、从节点、存储策略矩阵（S3/OSS/COS/OneDrive/R2/B2/本地）；
- 部署就是一个二进制 + 一个数据库，运维成本比 Nextcloud 低一个数量级；
- 前端是独立 React 仓库，可直接换掉（Pro 用户可拿前端源码）。

**硬伤**
- 文件在存储桶里是 **blob（哈希路径），不是原文件名**；脱离 Cloudreve 后需要 DB 才能还原目录结构 → 备份必须 DB + bucket 同时快照；
- 官方桌面同步客户端 **只有 Windows，且需要 Pro**；Mac/Linux 只能 WebDAV 或 rclone；
- 无后端插件系统，扩展只能"走 API"；
- 社区版前端仓库没有 LICENSE 文件，深度改 UI 前要先确认授权；
- 多存储策略自由切换、WebDAV 分目录策略等落在 Pro。

### Nextcloud 35 —— 全家桶 + 裸文件 + 全平台客户端，但重

**强项**
- **文件就是普通文件**，落在 `data/<user>/files/`，rsync/restic 直接备份，随时能跑路——对"存几十年的个人数据"这是最大优势；
- 生态最大：Photos、Calendar、Contacts、Mail、Talk、Office、Notes、Deck……很多"额外功能"直接装；
- 扩展性其实不差：PHP App，以及 **AppAPI ExApp**——后端可以用 Python/Go/Node 写成容器微服务，前端 Vue，挂进 Nextcloud 菜单；
- 客户端覆盖无死角（Win/mac/Linux/iOS/Android），WebDAV 实现成熟。

**硬伤**
- 重：PHP-FPM + nginx + DB + Redis + cron，升级频繁，大版本升级偶有坑；
- 文件量大时（单目录数万文件）列表/扫描会慢，DB filecache 是瓶颈；
- 想改核心 = 改 PHP，心智负担大；AppAPI 需要 Docker 环境。

### Seafile 13/14 —— 同步王者，扩展性最弱

**强项**
- 块级增量同步、断点续传、冲突副本、加密库，多设备同步体验是这几个里最好的；
- 13 起有"文件属性 + 灵活视图"，往"文件即数据库"方向走了一步；
- 桌面/移动客户端成熟。

**硬伤**
- 数据是块存储 + commit 模型，**最封闭**，脱离 Seafile 需要专用导出工具；
- 扩展点是 REST/WebDAV，没有插件体系，想加功能基本只能外挂；
- 社区版与 Pro 差距明显（全文检索、在线 Office、高级权限在 Pro）。

### Pydio Cells 5 —— 架构先进，社区偏小

Go 微服务 + gRPC，有 Cells Flows 自动化，SDK 齐全；但多进程运维复杂、社区规模小，个人项目性价比不高。

### FileBrowser Quantum —— 适合"够用就好"

单二进制、映射真实目录、索引搜索，几分钟能跑起来。没有版本、没有像样的分享权限体系，只能当轻量文件浏览器，不是基座。

### OpenList —— 不是基座，是"入口"

AList 因商业纠纷被社区分叉为 OpenList（原版已不可信）。它把 30+ 云盘挂成统一虚拟文件系统 + WebDAV，非常适合"聚合外部云"，但**没有版本、没有一致性保证、不做主存储**。定位：并存的第二入口，不是底座。

---

## 5. 三种推荐架构

### A. Cloudreve 为核（推荐：能写代码、要 API、轻运维）

```
Web / iOS / WebDAV
        │
   Cloudreve 4  ── 文件·分享·权限·版本·元数据·全文/语义检索
        │                    ▲ REST + SSE
   Postgres / S3(或本地)      │
                       你的业务服务  ── 自己的 Postgres + pgvector
```

- 业务服务不改 Cloudreve 源码：读目录、拿临时直链、写 `props:*` 元数据、订阅 SSE 做自动处理；
- 额外功能（相册、时间线、AI 问答、自动归档）都是独立进程，坏了不影响网盘。

### B. Nextcloud 为核（推荐：要全家桶 + 多端同步 + 跑路能力）

```
Win/mac/Linux/iOS/Android 官方客户端
        │
   Nextcloud 35  ── Files/Photos/Calendar/Contacts + AppAPI ExApp
        │
   data/<user>/files  ←── WebDAV / OCS API ── 你的业务服务
```

- 文件保持裸文件，十年后不用 Nextcloud 也能读；
- 额外功能做成 ExApp（任意语言）或独立服务读 WebDAV。

### C. 自建薄基座（长期主义，工作量最大）

```
你的前端 → 你的 API → Postgres(元数据/ACL) + S3(RustFS/云 OSS)
                          ↑ 可选：FileBrowser 作为运维后门 UI（只读挂载）
```

- 只有你能定义数据模型，跑路能力最强；
- 注意：**同一个 bucket 只能有一个写者**。Cloudreve/Nextcloud 的 blob 命名与你自己的命名不能混写，否则必冲突。

---

## 6. 避坑清单

- **MinIO 的 GitHub 仓库已归档（2026-04）**，社区版事实上停更，新项目别选；S3 层用云 OSS、SeaweedFS、Garage，RustFS 很新（谨慎）。
- **AList 原版已不可信**（商业纠纷），用 OpenList 社区分支。
- **ownCloud 公司层面近两年动荡**（业务被收购/重组），oCIS 不建议作为新项目基座。
- **blob 类网盘（Cloudreve/Seafile）的备份必须 DB + 存储桶同一时点快照**，否则恢复出来是一堆无法解释的哈希。
- **自建 ≠ 备份**：至少 3-2-1，用 restic 推到另一家对象存储；别让自建盘成为唯一副本。
- **别把所有东西塞进一个 Seafile 库**：库过大后操作明显变慢，按主题分库。
- **Cloudreve 桌面同步仅 Windows + Pro**，如果你主力 Mac/iPhone 且要"像 Dropbox 一样同步"，这是否决项。

---

## 7. 一周验证计划（比看评测靠谱）

| 天 | 动作 |
|---|---|
| D1 | docker compose 起 `postgres + meilisearch + cloudreve`，跑通上传/分享 |
| D2 | 灌 5 万文件（脚本造目录树），测列表、搜索、缩略图、大文件断点续传 |
| D3 | iPhone 客户端 + rclone WebDAV 挂载 + 外链分享，记录体验断点 |
| D4 | Nextcloud AIO 同机另端口起一套，重复 D2/D3 对照 |
| D5 | 写 200 行脚本调 Cloudreve API：列目录 → 取直链 → 写 `props:*` → 订阅 SSE |
| D6 | 备份演练：`pg_dump` + bucket 快照 → 恢复到全新容器，验证文件与目录树完整 |
| D7 | 决策：按"同步体验 / 扩展顺手度 / 运维痛苦度"三项打分 |

---

## 8. 参考

- Cloudreve 文档（概念 / File URI / Metadata / File Apps / 语义检索）：https://docs.cloudreve.org/
- Cloudreve 源码：https://github.com/cloudreve/Cloudreve
- Nextcloud ExApp 开发：https://docs.nextcloud.com/server/32/developer_manual/exapp_development/development_overview/ExAppOverview.html
- Nextcloud Server：https://github.com/nextcloud/server
- Seafile 14 预览：https://blog.seafile.com/seafile-14-0-is-ready-for-testing-620a335e5668
- Seafile 13：https://blog.seafile.com/seafile-13-0-is-ready-470c5e75f6fc
- Pydio Cells：https://github.com/pydio/cells
- OpenList：https://github.com/OpenListTeam/OpenList
- FileBrowser Quantum：https://github.com/gtsteffaniak/filebrowser
- MinIO（已归档）：https://github.com/minio/minio
- RustFS：https://github.com/rustfs/rustfs
- SeaweedFS：https://github.com/seaweedfs/seaweedfs
