# Visual system · Plain

One interface, two temperatures. Colors, type, buttons, icons, radii, and spacing all converge on one set of tokens;
**B (warm white) has already shipped as the real site**; A (plain white) is the same components under a cool preset.

## Preview

```bash
cd docs/design/mocks
python3 -m http.server 4173 --bind 127.0.0.1
```

- <http://127.0.0.1:4173/system.html> — **the visual system spec**: type / sizes / colors / buttons / icons / forms / surfaces
- <http://127.0.0.1:5173/> — **the real site** (warm white B has shipped)
- <http://127.0.0.1:4173/> — A/B switchable preview + selection
- `a.html` — A plain white; `b.html` — B warm white
- Both can switch to dark in the top right; `?theme=dark`, `?view=app` / `?view=auth` also work

## Timeline (event cards · clickable mock)

`timeline.html` — the timeline has exactly one layer: **event cards**, a flat list from top to bottom. Synthetic data.

- No year / month headings, no "N files / N events" statistics, no year band and no density bars; every card carries its full date
- One event = one card, and cards are typed: `photo` / `note` / `metric` / `html` (the theater ticket in the mock is an html card, rendered in a sandbox iframe)
- Events have no editing, and no "view details" button (when there is a detail_url, the title is the link)
- For events written by third-party apps, the date line gains an origin name (in the mock: 「晨跑」 comes from Strava, 「读完」 from Readwise, 「剧票」 from 大麦); the structure is in [events.md](../events.md)
- Health shows a card only **when something changed** (in the mock: less sleep in August); no monthly charts, no annual overview
- Bottom-left toolbar: notes (what this version cut), dark, mobile
- Dark / mobile: `?theme=dark`, `?view=mobile`; `?notes=1` opens the notes directly

Screenshots: `shots/timeline-desktop.png` (first screen), `shots/timeline-events.png` (text events + the health change card),
`shots/timeline-source.png` (third-party origins + number slots), `shots/timeline-mobile.png`, `shots/timeline-dark.png`.

## Type

**Don't pick a single typeface; pick a system font stack.** Apple's approach is to let the system choose a typeface per character rather than having one typeface cover both Latin and Chinese:

- Apple: Latin SF Pro Text / Display, Chinese PingFang SC
- Windows: Latin Segoe UI, Chinese Microsoft YaHei
- Android / Linux: Latin Roboto / Noto Sans, Chinese Noto Sans CJK

```css
--font-ui:
  -apple-system, BlinkMacSystemFont,
  "SF Pro Text", "SF Pro Display", "Segoe UI",
  "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei",
  "Noto Sans SC", "Hiragino Kaku Gothic ProN", "Yu Gothic",
  "Apple SD Gothic Neo", "Malgun Gothic", "Noto Sans JP", "Noto Sans KR",
  "Helvetica Neue", Arial, sans-serif;
```

The order of the stack is the fallback order: the Chinese faces sit after `-apple-system`, and the browser picks per character. Don't cover every language with one CJK webfont — it's big, it's slow on first paint, and it fits each platform worse than the system fonts. If you need Latin on Windows / Android to look close to SF too, add a self-hosted **Inter** after `-apple-system` and keep Chinese on the system fonts.

### Sizes

| Token | Size / Line | Weight | Usage |
|---|---|---|---|
| display | 28 / 1.25 | 600 | large headings / empty states |
| title-1 | 22 / 1.35 | 600 | page and card titles |
| title-2 | 17 / 1.4 | 600 | section titles |
| title-3 | 15 / 1.4 | 600 | subheadings |
| body | 14 / 1.65 | 400 | body text |
| secondary | 12.5 / 1.6 | 400 | supporting copy |
| label | 12 / 1.4 | 500 | form labels |
| eyebrow | 10.5 / 1.4 | 600 | small English labels · .08em |
| caption | 11.5 / 1.5 | 400 | time / hints |
| button | 13 / 1.2 | 600 | buttons |

No negative tracking for Chinese; numbers use `tabular-nums`; only 400 / 500 / 600; monospace only for code and paths.

## Color

- Neutrals (warm white B): Paper `#FAF8F4` / Surface `#FFFDFB` / Hover `#F3F0EA` / Line `#E8E3DA` / Line strong `#D6CEC2` / Ink `#1A1714` / Ink 2 `#615B52` / Ink 3 `#786F65`
- Blue: Blue light `#EAF6FE` (icon ground) / Blue `#13A7F5` (primary button) / Blue ink `#0B7BC4` (icon strokes, links, focus)
- Danger: `#B4483F` (delete / remove password only)
- Coral `#FD605E` stays in the logo only and never enters a component

## Buttons

| Type | When to use | Style |
|---|---|---|
| Primary | upload / continue / save; one per view | Blue `#13A7F5` + white text (bright blue + dark text in dark mode) |
| Secondary | send verification code / sign out | Surface + 1px line + ink text |
| Critical | delete confirmation | brick red solid + white text |
| Critical text | inline delete / remove password | brick red text |
| Ghost | icon buttons / light actions | transparent + gray text; 30×30, 8px radius |
| Link | page switches / secondary entry points | Blue ink; underlined on hover |

Sizes 30 / 36 / 44; radii 8 / 10 / 10; type sizes 12 / 13 / 14. No gradients, no hard shadows, no two-tone buttons.

## Icons

- 16×16 grid, 1.5px stroke, round caps and joins, currentColor, no fill
- 16px by default, 14px in dense areas, 20px in empty states
- File types are not color-coded by type: one pale blue ground + one dark blue stroke, distinguished by shape
- Icon buttons are 30×30; an aria-label is mandatory; don't mix in other icon libraries, don't use emoji

## Radii / spacing

6 (chip) · 8 (icons/small controls) · 10 (buttons/inputs) · 14 (cards/panels) · 18 (dialogs).
Spacing 4 / 8 / 12 / 16 / 20 / 24 / 32 / 40 / 48 / 64.

## The two presets

| | A plain white | B warm white |
|---|---|---|
| Ground | `#F6F7F7` cool white | `#FAF8F4` warm white (shipped) |
| Content width | 880px | 920px |
| Row height | 52px | 58px |
| Header | breadcrumb | page title + one line of stats |
| Components | shared | shared |

## Files

```
timeline.html    timeline · event cards (clickable mock)
timeline.css     timeline styles (reuses the .v-b tokens)
timeline.js      synthetic data + rendering + interaction
system.html      the visual system spec
index.html       A/B switchable preview
a.html / b.html  A plain white / B warm white
plain.css        shared styles (review page + both app versions + dark)
favicon.svg      logo
shots/           real Chrome-rendered screenshots (1280 viewport, @2x)
```

## What shipped on the real site

This system has already landed in `web/`:

- `web/src/styles/globals.css` — warm white / blue / brick red tokens, system font stack, focus
- `web/src/pages/DrivePage.tsx` / `DrivePage.module.css` — B's title + stats + new directory, 58px row height, pale blue glyph
- `web/src/pages/StreamPage.module.css` — the stream home's top bar, month headings, empty states, buttons
- `web/src/styles/auth.module.css` — login / account cards, inputs, blue primary button, brick red danger
- `web/src/components/icons.tsx` — icon stroke width unified at 1.5

## Notes
- White text on `#13A7F5` has a contrast ratio of about 2.7:1. For more reliable readability, push the button blue one half-step deeper (e.g. `#0B87D0`) or switch to dark ink text; this time we're going with the logo's original color.
