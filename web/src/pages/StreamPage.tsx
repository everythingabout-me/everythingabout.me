import { useCallback, useEffect, useRef, useState } from "react";
import { Link, useLoaderData, useNavigate } from "react-router";
import {
  ApiError,
  api,
  type AuthUser,
  type EventMedia,
  type EventMetric,
  type TimelineEvent,
  type StreamStatus,
} from "../api/client";
import { BrandMark } from "../components/BrandMark";
import { RefreshIcon } from "../components/icons";
import { filesText, t } from "../i18n";
import { formatEventDate } from "../i18n/format";
import styles from "./StreamPage.module.css";

/**
 * 时间线：一层扁平的事件流。
 *
 * 设计见 docs/design/timeline.md §17 和 docs/design/events.md。三条硬规则：
 *   1. 没有年 / 月标题，也没有「N 个文件 / N 个事件」——每张卡自带完整日期；
 *   2. 卡片按 type 渲染：photo / note / metric 用我们的版式，html 是应用自己的，
 *      跑在 sandbox iframe 里（脚本禁掉 + 注入设计 token）；
 *   3. 没有编辑，也没有「查看详情」按钮：有 detail_url 时只有标题是链接。
 */
export function StreamPage() {
  const { user } = useLoaderData() as { user: AuthUser };
  const navigate = useNavigate();

  const [events, setEvents] = useState<TimelineEvent[]>([]);
  const [status, setStatus] = useState<StreamStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const nextCursor = useRef<string | undefined>(undefined);
  const loadingMoreRef = useRef(false);
  const sentinel = useRef<HTMLDivElement>(null);
  const wasPending = useRef(false);

  const merge = useCallback((current: TimelineEvent[], incoming: TimelineEvent[]) => {
    const seen = new Set(current.map((event) => event.id));
    const merged = [...current];
    for (const event of incoming) {
      if (!seen.has(event.id)) {
        seen.add(event.id);
        merged.push(event);
      }
    }
    return merged;
  }, []);

  const loadInitial = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const page = await api.events(undefined, 20);
      setEvents(page.events);
      nextCursor.current = page.next;
      setHasMore(Boolean(page.next));
      setStatus(page.status);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      setError(cause instanceof Error ? cause.message : "failed to load");
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
      const page = await api.events(cursor, 20);
      setEvents((current) => merge(current, page.events));
      nextCursor.current = page.next;
      setHasMore(Boolean(page.next));
      setStatus(page.status);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      setError(cause instanceof Error ? cause.message : "failed to load");
    } finally {
      loadingMoreRef.current = false;
      setLoadingMore(false);
    }
  }, [merge, navigate]);

  useEffect(() => {
    void loadInitial();
  }, [loadInitial]);

  // 整理进行中时轮询状态；结束时自动刷新一次，让新事件出现。
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

  const empty = !loading && events.length === 0;
  const indexing = Boolean(status?.pending && status.pending > 0);

  return (
    <div className={styles.page}>
      <title>{t("app.name")}</title>

      <header className={styles.topbar}>
        <div className={styles.topbarInner}>
          <Link to="/" className={styles.brand}>
            <BrandMark className={styles.brandMark} />
            <span className={styles.brandText}>{t("app.name")}</span>
          </Link>

          <nav className={styles.nav}>
            <Link className={styles.navLink} to="/drive">
              {t("nav.files")}
            </Link>
          </nav>

          <div className={styles.spacer} />

          <Link
            className={styles.account}
            to="/account"
            title={t("nav.accountSettings")}
            aria-label={t("nav.accountSettingsFor", { email: user.email })}
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
            <span>{t("stream.working")}</span>
            <span className={styles.workingMeta}>
              {t("stream.workingMeta", { files: filesText(status?.indexed_files ?? 0) })}
            </span>
          </div>
        )}

        {loading && <LoadingSkeleton />}

        {!loading && error && (
          <div className={styles.errorBox}>
            <p>{error}</p>
            <button className={styles.textButton} onClick={() => void loadInitial()}>
              <RefreshIcon size={13} /> {t("common.retry")}
            </button>
          </div>
        )}

        {!loading && !error && empty && (
          <div className={styles.empty}>
            <span className={styles.emptyTitle}>
              {indexing ? t("stream.emptyIndexingTitle") : t("stream.emptyTitle")}
            </span>
            <span className={styles.emptyText}>
              {indexing ? t("stream.emptyIndexingText") : t("stream.emptyText")}
            </span>
            {!indexing && (
              <Link className={styles.emptyAction} to="/drive">
                {t("stream.emptyAction")}
              </Link>
            )}
          </div>
        )}

        {!loading && !error && !empty && (
          <div className={styles.feed}>
            {events.map((event) => (
              <EventCard key={event.id} event={event} />
            ))}
          </div>
        )}

        <div ref={sentinel} className={styles.sentinel} aria-hidden="true" />

        {loadingMore && (
          <div className={styles.moreLoading}>
            <span className={styles.dot} />
            {t("stream.loadingMore")}
          </div>
        )}
        {!loading && !loadingMore && !hasMore && !empty && !error && (
          <div className={styles.end}>{t("stream.end")}</div>
        )}
      </main>
    </div>
  );
}

