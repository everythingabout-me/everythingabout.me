/* ============================================================
   时间轴 · 事件卡（v2）
   一条事件 = 一张卡；健康只在「变了」和「一年一次」的时候出现
   ============================================================ */
(function () {
  "use strict";

  const screen = document.getElementById("screen");
  const feed = document.getElementById("feed");

  const esc = (s) =>
    String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));

  /* ---------- 数据 ---------- */

  const SLEEP_2025 = [7.5, 7.3, 7.6, 7.2, 7.4, 6.9, 7.0, 6.7, 7.2, 7.3, 7.1, 7.0];
  const SLEEP_2024 = [7.6, 7.5, 7.6, 7.4, 7.5, 7.3, 7.4, 7.2, 7.5, 7.4, 7.3, 7.2];

  const DATA = [
    {
      y: 2025,
      events: [
        {
          ym: "2025-12",
          date: "12 月 31 日",
          title: "跨年 · 天台",
          kind: "g3",
          photos: ["g11", "g4", "g1"],
          body: "凌晨两点，风很大。楼下有人在放烟花，没拍到好看的。",
        },
        {
          ym: "2025-12",
          date: "12 月 24 日",
          title: "平安夜 · 家",
          kind: "hero",
          photos: ["g5"],
          body: "第一次自己做一整桌。火鸡烤过头了。",
          meta: "6 个人 · 21:40",
        },
        {
          ym: "2025-10",
          date: "10 月 2 日",
          title: "婚礼 · 仪式",
          kind: "hero",
          photos: ["g6"],
          body: "表姐结婚。仪式 11:20 开始，比流程晚了半小时。",
          meta: "1,840 张 → 12 张",
        },
        {
          ym: "2025-10",
          date: "10 月 3 日",
          title: "婚礼 · 宴席",
          kind: "g2",
          photos: ["g6", "g5"],
          body: "散场的时候剩下的人已经不够一桌。",
        },
        {
          ym: "2025-09",
          date: "9 月 7 日",
          title: "搬家",
          kind: "note",
          body: "搬到城西。第一晚只有一张床垫和一个箱子，窗外是高架，整夜有车过去。",
          meta: "214 个文件 · 大部分是旧物照片",
        },
        {
          ym: "2025-09",
          date: "9 月 14 日",
          title: "暗恋桃花源",
          kind: "html",
          height: 176,
          source: "大麦",
          html: `<style>
    .tk{height:100%;display:flex;flex-direction:column;justify-content:space-between;padding:16px 18px;border:1px solid var(--eam-line);border-radius:12px;background:var(--eam-surface)}
    .tk-top,.tk-bot{display:flex;justify-content:space-between;align-items:center;font-size:10.5px;letter-spacing:.08em;color:var(--eam-text3);text-transform:uppercase}
    .tk-bot{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;letter-spacing:.06em}
    .tk-mid strong{display:block;font-size:19px;font-weight:600;letter-spacing:.01em}
    .tk-mid span{display:block;margin-top:5px;font-size:12px;color:var(--eam-text2)}
    .tk-cut{height:1px;margin:11px 0;background:repeating-linear-gradient(90deg,var(--eam-line) 0 6px,transparent 6px 12px)}
    .tk-code{display:inline-flex;gap:6px;align-items:center}
    .tk-code i{display:inline-block;width:2px;height:10px;background:var(--eam-text3)}
    .tk-code i:nth-child(2){height:13px}.tk-code i:nth-child(4){height:8px}
  </style>
  <div class="tk">
    <div class="tk-top"><span>剧票</span><span>9 / 14 19:30</span></div>
    <div class="tk-mid"><strong>暗恋桃花源</strong><span>上海大剧院 · 3 排 12 座</span></div>
    <div class="tk-cut"></div>
    <div class="tk-bot">
      <span>已入场</span>
      <span class="tk-code"><i></i><i></i><i></i><i></i><i></i></span>
      <span>EAM 0914 0312</span>
    </div>
  </div>`,
        },
        {
          ym: "2025-08",
          date: "8 月 31 日",
          kind: "insight",
          title: "这个月睡得少",
          body: "8 月平均 6.7 小时，比前三个月少 42 分钟；同期静息心率高了 4 bpm。",
          values: SLEEP_2025,
          baseline: 7.3,
          caption: "近 12 个月 · 8 月最低",
        },
        {
          ym: "2025-08",
          date: "8 月 3 日",
          title: "晨跑 · 北岸",
          kind: "plain",
          body: "配速 5'02\"，最后两公里掉到 5'30\"。出门时 24 度。",
          metrics: [
            { label: "距离", value: "12.4", unit: "km" },
            { label: "配速", value: "5'02\"", unit: "/km" },
            { label: "心率", value: "148", unit: "bpm" },
          ],
          source: "Strava",
        },
        {
          ym: "2025-08",
          date: "8 月 15 日",
          title: "海边 · 日落",
          kind: "hero",
          photos: ["g1"],
          body: "18:56，退潮退得很远。",
        },
        {
          ym: "2025-08",
          date: "8 月 22 日",
          title: "朋友来访",
          kind: "g3",
          photos: ["g5", "g10", "g12"],
          body: "两年没见，带来的酒没喝完。",
        },
        {
          ym: "2025-07",
          date: "7 月 12 日",
          title: "露营 · 山谷",
          kind: "hero",
          photos: ["g8"],
          body: "夜里 12 度，睡袋不够。第二天早上雾气到九点才散。",
          meta: "2 晚 · 180 张",
        },
        {
          ym: "2025-06",
          date: "6 月 12 – 15 日",
          title: "青海湖 · 四天",
          kind: "hero",
          photos: ["g3"],
          body: "第三天翻到海拔 4,100，风把三脚架吹倒了。回来之后一周都在补觉。",
          meta: "780 张 → 9 张 · 12.6 km",
        },
        {
          ym: "2025-06",
          date: "6 月 21 日",
          title: "家 · 生日",
          kind: "g4",
          photos: ["g6", "g5", "g12", "g10"],
          body: "蛋糕是楼下买的，蜡烛少了一根。",
          meta: "3 人",
        },
        {
          ym: "2025-06",
          date: "6 月 24 日",
          title: "夜骑 · 环城",
          kind: "hero",
          photos: ["g4"],
          body: "21:40 出门，绕了 38 公里，回到楼下已经快一点。",
        },
        {
          ym: "2025-06",
          date: "6 月 1 – 30 日",
          title: "每日一镜 · 六月",
          kind: "row",
          photos: ["g11"],
          body: "30 集 · 平均 4 分 12 秒",
          meta: "每天一条，镜头固定",
        },
        {
          ym: "2025-04",
          date: "4 月 18 日",
          title: "读完《海边的卡夫卡》",
          kind: "plain",
          body: "断断续续读了两个月。第 41 章是在医院走廊看完的，那天的光很白。",
          metrics: [
            { label: "用时", value: "6.3", unit: "h" },
            { label: "评分", value: "4", unit: "/5" },
          ],
          source: "Readwise",
        },
        {
          ym: "2025-05",
          date: "5 月 3 日",
          title: "西湖 · 徒步",
          kind: "g3",
          photos: ["g2", "g8", "g9"],
          body: "12.6 公里，爬升 620 米。下山的时候下了半小时雨。",
        },
        {
          ym: "2025-02",
          date: "2 月 14 日",
          title: "第一个雪天",
          kind: "hero",
          photos: ["g3"],
          body: "早上六点，路上还没有人踩过。",
        },
      ],
    },
    {
      y: 2024,
      events: [
        {
          ym: "2024-12",
          date: "12 月 31 日",
          title: "年末 · 烟花",
          kind: "g2",
          photos: ["g4", "g11"],
          body: "在天台上看到三处烟花，只有一处拍清楚了。",
        },
        {
          ym: "2024-12",
          date: "12 月 22 – 26 日",
          title: "京都 · 五天",
          kind: "hero",
          photos: ["g1"],
          body: "第一天就下雪。每天走两万步，回来脚肿了。",
          meta: "1,204 张 → 18 张",
        },
        {
          ym: "2024-08",
          date: "8 月 10 日",
          title: "露营 · 湖边",
          kind: "hero",
          photos: ["g8"],
          body: "第一次自己搭帐篷，用了四十分钟。",
        },
        {
          ym: "2024-05",
          date: "5 月 4 日",
          title: "植物园",
          kind: "g3",
          photos: ["g12", "g8", "g10"],
          body: "芍药开得正好，人比花多。",
        },
        {
          ym: "2024-01",
          date: "1 月 6 日",
          title: "第一场雪",
          kind: "hero",
          photos: ["g3"],
          body: "楼下的车顶都白了，中午就化了。",
        },
      ],
    },
  ];

  /* ---------- 渲染 ---------- */

  function spark(values, baseline, w, h) {
    const min = Math.min(...values, baseline);
    const max = Math.max(...values, baseline);
    const span = max - min || 1;
    const y = (v) => (h - 3 - ((v - min) / span) * (h - 6)).toFixed(2);
    const coords = values.map((v, i) => [((i / (values.length - 1)) * w).toFixed(2), y(v)]);
    const pts = coords.map((p) => p.join(",")).join(" ");
    const area = `M0,${h} L${coords.map((p) => p.join(",")).join(" L")} L${w},${h} Z`;
    return `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" aria-hidden="true">
      <path class="tl-spark-area" d="${area}"/>
      <line class="tl-spark-base" x1="0" y1="${y(baseline)}" x2="${w}" y2="${y(baseline)}" vector-effect="non-scaling-stroke"/>
      <polyline class="tl-spark" points="${pts}" vector-effect="non-scaling-stroke"/>
    </svg>`;
  }

  function media(e) {
    if (!e.photos || !e.photos.length) return "";
    const vs = ["a", "b", "c"];
    if (e.kind === "hero" || e.kind === "wide") {
      return `<a class="tl-media ${e.kind}" href="#" aria-label="${esc(e.title)}">
        <span class="tl-ph" data-g="${e.photos[0]}" data-v="a"></span>
      </a>`;
    }
    const cls = e.kind === "g2" ? "g2" : e.kind === "g4" ? "g4" : "g3";
    const cells = e.photos
      .map((g, i) => `<span class="tl-ph" data-g="${g}" data-v="${vs[i % 3]}"></span>`)
      .join("");
    return `<a class="tl-media" href="#" aria-label="${esc(e.title)}"><span class="tl-grid ${cls}">${cells}</span></a>`;
  }

  function kicker(e) {
    const src = e.source ? `<span>·</span><span class="tl-src">${esc(e.source)}</span>` : "";
    const health = e.kind === "insight" ? "<span>·</span><span>健康</span>" : "";
    return `<div class="tl-evk"><b>${e.year} 年 ${esc(e.date)}</b>${src}${health}</div>`;
  }

  function stats(e) {
    if (!e.metrics || !e.metrics.length) return "";
    const items = e.metrics
      .map(
        (m) =>
          `<span><em>${esc(m.value)}${m.unit ? `<s>${esc(m.unit)}</s>` : ""}</em><i>${esc(m.label)}</i></span>`,
      )
      .join("");
    return `<div class="tl-stats">${items}</div>`;
  }

  function tokenCSS() {
    const cs = getComputedStyle(screen);
    const v = (n) => cs.getPropertyValue(n).trim();
    const mode = document.documentElement.dataset.theme === "dark" ? "dark" : "light";
    return `:root{color-scheme:${mode};--eam-bg:${v("--bg")};--eam-surface:${v("--surface")};--eam-text:${v("--text")};--eam-text2:${v("--text-2")};--eam-text3:${v("--text-3")};--eam-line:${v("--border")};--eam-accent:${v("--blue")}}`;
  }

  function htmlCard(e) {
    const h = Math.max(40, Math.min(640, e.height || 176));
    const doc = `<!doctype html><meta charset="utf-8"><style>${tokenCSS()}
      html,body{margin:0;height:100%;background:transparent;color:var(--eam-text);font:13px/1.55 -apple-system,BlinkMacSystemFont,"PingFang SC","Microsoft YaHei",sans-serif;-webkit-font-smoothing:antialiased}
      *{box-sizing:border-box}
    </style>${e.html}`;
    return `<iframe class="tl-htmlcard" sandbox="" height="${h}" style="height:${h}px" title="${esc(e.title)}" srcdoc="${esc(doc)}"></iframe>`;
  }

  function plainHTML(e) {
    return `<article class="tl-ev">
      ${kicker(e)}
      <h3 class="tl-evtitle">${esc(e.title)}</h3>
      ${e.body ? `<p class="tl-evbody">${esc(e.body)}</p>` : ""}
      ${stats(e)}
    </article>`;
  }

  function eventHTML(e) {
    if (e.kind === "plain") return plainHTML(e);

    if (e.kind === "html") {
      return `<article class="tl-ev">
        ${kicker(e)}
        ${htmlCard(e)}
      </article>`;
    }

    if (e.kind === "note") {
      return `<article class="tl-ev">
        <div class="tl-note">
          ${kicker(e)}
          <h3 class="tl-evtitle">${esc(e.title)}</h3>
          <p class="tl-evbody">${esc(e.body)}</p>
          ${e.meta ? `<div class="tl-evmeta">${esc(e.meta)}</div>` : ""}
        </div>
      </article>`;
    }

    if (e.kind === "insight") {
      return `<article class="tl-ev">
        <div class="tl-insight">
          ${kicker(e)}
          <h3 class="tl-evtitle">${esc(e.title)}</h3>
          <p class="tl-evbody">${esc(e.body)}</p>
          ${spark(e.values, e.baseline, 100, 34)}
          <div class="tl-insight-foot"><span>${esc(e.caption)}</span></div>
        </div>
      </article>`;
    }

    if (e.kind === "row") {
      return `<article class="tl-ev">
        ${kicker(e)}
        <a class="tl-rowcard" href="#">
          <span class="tl-rowthumb"><span class="tl-ph" data-g="${e.photos[0]}" data-v="a"></span></span>
          <span class="tl-rowbody">
            <span class="tl-evtitle">${esc(e.title)}</span>
            <span class="tl-rowmeta">${esc(e.body)}</span>
          </span>
          <span class="tl-rowgo">查看 <svg class="ic"><use href="#i-right"/></svg></span>
        </a>
        ${e.meta ? `<div class="tl-evmeta">${esc(e.meta)}</div>` : ""}
      </article>`;
    }

    return `<article class="tl-ev">
      ${kicker(e)}
      <h3 class="tl-evtitle">${esc(e.title)}</h3>
      ${media(e)}
      ${e.body ? `<p class="tl-evbody">${esc(e.body)}</p>` : ""}
      ${stats(e)}
      ${e.meta ? `<div class="tl-evmeta">${esc(e.meta)}</div>` : ""}
    </article>`;
  }

  function dayOf(e) {
    const m = String(e.date).match(/月\s*(\d+)/);
    return m ? Number(m[1]) : 0;
  }

  const EVENTS = [];
  DATA.forEach((y) =>
    y.events.forEach((e) =>
      EVENTS.push({
        ...e,
        year: y.y,
        key: Number(e.ym.slice(0, 4)) * 10000 + Number(e.ym.slice(5)) * 100 + dayOf(e),
      }),
    ),
  );
  EVENTS.sort((a, b) => b.key - a.key);

  function render() {
    feed.innerHTML = EVENTS.map(eventHTML).join("");
  }
  render();

  /* ---------- 交互 ---------- */

  const btnTheme = document.getElementById("btnTheme");
  function paintTheme() {
    const dark = document.documentElement.dataset.theme === "dark";
    btnTheme.innerHTML = dark
      ? `<svg class="ic"><use href="#i-sun"/></svg><span>浅色</span>`
      : `<svg class="ic"><use href="#i-moon"/></svg><span>深色</span>`;
  }
  btnTheme.addEventListener("click", () => {
    const dark = document.documentElement.dataset.theme === "dark";
    document.documentElement.dataset.theme = dark ? "light" : "dark";
    try {
      localStorage.setItem("eam-mock-theme", document.documentElement.dataset.theme);
    } catch (e) {}
    paintTheme();
    render();
  });
  paintTheme();

  const btnView = document.getElementById("btnView");
  btnView.addEventListener("click", () => {
    const mobile = screen.dataset.frame === "mobile";
    screen.dataset.frame = mobile ? "desktop" : "mobile";
    document.body.classList.toggle("is-mobile", !mobile);
    btnView.textContent = mobile ? "移动" : "桌面";
    screen.scrollTop = 0;
  });

  const notes = document.getElementById("notes");
  function toggleNotes(open) {
    notes.classList.toggle("open", open);
    notes.setAttribute("aria-hidden", String(!open));
  }
  document.getElementById("btnNotes").addEventListener("click", () => toggleNotes(!notes.classList.contains("open")));
  document.getElementById("btnNotesClose").addEventListener("click", () => toggleNotes(false));
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") toggleNotes(false);
  });

  const q = new URLSearchParams(location.search);
  if (q.get("view") === "mobile") {
    screen.dataset.frame = "mobile";
    document.body.classList.add("is-mobile");
    btnView.textContent = "桌面";
  }
  if (q.get("notes")) toggleNotes(true);
  if (q.get("scroll")) {
    requestAnimationFrame(() => screen.scrollTo({ top: Number(q.get("scroll")), behavior: "auto" }));
  }
})();
