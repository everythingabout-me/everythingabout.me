# 视觉系统 · 朴素版

一套界面，两种温度。颜色、字体、按钮、图标、圆角、间距都收敛到同一套 token；
**B 暖白已落地成实站**，A 素白是同一套组件的冷色 preset。

## 预览

```bash
cd docs/design/mocks
python3 -m http.server 4173 --bind 127.0.0.1
```

- <http://127.0.0.1:4173/system.html> — **视觉系统规范**：字体 / 字号 / 颜色 / 按钮 / 图标 / 表单 / 表面
- <http://127.0.0.1:5173/> — **实站**（暖白 B 已落地）
- <http://127.0.0.1:4173/> — A/B 切换预览 + 选型
- `a.html` — A 素白；`b.html` — B 暖白
- 右上角都能切深色；也支持 `?theme=dark`、`?view=app` / `?view=auth`

## 字体

**不选单一字体，选系统字体栈。** 苹果的做法是让系统按字符挑字体，而不是一款字体覆盖中英文：

- Apple：拉丁 SF Pro Text / Display，中文 PingFang SC
- Windows：拉丁 Segoe UI，中文 Microsoft YaHei
- Android / Linux：拉丁 Roboto / Noto Sans，中文 Noto Sans CJK

```css
--font-ui:
  -apple-system, BlinkMacSystemFont,
  "SF Pro Text", "SF Pro Display", "Segoe UI",
  "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei",
  "Noto Sans SC", "Hiragino Kaku Gothic ProN", "Yu Gothic",
  "Apple SD Gothic Neo", "Malgun Gothic", "Noto Sans JP", "Noto Sans KR",
  "Helvetica Neue", Arial, sans-serif;
```

字体栈的顺序就是回退顺序：中文字体放在 `-apple-system` 后面，浏览器会逐字选择。不要用一款 CJK webfont 覆盖全部语言——体积大、首屏慢，而且不如系统字体贴合平台。若要求 Windows / Android 的拉丁也接近 SF，可在 `-apple-system` 之后加自托管的 **Inter**，中文继续走系统。

### 字号

| Token | Size / Line | Weight | 用途 |
|---|---|---|---|
| display | 28 / 1.25 | 600 | 大标题 / 空态 |
| title-1 | 22 / 1.35 | 600 | 页面、卡片标题 |
| title-2 | 17 / 1.4 | 600 | 区块标题 |
| title-3 | 15 / 1.4 | 600 | 小标题 |
| body | 14 / 1.65 | 400 | 正文 |
| secondary | 12.5 / 1.6 | 400 | 辅助说明 |
| label | 12 / 1.4 | 500 | 表单标签 |
| eyebrow | 10.5 / 1.4 | 600 | 英文小标签 · .08em |
| caption | 11.5 / 1.5 | 400 | 时间 / 提示 |
| button | 13 / 1.2 | 600 | 按钮 |

中文不做负字距；数字 `tabular-nums`；只用 400 / 500 / 600；只有代码、路径用等宽。

## 颜色

- 中性（暖白 B）：Paper `#FAF8F4` / Surface `#FFFDFB` / Hover `#F3F0EA` / Line `#E8E3DA` / Line strong `#D6CEC2` / Ink `#1A1714` / Ink 2 `#615B52` / Ink 3 `#786F65`
- 蓝：Blue light `#EAF6FE`（图标底）/ Blue `#13A7F5`（主按钮）/ Blue ink `#0B7BC4`（图标线、链接、焦点）
- 危险：`#B4483F`（只给删除 / 移除密码）
- 珊瑚 `#FD605E` 只留在 logo，不进入组件

## 按钮

| 类型 | 用法 | 样式 |
|---|---|---|
| Primary | 上传 / 继续 / 保存，一个视图一个 | Blue `#13A7F5` + 白字（深色亮蓝 + 深字） |
| Secondary | 发送验证码 / 退出登录 | Surface + 1px 线 + 墨字 |
| Critical | 删除确认 | 砖红实心 + 白字 |
| Critical text | 行内删除 / 移除密码 | 砖红文字 |
| Ghost | 图标按钮 / 轻操作 | 透明 + 灰字；30×30，8px 圆角 |
| Link | 页面切换 / 补充入口 | Blue ink；hover 下划线 |

尺寸 30 / 36 / 44；圆角 8 / 10 / 10；字号 12 / 13 / 14。没有渐变、没有硬阴影、没有双色按钮。

## 图标

- 16×16 网格、1.5px 描边、圆头圆角、currentColor、无填充
- 常用 16px，密集处 14px，空态 20px
- 文件类型不按类型上色：同一块浅蓝底 + 同一个深蓝线，靠形状区分
- 图标按钮 30×30；必须有 aria-label；不混用其他图标库、不用 emoji

## 圆角 / 间距

6（chip）· 8（图标/小控件）· 10（按钮/输入）· 14（卡片/面板）· 18（弹窗）。
间距 4 / 8 / 12 / 16 / 20 / 24 / 32 / 40 / 48 / 64。

## 两版 preset

| | A 素白 | B 暖白 |
|---|---|---|
| 底色 | `#F6F7F7` 冷白 | `#FAF8F4` 暖白（已落地） |
| 内容宽 | 880px | 920px |
| 行高 | 52px | 58px |
| 标题 | 面包屑 | 页面标题 + 一行统计 |
| 组件 | 共用 | 共用 |

## 文件

```
system.html      视觉系统规范
index.html       A/B 切换预览
a.html / b.html  A 素白 / B 暖白
plain.css        共享样式（评审页 + 两版 app + 深色）
favicon.svg      logo
shots/           真实 Chrome 渲染截图（1280 视口，@2x）
```

## 实站落地

这套系统已经落到 `web/`：

- `web/src/styles/globals.css` — 暖白 / 蓝 / 砖红 token、系统字体栈、focus
- `web/src/pages/DrivePage.tsx` / `DrivePage.module.css` — B 的标题 + 统计 + 新建目录、58 行高、浅蓝 glyph
- `web/src/pages/StreamPage.module.css` — 时间线首页的顶栏、月标题、空态、按钮
- `web/src/styles/auth.module.css` — 登录 / 账户的卡片、输入、蓝色主按钮、砖红危险
- `web/src/components/icons.tsx` — 图标线宽统一 1.5

## 备注
- `#13A7F5` 上的白字对比约 2.7:1。如果要在可读性上更稳，可以只把按钮蓝压深半档（如 `#0B87D0`）或改用深墨字；这次先按 logo 原色。
