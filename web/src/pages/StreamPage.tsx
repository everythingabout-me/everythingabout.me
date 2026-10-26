import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useLoaderData, useNavigate } from "react-router";
import {
  ApiError,
  api,
  type AuthUser,
  type StreamFeed,
  type StreamItem,
  type StreamPeriod,
  type StreamSeries,
  type StreamStatus,
  type StreamThumb,
} from "../api/client";
import { BrandMark } from "../components/BrandMark";
import { RefreshIcon } from "../components/icons";
import styles from "./StreamPage.module.css";

function formatMonth(month: string): string {
  const [year, mon] = month.split("-");
  return `${year} 年 ${Number(mon)} 月`;
}

function formatRange(from?: string, to?: string): string {
  if (!from || !to) return "";
  const [fy, fm, fd] = from.split("-").map(Number);
  const [ty, tm, td] = to.split("-").map(Number);
  if (fy === ty && fm === tm) return `${fm} 月 ${fd} – ${td} 日`;
  if (fy === ty) return `${fm} 月 ${fd} 日 – ${tm} 月 ${td} 日`;
  return `${fy} 年 ${fm} 月 ${fd} 日 – ${ty} 年 ${tm} 月 ${td} 日`;
}

function formatMetric(value: number): string {
  if (!Number.isFinite(value)) return "—";
  if (Math.abs(value) >= 1000) return Math.round(value).toLocaleString("zh-CN");
  if (Number.isInteger(value)) return String(value);
  return value.toFixed(1);
}

function filesHref(folder?: string): string {
  return folder ? `/drive?path=${encodeURIComponent(folder)}` : "/drive";
}

export function StreamPage() {
  const { user } = useLoaderData() as { user: AuthUser };
  const navigate = useNavigate();

  const [periods, setPeriods] = useState<StreamPeriod[]>([]);
  const [status, setStatus] = useState<StreamStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const nextCursor = useRef<string | undefined>(undefined);
  const loadingMoreRef = useRef(false);
  const sentinel = useRef<HTMLDivElement>(null);
  const wasPending = useRef(false);

  const merge = useCallback((current: StreamPeriod[], incoming: StreamPeriod[]) => {
    const seen = new Set(current.map((period) => period.month));
    const merged = [...current];
    for (const period of incoming) {
      if (!seen.has(period.month)) {
        seen.add(period.month);
        merged.push(period);
      }
    }
    return merged;
  }, []);

  const loadInitial = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const feed: StreamFeed = await api.stream(undefined, 2);
      setPeriods(feed.periods);
      nextCursor.current = feed.next;
      setHasMore(Boolean(feed.next));
      setStatus(feed.status);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      setError(cause instanceof Error ? cause.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, [navigate]);

  const loadMore = useCallback(async () => {
    const cursor = nextCursor.current;
    if (!cursor || loadingMoreRef.current) return;
    loadingMoreRef.current = true;
    setLoadingMore(true);
    try {
      const feed = await api.stream(cursor, 2);
      setPeriods((current) => merge(current, feed.periods));
      nextCursor.current = feed.next;
      setHasMore(Boolean(feed.next));
      setStatus(feed.status);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      setError(cause instanceof Error ? cause.message : "加载失败");
    } finally {
      loadingMoreRef.current = false;
      setLoadingMore(false);
    }
  }, [merge, navigate]);

  useEffect(() => {
    void loadInitial();
  }, [loadInitial]);

  // 整理进行中时轮询状态；结束时自动刷新一次，让新内容出现。
  useEffect(() => {
    const pending = status?.pending ?? 0;
    if (pending > 0) {
      wasPending.current = true;
      return;
    }
    if (wasPending.current) {
      wasPending.current = false;
      void loadInitial();
    }
  }, [status?.pending, loadInitial]);

  useEffect(() => {
    if (!status || status.pending === 0) return;
    let cancelled = false;
    const timer = window.setInterval(async () => {
      try {
        const latest = await api.streamStatus();
        if (!cancelled) setStatus(latest);
      } catch {
        // 状态轮询失败不打扰阅读。
      }
    }, 4000);
    return () => {
      cancelled = true;
      window.clearInterval(timer);
    };
  }, [status?.pending]);

  useEffect(() => {
    const node = sentinel.current;
    if (!node) return;
    const observer = new IntersectionObserver(
      (entries) => {
        if (entries[0]?.isIntersecting) void loadMore();
      },
      { rootMargin: "700px" },
    );
    observer.observe(node);
    return () => observer.disconnect();
  }, [loadMore, hasMore]);

  const empty = !loading && periods.length === 0;
  const indexing = Boolean(status?.pending && status.pending > 0);

  return (
    <div className={styles.page}>
      <title>Everything About Me</title>

      <header className={styles.topbar}>
        <div className={styles.topbarInner}>
          <Link to="/" className={styles.brand}>
            <BrandMark className={styles.brandMark} />
            <span className={styles.brandText}>Everything About Me</span>
          </Link>

          <nav className={styles.nav}>
            <Link className={styles.navLink} to="/drive">
              文件
            </Link>
          </nav>

          <div className={styles.spacer} />

          <Link
            className={styles.account}
            to="/account"
            title="账户设置"
            aria-label={`账户设置：${user.email}`}
          >
            <span className={styles.avatar}>{user.email.slice(0, 1).toUpperCase()}</span>
            <span className={styles.email}>{user.email}</span>
          </Link>
        </div>
      </header>

      <main className={styles.main}>
        {indexing && (
          <div className={styles.working}>
            <span className={styles.dot} />
            <span>正在整理更早的内容</span>
            <span className={styles.workingMeta}>已整理 {status?.indexed_files ?? 0} 个文件</span>
          </div>
        )}

        {loading && <LoadingSkeleton />}

        {!loading && error && (
          <div className={styles.errorBox}>
            <p>{error}</p>
            <button className={styles.textButton} onClick={() => void loadInitial()}>
              <RefreshIcon size={13} /> 重试
            </button>
          </div>
        )}

        {!loading && !error && empty && (
          <div className={styles.empty}>
            <span className={styles.emptyTitle}>{indexing ? "正在整理…" : "还没有内容"}</span>
            <span className={styles.emptyText}>
              {indexing
                ? "第一次整理会久一点，照片和视频会按时间出现在这里。"
                : "上传一些照片、视频或数据文件，它们会按时间出现在这里。"}
            </span>
            {!indexing && (
              <Link className={styles.emptyAction} to="/drive">
                去文件里上传
              </Link>
            )}
          </div>
        )}

        {!loading && !error && !empty && (
          <div className={styles.stream}>
            {periods.map((period) => (
              <StreamMonth key={period.month} period={period} />
            ))}
          </div>
        )}

        <div ref={sentinel} className={styles.sentinel} aria-hidden="true" />

        {loadingMore && (
          <div className={styles.moreLoading}>
            <span className={styles.dot} />
            继续往下…
          </div>
        )}
        {!loading && !loadingMore && !hasMore && !empty && !error && (
          <div className={styles.end}>到底了</div>
        )}
      </main>
    </div>
  );
}

