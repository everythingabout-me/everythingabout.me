# everything about me

个人数据基座的**文件层**：字节放腾讯云 COS，目录树 / 属性 / 检索放 Postgres。
没有 Auth、没有用户表——按约定，之后再加。

```
浏览器 ──STS 临时凭证直传──▶ COS (drive/…)
   │
   └──▶ Next.js API ──▶ Postgres nodes 表
```

## 为什么不是"只用 S3"

S3 只能按前缀列举、不能搜索、没有回收站、没有标签。这张 `nodes` 表就是补上这层：
它是**索引**，不是副本；对象字节永远只在 COS 里。

## 目录约定

| 前缀 | 用途 |
|---|---|
| `drive/` | 用户可见文件，对象的 key 就是真实路径 |
| `_trash/` | 被同名新文件顶掉的旧对象 |
| `_inventory/` | COS 清单输出（可选） |

key 直接用路径（`drive/Photos/2024/a.jpg`），意味着：

- 用 `coscli` / `rclone` 直接往桶里放文件，跑一遍 `reconcile` 就出现在网站里；
- 哪天不用这个站了，`coscli sync` 拉下来就是完整的目录结构，不需要跑任何程序。

## 界面

- 单页文件浏览器：面包屑、相对时间、类型图标、悬停操作
- 拖拽上传 + COS 直传进度提示；`/` 聚焦搜索，`Esc` 关闭弹层
- 搜索会同时匹配文件名和完整路径，结果右侧显示所在目录
- 新建目录是行内输入框，删除有确认弹窗（进回收站）
- 跟随系统深浅色；窄屏自动隐藏次要信息

## 起步

```bash
cp .env.example .env      # 填 DATABASE_URL / COS_*
npm run db:init
npm run check      # 自检：数据库 / COS 鉴权 / STS 签发
npm run dev
```

用 `127.0.0.1` 或局域网 IP 访问 dev server 时，Next 16 默认会拦掉 `/_next/hmr`，
表现为页面一直停在骨架屏。`next.config.ts` 里已经加了 `allowedDevOrigins`。

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
coscli cp -r ~/Pictures cos://your-bucket-1250000000/drive/Pictures
npm run reconcile -- --dry-run     # 先看会写多少条
npm run reconcile
```

百万对象时改用 COS **清单（Inventory）**，避免全量 List 的请求费：

```bash
npm run reconcile -- --inventory ./inventory.csv
```

`reconcile` 做三件事：补新增、更新变动的（size/etag）、把桶里已消失的标成删除。
它不会删任何对象——删除权只在你手里。

## 回收站

删除是软删除：`deleted_at` 打标记，对象留在原地。

```bash
npm run purge -- --days 30 --dry-run
npm run purge -- --days 30
```

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/fs/list?path=` | 列当前层（查 Postgres，不查桶） |
| GET | `/api/fs/search?q=` | 按名称/路径模糊搜索 |
| POST | `/api/fs/upload/sign` | 拿 STS 临时凭证，浏览器直传 |
| POST | `/api/fs/upload/confirm` | HeadObject 校验后落库 |
| GET | `/api/fs/download?path=` | 302 到预签名 URL（`&json=1` 返回 JSON） |
| POST | `/api/fs/mkdir` | 建目录（写零字节占位对象） |
| POST | `/api/fs/rename` | 单文件改名/移动；目录请用 coscli |
| POST | `/api/fs/delete` | 软删除（子树上所有记录） |

## 已知取舍

- **目录重命名**要逐个对象做服务端复制，TB 级会很久。先用 coscli 改，再 reconcile。
- **秒传**没做：TB 级算全量 SHA256 等于读一遍数据。要做的话用 `size + 首尾 1MB hash`。
- **多用户**没做：`nodes` 表没有 owner 列，加用户时加一列 `owner_id` 即可，路径不用动。
- **鉴权**没有：测试期间请用 IP 白名单或 Basic Auth 挡住，别裸奔在公网。