function EventCard({ event }: { event: TimelineEvent }) {
  const date = formatEventDate(event.local_date);
  const source = event.source.kind === "app" ? event.source.name : undefined;

  return (
    <article className={styles.ev}>
      <div className={styles.kicker}>
        <b>{date}</b>
        {source && (
          <>
            <span aria-hidden="true">·</span>
            <span className={styles.source}>
              {source}
              {event.source.disconnected ? " (disconnected)" : ""}
            </span>
          </>
        )}
      </div>

      {event.type === "html" ? (
        <HtmlCard event={event} />
      ) : event.type === "note" ? (
        <div className={styles.note}>
          <h3 className={styles.title}>
            {event.detail_url ? (
              <a href={event.detail_url} target="_blank" rel="noreferrer">
                {event.title}
              </a>
            ) : (
              event.title
            )}
          </h3>
          {event.payload.body ? <p className={styles.body}>{event.payload.body}</p> : null}
          {event.payload.meta ? <div className={styles.metaLine}>{event.payload.meta}</div> : null}
        </div>
      ) : (
        <>
          <h3 className={styles.title}>
            {event.detail_url ? (
              <a href={event.detail_url} target="_blank" rel="noreferrer">
                {event.title}
              </a>
            ) : (
              event.title
            )}
          </h3>
          <Media media={event.payload.media ?? []} />
          {event.payload.body ? <p className={styles.body}>{event.payload.body}</p> : null}
          <Metrics metrics={event.payload.metrics ?? []} />
          {event.payload.meta ? <div className={styles.metaLine}>{event.payload.meta}</div> : null}
        </>
      )}
    </article>
  );
}

/**
 * HtmlCard 把三方应用的 HTML 放在 sandbox iframe 里渲染。
 *
 * - sandbox 不带 allow-scripts：脚本、表单、顶层跳转全部禁掉；
 * - 文档里只注入一组设计 token（--eam-*），卡片可以跟随明暗主题，也可以完全不用；
 * - 高度由 payload 给，服务端 clamp 过（40–640）。
 *
 * 不要把它改成 dangerouslySetInnerHTML：样式、事件、暗色模式会一起漏进来。
 */
function HtmlCard({ event }: { event: TimelineEvent }) {
  const tokens = useCardTokens();
  const height = Math.min(640, Math.max(40, event.payload.height ?? 240));
  const doc = `<!doctype html><meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data: https:; style-src 'unsafe-inline'; font-src data:">
<style>${tokens}
html,body{margin:0;height:100%;background:transparent;color:var(--eam-text);font:13px/1.55 -apple-system,BlinkMacSystemFont,"PingFang SC","Microsoft YaHei",sans-serif;-webkit-font-smoothing:antialiased}
*{box-sizing:border-box}
</style>${event.payload.html ?? ""}`;

  return (
    <>
      <h3 className={styles.srOnly}>{event.title}</h3>
      <iframe
        className={styles.htmlCard}
        sandbox=""
        referrerPolicy="no-referrer"
        title={event.title}
        srcDoc={doc}
        style={{ height }}
      />
    </>
  );
}

function useCardTokens(): string {
  const [tokens, setTokens] = useState("");
  useEffect(() => {
    const computed = getComputedStyle(document.documentElement);
    const value = (name: string) => computed.getPropertyValue(name).trim();
    const dark = window.matchMedia?.("(prefers-color-scheme: dark)").matches ?? false;
    setTokens(
      `:root{color-scheme:${dark ? "dark" : "light"};` +
        `--eam-bg:${value("--bg")};--eam-surface:${value("--surface")};` +
        `--eam-text:${value("--text")};--eam-text2:${value("--text-2")};` +
        `--eam-text3:${value("--text-3")};--eam-line:${value("--border")};` +
        `--eam-accent:${value("--blue")}}`,
    );
  }, []);
  return tokens;
}

function Media({ media }: { media: EventMedia[] }) {
  const items = media.slice(0, 4);
  if (items.length === 0) return null;
  const layout =
    items.length === 1
      ? styles.mediaOne
      : items.length === 2
        ? styles.mediaTwo
        : items.length === 3
          ? styles.mediaThree
          : styles.mediaFour;
  return (
    <div className={`${styles.media} ${layout}`}>
      {items.map((item) => (
        <Thumb key={item.path} media={item} />
      ))}
    </div>
  );
}

function Thumb({ media }: { media: EventMedia }) {
  const [failed, setFailed] = useState(false);

  if (media.video) {
    return (
      <div className={`${styles.tile} ${styles.tileVideo}`}>
        <span className={styles.playIcon} aria-hidden="true" />
      </div>
    );
  }
  if (failed || !media.url) return <div className={`${styles.tile} ${styles.tileFallback}`} aria-hidden="true" />;
  return (
    <img
      className={styles.tile}
      src={media.url}
      alt={media.name ?? ""}
      loading="lazy"
      decoding="async"
      onError={() => setFailed(true)}
    />
  );
}

function Metrics({ metrics }: { metrics: EventMetric[] }) {
  if (metrics.length === 0) return null;
  return (
    <div className={styles.stats}>
      {metrics.map((metric) => (
        <span key={metric.label}>
          <em>
            {metric.value}
            {metric.unit ? <s>{metric.unit}</s> : null}
          </em>
          <i>{metric.label}</i>
        </span>
      ))}
    </div>
  );
}

function LoadingSkeleton() {
  return (
    <div className={styles.feed} aria-busy="true">
      {[0, 1].map((index) => (
        <div className={styles.ev} key={index}>
          <span className={styles.skKicker} />
          <span className={styles.skTitle} />
          <div className={styles.skMedia} />
        </div>
      ))}
    </div>
  );
}