function StreamMonth({ period }: { period: StreamPeriod }) {
  return (
    <section className={styles.month}>
      <header className={styles.monthHead}>
        <h2 className={styles.monthTitle}>{formatMonth(period.month)}</h2>
        <span className={styles.monthMeta}>{period.file_count.toLocaleString("zh-CN")} 个文件</span>
      </header>
      <div className={styles.list}>
        {period.items.map((item, index) => (
          <StreamRow key={`${period.month}-${index}-${item.id ?? item.from ?? item.type}`} item={item} />
        ))}
        {period.items.length === 0 && (
          <div className={styles.gap}>
            <span className={styles.gapLine} />
            <span className={styles.gapText}>这个月没有可展示的内容</span>
            <span className={styles.gapMeta}>{period.file_count} 个文件</span>
          </div>
        )}
      </div>
    </section>
  );
}

function StreamRow({ item }: { item: StreamItem }) {
  if (item.type === "gap") {
    return (
      <div className={styles.gap}>
        <span className={styles.gapLine} />
        <span className={styles.gapText}>
          {item.label === "正在整理" ? "正在整理" : formatRange(item.from, item.to)}
        </span>
        {item.item_count ? <span className={styles.gapMeta}>{item.item_count} 个文件</span> : null}
      </div>
    );
  }

  if (item.type === "ambient") {
    return (
      <div className={styles.ambient}>
        {(item.series ?? []).map((series) => (
          <Sparkline key={series.metric} series={series} />
        ))}
      </div>
    );
  }

  const gridLike = item.layout === "mosaic" || item.layout === "series";
  const thumbs = gridLike ? item.grid ?? [] : item.cover ? [item.cover] : [];
  if (!thumbs.length) return null;

  return (
    <article className={styles.card}>
      <Link
        className={`${styles.media} ${
          item.layout === "hero"
            ? styles.mediaHero
            : item.layout === "mosaic"
              ? styles.mediaMosaic
              : styles.mediaSingle
        }`}
        to={filesHref(item.folder)}
        aria-label={item.title ?? "查看"}
      >
        {item.layout === "series" ? (
          <SeriesStrip thumbs={thumbs} />
        ) : item.layout === "mosaic" ? (
          <Mosaic thumbs={thumbs} count={thumbs.length} />
        ) : (
          <MediaThumb thumb={thumbs[0]} big={item.layout === "hero"} />
        )}
        {item.more ? (
          <span className={styles.moreChip}>
            还有 {item.more} {item.layout === "series" ? "个" : "张"}
          </span>
        ) : null}
      </Link>
      <div className={styles.cardText}>
        {item.title && <div className={styles.cardTitle}>{item.title}</div>}
        {item.subtitle && <div className={styles.cardSub}>{item.subtitle}</div>}
      </div>
    </article>
  );
}

