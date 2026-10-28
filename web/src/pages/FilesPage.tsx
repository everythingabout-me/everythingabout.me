import { Suspense, lazy, memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLoaderData, useNavigate, useParams } from "react-router";
import { ApiError, api, type AuthUser, type Crumb, type Item } from "../api/client";
import { AppHeader } from "../components/AppHeader";
import {
  ChevronRightIcon,
  CloseIcon,
  DownloadIcon,
  FileIcon,
  FolderIcon,
  GlyphFor,
  PlusIcon,
  RefreshIcon,
  SearchIcon,
  UploadIcon,
} from "../components/icons";
import { canPreviewFile } from "../components/preview/supported";
import { filesHref } from "../filesPath";
import { filesText, resultsText, t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import { useUploadQueue, type UploadStatus, type UploadTask } from "./useUploadQueue";
import styles from "./FilesPage.module.css";

/**
 * The previewer is a heavy chunk, only fetched when a file detail page actually shows a
 * preview. This import must stay at module top level so the build splits it into its own chunk.
 */
const FilePreview = lazy(() => import("../components/preview/FilePreview"));

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

/** URL paths arrive with NFD on macOS; the backend treats NFC as identity. */
function normalizePath(value: string): string {
  return value.normalize("NFC").replace(/^\/+|\/+$/g, "");
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${Math.max(0, Math.round(bytes))} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value.toFixed(value >= 100 || Number.isInteger(value) ? 0 : 1)} ${units[index]}`;
}

function formatSize(bytes: number): string {
  return bytes ? formatBytes(bytes) : "—";
}

function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return "0 B/s";
  return `${formatSize(bytesPerSecond)}/s`;
}

/** Matches the backend: directories first, the rest sorted by lowercased name. */
function compareNames(a: string, b: string): number {
  const left = a.toLowerCase();
  const right = b.toLowerCase();
  if (left === right) return a < b ? -1 : a > b ? 1 : 0;
  return left < right ? -1 : 1;
}

function compareItems(a: Item, b: Item): number {
  if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
  return compareNames(a.name, b.name);
}

type RowEntry =
  | { kind: "item"; key: string; item: Item }
  | { kind: "upload"; key: string; task: UploadTask };

function compareEntries(a: RowEntry, b: RowEntry): number {
  const aDir = a.kind === "item" && a.item.is_dir;
  const bDir = b.kind === "item" && b.item.is_dir;
  if (aDir !== bDir) return aDir ? -1 : 1;
  return compareNames(
    a.kind === "item" ? a.item.name : a.task.name,
    b.kind === "item" ? b.item.name : b.task.name,
  );
}

function uploadStatusText(status: UploadStatus): string {
  switch (status) {
    case "queued":
      return t("drive.queued");
    case "uploading":
      return t("drive.uploading");
    case "finalizing":
      return t("drive.finishing");
    case "failed":
      return t("drive.failed");
  }
}

function uploadStatusClass(status: UploadStatus): string {
  switch (status) {
    case "queued":
      return styles.statusQueued;
    case "failed":
      return styles.statusFailed;
    default:
      return styles.statusActive;
  }
}

/** The frontend names the root breadcrumb; the backend's "all files" string is API data only. */
function crumbName(crumb: Crumb): string {
  return crumb.path === "" ? t("drive.root") : crumb.name;
}

/** A local trail for the first paint (and for a path the API has no breadcrumb for yet). */
function crumbsForPath(path: string): Crumb[] {
  const trail: Crumb[] = [{ name: "", path: "" }];
  let cursor = "";
  for (const segment of path.split("/")) {
    if (!segment) continue;
    cursor = cursor ? `${cursor}/${segment}` : segment;
    trail.push({ name: segment, path: cursor });
  }
  return trail;
}

function fileTypeName(item: Item): string {
  const dot = item.name.lastIndexOf(".");
  if (dot > 0 && dot < item.name.length - 1) {
    return item.name.slice(dot + 1).toUpperCase();
  }
  return item.mime || t("file.unknownType");
}

const ItemRow = memo(function ItemRow({
  item,
  activeQuery,
}: {
  item: Item;
  activeQuery: boolean;
}) {
  return (
    // Every entry is a real URL: folders list their contents, files open their detail page.
    <Link className={styles.row} to={filesHref(item.path)} title={item.name}>
      <span className={styles.glyph}>
        <GlyphFor name={item.name} mime={item.mime} isDir={item.is_dir} />
      </span>

      <div className={styles.nameCell}>
        <span className={`${styles.name} ${item.is_dir ? styles.nameDir : ""}`}>
          <span className={styles.nameLink}>{item.name}</span>
        </span>
        {activeQuery && item.parent && <span className={styles.pathHint}>{item.parent}</span>}
      </div>

      <span className={styles.size}>{item.is_dir ? "" : formatSize(Number(item.size))}</span>
      <span className={styles.time}>{formatRelativeTime(item.updated_at)}</span>
    </Link>
  );
});

const UploadRow = memo(function UploadRow({
  task,
  onCancel,
  onRetry,
}: {
  task: UploadTask;
  onCancel: (id: string) => void;
  onRetry: (id: string) => void;
}) {
  const percent =
    task.status === "finalizing"
      ? 100
      : task.size > 0
        ? Math.min(100, Math.round((task.loaded / task.size) * 100))
        : 0;
  const showProgress = task.status === "uploading" || task.status === "finalizing";

  return (
    <div className={`${styles.row} ${styles.uploadRow}`}>
      <span className={styles.glyph}>
        <GlyphFor name={task.name} mime={task.file.type || null} isDir={false} />
      </span>

      <div className={styles.uploadInfo}>
        {/* Progress, speed and sent/total sit after the status; the row stays two lines tall. */}
        <div className={styles.uploadTitle}>
          <span className={styles.name} title={task.name}>
            {task.name}
          </span>
          <span className={`${styles.statusBadge} ${uploadStatusClass(task.status)}`}>
            {uploadStatusText(task.status)}
          </span>

          {task.status === "uploading" ? (
            <span className={styles.uploadMeta}>
              <span>{percent}%</span>
              <span className={styles.uploadSep}>·</span>
              <span>{formatSpeed(task.speed)}</span>
              <span className={styles.uploadSep}>·</span>
              <span>
                {formatBytes(Math.min(task.loaded, task.size))} / {formatSize(task.size)}
              </span>
            </span>
          ) : (
            <span className={styles.uploadMeta}>
              {task.status === "finalizing" && (
                <>
                  <span>100%</span>
                  <span className={styles.uploadSep}>·</span>
                </>
              )}
              <span>{formatSize(task.size)}</span>
              {task.status === "failed" && (
                <>
                  <span className={styles.uploadSep}>·</span>
                  <span className={styles.uploadError} title={task.error}>
                    {task.error}
                  </span>
                </>
              )}
            </span>
          )}
        </div>

        {showProgress && (
          <div
            className={styles.progressTrack}
            role="progressbar"
            aria-label={task.name}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={percent}
          >
            <div
              className={`${styles.progressFill} ${
                task.status === "finalizing" ? styles.progressFillFinishing : ""
              }`}
              style={{ width: `${percent}%` }}
            />
          </div>
        )}
      </div>

      <div className={styles.uploadActions}>
        {task.status === "failed" && (
          <button
            className={styles.textBtn}
            onClick={() => onRetry(task.id)}
            title={t("drive.retryUpload")}
          >
            {t("common.retry")}
          </button>
        )}
        {task.status !== "finalizing" && (
          <button
            className={`${styles.ghostBtn} ${styles.cancelUpload}`}
            onClick={() => onCancel(task.id)}
            title={t("drive.cancelUpload")}
            aria-label={t("drive.cancelUpload")}
          >
            <CloseIcon size={13} />
          </button>
        )}
      </div>
    </div>
  );
});

function Breadcrumbs({ crumbs }: { crumbs: Crumb[] }) {
  return (
    <nav className={styles.breadcrumbs} aria-label={t("drive.pathLabel")}>
      {crumbs.map((crumb, index) => (
        <span key={crumb.path || "root"} style={{ display: "contents" }}>
          {index > 0 && (
            <span className={styles.breadcrumbSep}>
              <ChevronRightIcon size={12} />
            </span>
          )}
          <Link className={styles.crumbLink} to={filesHref(crumb.path)}>
            {crumbName(crumb)}
          </Link>
        </span>
      ))}
    </nav>
  );
}

/** A file's own page, built like a folder page: same header, breadcrumbs and title block. */
function FileDetail({ file, trail }: { file: Item; trail: Crumb[] }) {
  const parents = trail.slice(0, -1);

  return (
    <>
      <div className={styles.masthead}>
        <div className={styles.mastheadMain}>
          {parents.length > 0 && <Breadcrumbs crumbs={parents} />}
          <h1 className={styles.pageTitle}>{file.name}</h1>
          <p className={styles.stats}>
            <span>{fileTypeName(file)}</span>
            <span className={styles.statSep}>·</span>
            <span>{formatSize(Number(file.size))}</span>
            <span className={styles.statSep}>·</span>
            <span>{formatRelativeTime(file.updated_at)}</span>
          </p>
        </div>
      </div>

      <section className={styles.viewerPanel}>
        {canPreviewFile(file.name) ? (
          <Suspense
            fallback={
              <div className={styles.viewerLoading} role="status">
                {t("preview.loading")}
              </div>
            }
          >
            <FilePreview path={file.path} name={file.name} />
          </Suspense>
        ) : (
          <div className={styles.noPreview}>
            <span className={styles.noPreviewIcon}>
              <GlyphFor name={file.name} mime={file.mime} isDir={false} />
            </span>
            <span className={styles.emptyTitle}>{t("file.unsupportedTitle")}</span>
            <span className={styles.emptyText}>{t("file.unsupportedText")}</span>
            <a
              className={styles.primaryLink}
              href={api.downloadHref(file.path)}
              target="_blank"
              rel="noreferrer"
            >
              <DownloadIcon size={14} />
              {t("file.download")}
            </a>
          </div>
        )}
      </section>

      <section className={styles.detailPanel} aria-label={t("file.details")}>
        <dl className={styles.detailList}>
          <div className={styles.detailRow}>
            <dt>{t("file.path")}</dt>
            <dd className={styles.detailPath}>{file.path}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.type")}</dt>
            <dd>{file.mime || fileTypeName(file)}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.size")}</dt>
            <dd>{formatSize(Number(file.size))}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.modified")}</dt>
            <dd>{formatRelativeTime(file.updated_at)}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.created")}</dt>
            <dd>{file.created_at ? formatRelativeTime(file.created_at) : "—"}</dd>
          </div>
        </dl>
      </section>
    </>
  );
}

export function FilesPage() {
  const { user } = useLoaderData() as { user: AuthUser };
  const params = useParams();
  const navigate = useNavigate();
  const path = normalizePath(params["*"] ?? "");

  const [items, setItems] = useState<Item[]>([]);
  const [current, setCurrent] = useState<Item | null>(null);
  const [crumbTrail, setCrumbTrail] = useState<Crumb[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadedPath, setLoadedPath] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const [activeQuery, setActiveQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const [newName, setNewName] = useState("");
  const [toast, setToast] = useState<{ text: string; kind: "info" | "error" } | null>(null);
  const [dragging, setDragging] = useState(false);

  const fileInput = useRef<HTMLInputElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const toastTimer = useRef<number | null>(null);
  const dragDepth = useRef(0);
  const completedUploads = useRef(0);
  // Every list / search request gets a ticket; a response older than the newest ticket is dropped.
  const requestId = useRef(0);
  // The upload callback is async: by completion the user may have moved to another directory.
  const listingRef = useRef({ path, searching: false });
  listingRef.current = { path, searching: Boolean(activeQuery) };

  const showToast = useCallback(
    (text: string, kind: "info" | "error" = "info", sticky = false) => {
      setToast({ text, kind });
      if (toastTimer.current) window.clearTimeout(toastTimer.current);
      if (!sticky) toastTimer.current = window.setTimeout(() => setToast(null), 2600);
    },
    [],
  );

  const load = useCallback(
    async (target: string) => {
      const request = ++requestId.current;
      setLoading(true);
      setLoadError(null);
      try {
        const data = await api.list(target);
        if (requestId.current !== request) return;
        setItems(data.items);
        setCurrent(data.node);
        setCrumbTrail(data.breadcrumb);
        setActiveQuery("");
        setQuery("");
        setLoadedPath(target);
      } catch (error) {
        if (requestId.current !== request) return;
        if (error instanceof ApiError && error.status === 401) {
          navigate("/login", { replace: true });
          return;
        }
        setLoadError(error instanceof Error ? error.message : t("error.loadFailed"));
        setLoadedPath(target);
      } finally {
        if (requestId.current === request) setLoading(false);
      }
    },
    [navigate],
  );

  // The path is the URL (/files/Photos/2024), so back/forward and direct visits open the right place.
  useEffect(() => {
    void load(path);
  }, [path, load]);

  /** Once a file's confirm succeeds, swap its placeholder row for the returned node. */
  const handleUploaded = useCallback((task: UploadTask, item: Item) => {
    completedUploads.current += 1;
    if (listingRef.current.path !== task.dir || listingRef.current.searching) return;
    setItems((currentItems) => {
      const next = currentItems.filter((existing) => existing.path !== item.path);
      next.push(item);
      next.sort(compareItems);
      return next;
    });
  }, []);

  const {
    tasks: uploads,
    enqueue,
    cancel: cancelUpload,
    retry: retryUpload,
  } = useUploadQueue(handleUploaded);

  // Summarize once the whole batch is done; the placeholder rows may sit far down the list.
  useEffect(() => {
    if (uploads.length > 0 || completedUploads.current === 0) return;
    const count = completedUploads.current;
    completedUploads.current = 0;
    showToast(
      count === 1
        ? t("drive.uploadCompleted")
        : t("drive.uploadCompletedMany", { files: filesText(count) }),
    );
  }, [uploads.length, showToast]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setCreating(false);
        setToast(null);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  // "/" focuses the page's search box, except while typing in a field.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target?.tagName === "INPUT" || target?.tagName === "TEXTAREA" || target?.isContentEditable;
      if (event.key === "/" && !typing) {
        event.preventDefault();
        searchInput.current?.focus();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  const runSearch = async (keyword: string) => {
    const value = keyword.trim();
    if (!value) {
      void load(path);
      return;
    }
    const request = ++requestId.current;
    setLoading(true);
    setLoadError(null);
    try {
      const data = await api.search(value);
      if (requestId.current !== request) return;
      setItems(data.items);
      setActiveQuery(value);
      // Search results stand in for the folder listing; mark the path settled so the skeleton
      // does not stay up if the search won a race against the first load.
      setCrumbTrail(crumbsForPath(path));
      setLoadedPath(path);
    } catch (error) {
      if (requestId.current !== request) return;
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      showToast(error instanceof Error ? error.message : t("error.searchFailed"), "error");
      // A failed search should fall back to the folder, not to an endless skeleton.
      void load(path);
    } finally {
      if (requestId.current === request) setLoading(false);
    }
  };

  const itemExists = useCallback(
    (target: string) => items.some((item) => item.path === target),
    [items],
  );

  const createFolder = async () => {
    const name = newName.trim().normalize("NFC");
    if (!name) {
      setCreating(false);
      return;
    }
    try {
      await api.mkdir(joinPath(path, name));
      setCreating(false);
      setNewName("");
      await load(path);
    } catch (error) {
      showToast(error instanceof Error ? error.message : t("error.saveFailed"), "error");
    }
  };

  // In-flight placeholder rows share one list with real files and follow the same ordering:
  // when an upload finishes, the real file lands where its placeholder was and nothing jumps.
  const { entries, visibleUploads } = useMemo(() => {
    const uploading = uploads.filter((task) => task.dir === path);
    const pendingPaths = new Set(uploading.map((task) => task.path));
    // When overwriting a same-named file, hide the old one and keep only the placeholder row;
    // directories are unaffected.
    const baseItems = items.filter((item) => item.is_dir || !pendingPaths.has(item.path));
    const list: RowEntry[] = [
      ...baseItems.map<RowEntry>((item) => ({
        kind: "item",
        key: `item:${item.id}`,
        item,
      })),
      ...uploading.map<RowEntry>((task) => ({
        kind: "upload",
        key: `upload:${task.id}`,
        task,
      })),
    ];
    if (!activeQuery) list.sort(compareEntries);
    return { entries: list, visibleUploads: uploading };
  }, [activeQuery, items, path, uploads]);

  const activeUploadCount = visibleUploads.filter((task) => task.status !== "failed").length;

  // Before the first listing arrives, build the trail from the URL so the title and breadcrumbs
  // do not flash "all files" on a deep link.
  const trail = crumbTrail.length > 0 && loadedPath === path ? crumbTrail : crumbsForPath(path);
  const currentCrumb = trail[trail.length - 1];
  const parentCrumbs = trail.slice(0, -1);
  const currentName = currentCrumb ? crumbName(currentCrumb) : t("drive.root");

  const pending = loading || loadedPath !== path;
  const file = !activeQuery && current && !current.is_dir ? current : null;
  const notFound = !pending && !loadError && !activeQuery && !file && current === null && path !== "";
  // The current path may be a file even while search results are on screen; never upload to it.
  const canUpload =
    !pending && !loadError && !file && !notFound && !(current !== null && !current.is_dir);

  const onDrop = (event: React.DragEvent) => {
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    enqueue(event.dataTransfer.files, path, itemExists);
  };

  return (
    <div
      className={styles.page}
      onDragEnter={
        canUpload
          ? (event) => {
              if (!event.dataTransfer.types.includes("Files")) return;
              dragDepth.current += 1;
              setDragging(true);
            }
          : undefined
      }
      onDragOver={canUpload ? (event) => event.preventDefault() : undefined}
      onDragLeave={
        canUpload
          ? () => {
              dragDepth.current -= 1;
              if (dragDepth.current <= 0) setDragging(false);
            }
          : undefined
      }
      onDrop={canUpload ? onDrop : undefined}
    >
      <title>{`${file ? file.name : currentName} · ${t("app.name")}`}</title>

      <AppHeader user={user} />

      <input
        ref={fileInput}
        type="file"
        multiple
        hidden
        onChange={(event) => {
          const files = Array.from(event.target.files ?? []);
          event.target.value = "";
          enqueue(files, path, itemExists);
        }}
      />

      <main className={styles.main}>
        {/* Search and the primary action belong to this page, not to the shared header. */}
        <div className={styles.tools}>
          <div className={styles.search}>
            <span className={styles.searchIcon}>
              <SearchIcon size={14} />
            </span>
            <input
              ref={searchInput}
              className={styles.searchInput}
              placeholder={t("drive.searchPlaceholder")}
              aria-label={t("drive.searchPlaceholder")}
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                if (!event.target.value && activeQuery) void load(path);
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter") void runSearch(query);
                if (event.key === "Escape") {
                  setQuery("");
                  if (activeQuery) void load(path);
                  event.currentTarget.blur();
                }
              }}
            />
            <span className={styles.kbd}>/</span>
          </div>

          <div className={styles.toolActions}>
            {file && (
              <a
                className={styles.secondaryBtn}
                href={api.downloadHref(file.path)}
                target="_blank"
                rel="noreferrer"
              >
                <DownloadIcon size={14} />
                {t("file.download")}
              </a>
            )}

            {!file && !notFound && (
              <>
                <button
                  className={styles.secondaryBtn}
                  onClick={() => {
                    setCreating(true);
                    setNewName("");
                  }}
                  disabled={Boolean(activeQuery)}
                >
                  <PlusIcon size={14} />
                  {t("drive.newFolder")}
                </button>
                <button
                  className={styles.ghostBtn}
                  onClick={() => void load(path)}
                  title={t("common.refresh")}
                  aria-label={t("common.refresh")}
                >
                  <RefreshIcon size={16} />
                </button>
              </>
            )}

            {canUpload && (
              <button
                className={styles.uploadBtn}
                onClick={() => fileInput.current?.click()}
                title={t("drive.upload")}
              >
                <UploadIcon size={14} />
                <span className={styles.uploadLabel}>{t("drive.upload")}</span>
                {activeUploadCount > 0 && (
                  <span className={styles.uploadBadge}>{activeUploadCount}</span>
                )}
              </button>
            )}
          </div>
        </div>

        {loadError ? (
          <section className={styles.panel}>
            <div className={styles.empty}>
              <span className={styles.emptyTitle}>{t("error.loadFailed")}</span>
              <span className={styles.emptyText}>{loadError}</span>
              <button className={styles.secondaryBtn} onClick={() => void load(path)}>
                {t("common.retry")}
              </button>
            </div>
          </section>
        ) : file ? (
          <FileDetail file={file} trail={trail} />
        ) : notFound ? (
          <>
            <div className={styles.masthead}>
              <div className={styles.mastheadMain}>
                {parentCrumbs.length > 0 && <Breadcrumbs crumbs={parentCrumbs} />}
                <h1 className={styles.pageTitle}>{currentName}</h1>
              </div>
            </div>
            <section className={styles.panel}>
              <div className={styles.empty}>
                <span className={styles.emptyIcon}>
                  <FileIcon size={20} />
                </span>
                <span className={styles.emptyTitle}>{t("file.notFoundTitle")}</span>
                <span className={styles.emptyText}>{t("file.notFoundText")}</span>
                <Link className={styles.secondaryBtn} to="/files">
                  {t("file.backToRoot")}
                </Link>
              </div>
            </section>
          </>
        ) : (
          <>
            <div className={styles.masthead}>
              <div className={styles.mastheadMain}>
                {activeQuery ? (
                  <>
                    <h1 className={styles.pageTitle}>{t("drive.results")}</h1>
                    <p className={styles.stats}>
                      {t("drive.resultsFor", {
                        query: activeQuery,
                        results: resultsText(items.length),
                      })}
                      <button className={styles.clearLink} onClick={() => void load(path)}>
                        {t("drive.clear")}
                      </button>
                    </p>
                  </>
                ) : (
                  <>
                    {parentCrumbs.length > 0 && <Breadcrumbs crumbs={parentCrumbs} />}
                    <h1 className={styles.pageTitle}>{currentName}</h1>
                  </>
                )}
              </div>
            </div>

            <section className={styles.panel} aria-busy={pending}>
              {creating && (
                <div className={styles.newRow}>
                  <input
                    className={styles.newInput}
                    autoFocus
                    placeholder={t("drive.newFolderPlaceholder")}
                    value={newName}
                    onChange={(event) => setNewName(event.target.value)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter") void createFolder();
                      if (event.key === "Escape") setCreating(false);
                    }}
                  />
                  <button className={styles.textBtn} onClick={() => void createFolder()}>
                    {t("drive.create")}
                  </button>
                  <button className={styles.textBtn} onClick={() => setCreating(false)}>
                    {t("common.cancel")}
                  </button>
                </div>
              )}

              {pending && (
                <>
                  {visibleUploads.map((task) => (
                    <UploadRow
                      key={task.id}
                      task={task}
                      onCancel={cancelUpload}
                      onRetry={retryUpload}
                    />
                  ))}
                  {Array.from({ length: 4 }).map((_, index) => (
                    <div key={index} className={styles.skeletonRow}>
                      <div
                        className={styles.skBar}
                        style={{ width: 16, height: 16, borderRadius: 4 }}
                      />
                      <div className={styles.skBar} style={{ width: `${38 + index * 11}%` }} />
                      <div className={styles.skBar} />
                      <div className={styles.skBar} />
                    </div>
                  ))}
                </>
              )}

              {!pending &&
                entries.map((entry) =>
                  entry.kind === "upload" ? (
                    <UploadRow
                      key={entry.key}
                      task={entry.task}
                      onCancel={cancelUpload}
                      onRetry={retryUpload}
                    />
                  ) : (
                    <ItemRow
                      key={entry.key}
                      item={entry.item}
                      activeQuery={Boolean(activeQuery)}
                    />
                  ),
                )}

              {!pending && entries.length === 0 && (
                <div className={styles.empty}>
                  <span className={styles.emptyIcon}>
                    {activeQuery ? <SearchIcon size={20} /> : <FolderIcon size={20} />}
                  </span>
                  <span className={styles.emptyTitle}>
                    {activeQuery ? t("drive.emptySearchTitle") : t("drive.emptyFolderTitle")}
                  </span>
                  {activeQuery && (
                    <span className={styles.emptyText}>{t("drive.emptySearchHint")}</span>
                  )}
                </div>
              )}
            </section>
          </>
        )}
      </main>

      {canUpload && dragging && (
        <div className={styles.dropOverlay}>
          <div className={styles.dropInner}>
            <UploadIcon size={20} />
            <span>{t("drive.dropToUpload", { target: currentName })}</span>
          </div>
        </div>
      )}

      {toast && (
        <div className={`${styles.toast} ${toast.kind === "error" ? styles.toastError : ""}`}>
          <span>{toast.text}</span>
          {toast.kind === "error" && (
            <button
              className={styles.ghostBtn}
              style={{ width: 20, height: 20, color: "inherit" }}
              onClick={() => setToast(null)}
              aria-label={t("common.close")}
            >
              <CloseIcon size={12} />
            </button>
          )}
        </div>
      )}
    </div>
  );
}