function Mosaic({ thumbs, count }: { thumbs: StreamThumb[]; count: number }) {
  return (
    <div className={styles.mosaic} data-count={Math.min(count, 9)}>
      {thumbs.map((thumb, index) => (
        <MediaThumb key={`${thumb.path}-${index}`} thumb={thumb} compact={count > 1} />
      ))}
    </div>
  );
}

function SeriesStrip({ thumbs }: { thumbs: StreamThumb[] }) {
  return (
    <div className={styles.seriesStrip} data-count={Math.min(thumbs.length, 6)}>
      {thumbs.map((thumb, index) => (
        <MediaThumb key={`${thumb.path}-${index}`} thumb={thumb} compact />
      ))}
    </div>
  );
}

function hueFor(value: string): number {
  let hash = 0;
  for (let i = 0; i < value.length; i += 1) {
    hash = (hash * 31 + value.charCodeAt(i)) % 360;
  }
  return hash;
}

function MediaThumb({
  thumb,
  big = false,
  compact = false,
}: {
  thumb: StreamThumb;
  big?: boolean;
  compact?: boolean;
}) {
  const [failed, setFailed] = useState(false);

  if (thumb.video) {
    const style = { "--tile-hue": String(hueFor(thumb.path)) } as React.CSSProperties;
    return (
      <div className={`${styles.videoTile} ${big ? styles.bigTile : ""}`} style={style}>
        <span className={styles.play} aria-hidden="true" />
        {!compact && (
          <span className={styles.videoTag}>
            {thumb.duration_ms ? formatDuration(thumb.duration_ms) : "视频"}
          </span>
        )}
      </div>
    );
  }
  if (failed || !thumb.url) {
    return <div className={styles.imageFallback} aria-hidden="true" />;
  }
  return (
    <img
      className={styles.image}
      src={thumb.url}
      alt={thumb.name ?? ""}
      loading="lazy"
      decoding="async"
      onError={() => setFailed(true)}
    />
  );
}

function Sparkline({ series }: { series: StreamSeries }) {
  const values = series.values;
  const min = series.min;
  const max = series.max;
  const points = values
    .map((value, index) => {
      const x = values.length <= 1 ? 50 : (index / (values.length - 1)) * 100;
      const y = max === min ? 12 : 20 - ((value - min) / (max - min)) * 16;
      return `${x.toFixed(2)},${y.toFixed(2)}`;
    })
    .join(" ");

  return (
    <div className={styles.series}>
      <span className={styles.seriesLabel} title={series.label}>
        {series.label}
      </span>
      <svg className={styles.spark} viewBox="0 0 100 24" preserveAspectRatio="none" aria-hidden="true">
        <polyline
          points={points}
          fill="none"
          stroke="var(--accent)"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />
      </svg>
      <span className={styles.seriesValue}>
        {formatMetric(series.latest)}
        {series.unit ? ` ${series.unit}` : ""}
      </span>
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className={styles.stream} aria-busy="true">
      {[0, 1].map((index) => (
        <div className={styles.month} key={index}>
          <div className={styles.monthHead}>
            <span className={styles.skTitle} />
            <span className={styles.skMeta} />
          </div>
          <div className={styles.skeletonCard} />
          <div className={styles.skeletonCard} />
        </div>
      ))}
    </div>
  );
}

function formatDuration(ms: number): string {
  const seconds = Math.round(ms / 1000);
  const minutes = Math.floor(seconds / 60);
  const rest = seconds % 60;
  if (minutes <= 0) return `${seconds} 秒`;
  return `${minutes}:${String(rest).padStart(2, "0")}`;
}
