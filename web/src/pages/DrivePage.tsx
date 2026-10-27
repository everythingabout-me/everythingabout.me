import { memo, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useLoaderData, useNavigate, useSearchParams } from "react-router";
import { ApiError, api, type AuthUser, type Crumb, type Item } from "../api/client";
import {
  ChevronRightIcon,
  CloseIcon,
  FolderIcon,
  GlyphFor,
  PlusIcon,
  RefreshIcon,
  SearchIcon,
  UploadIcon,
} from "../components/icons";
import { BrandMark } from "../components/BrandMark";
import { filesText, foldersText, resultsText, t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import { useUploadQueue, type UploadStatus, type UploadTask } from "./useUploadQueue";
import styles from "./DrivePage.module.css";

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
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

/** 和后端一致：目录在前，其余按名字小写排序。 */
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

const ItemRow = memo(function ItemRow({
  item,
  activeQuery,
  onOpenDir,
}: {
  item: Item;
  activeQuery: boolean;
  onOpenDir: (path: string) => void;
}) {
  return (
    <div className={styles.row}>
      <span className={styles.glyph}>
        <GlyphFor name={item.name} mime={item.mime} isDir={item.is_dir} />
      </span>

      <div className={styles.nameCell}>
        {item.is_dir ? (
          <button
            className={`${styles.name} ${styles.nameDir}`}
            onClick={() => onOpenDir(item.path)}
            title={item.name}
          >
            {item.name}
          </button>
        ) : (
          <a
            className={styles.name}
            href={api.downloadHref(item.path)}
            target="_blank"
            rel="noreferrer"
            title={item.name}
          >
            <span className={styles.nameLink}>{item.name}</span>
          </a>
        )}
        {activeQuery && item.parent && <span className={styles.pathHint}>{item.parent}</span>}
      </div>

      <span className={styles.size}>{item.is_dir ? "" : formatSize(Number(item.size))}</span>
      <span className={styles.time}>{formatRelativeTime(item.updated_at)}</span>
    </div>
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
        {/* 进度、速度、已传/总量都挤在状态后面，整行只占两行高度。 */}
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


/** 根面包屑由前端命名，后端返回的「全部文件」只作为 API 数据。 */
function crumbName(crumb: Crumb): string {
  return crumb.path === "" ? t("drive.root") : crumb.name;
}

export function DrivePage() {
  const { user } = useLoaderData() as { user: AuthUser };
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const path = searchParams.get("path") ?? "";

  const [items, setItems] = useState<Item[]>([]);
  const [crumbTrail, setCrumbTrail] = useState<Crumb[]>([]);
  const [loading, setLoading] = useState(true);
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
  // 上传回调是异步的，完成时可能已经切到别的目录或搜索结果里了。
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
      setLoading(true);
      try {
        const data = await api.list(target);
        setItems(data.items);
        setCrumbTrail(data.breadcrumb);
        setActiveQuery("");
        setQuery("");
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) {
          navigate("/login", { replace: true });
          return;
        }
        showToast(error instanceof Error ? error.message : t("error.loadFailed"), "error");
      } finally {
        setLoading(false);
      }
    },
    [navigate, showToast],
  );

  // 路径来自 URL（?path=），前进/后退和直接访问都能正确打开目录
  useEffect(() => {
    void load(path);
  }, [path, load]);

  /** 一个文件 confirm 成功后，用服务端返回的节点直接替换掉占位行。 */
  const handleUploaded = useCallback((task: UploadTask, item: Item) => {
    completedUploads.current += 1;
    if (listingRef.current.path !== task.dir || listingRef.current.searching) return;
    setItems((current) => {
      const next = current.filter((existing) => existing.path !== item.path);
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

  // 整批传完后给一句总结，占位行可能在列表很靠下的位置。
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

  const goTo = useCallback(
    (target: string) => {
      setActiveQuery("");
      setSearchParams(target ? { path: target } : {}, { replace: true });
    },
    [setSearchParams],
  );

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target?.tagName === "INPUT" || target?.tagName === "TEXTAREA" || target?.isContentEditable;
      if (event.key === "/" && !typing) {
        event.preventDefault();
        searchInput.current?.focus();
      }
      if (event.key === "Escape") {
        setCreating(false);
        setToast(null);
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
    setLoading(true);
    try {
      const data = await api.search(value);
      setItems(data.items);
      setActiveQuery(value);
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      showToast(error instanceof Error ? error.message : t("error.searchFailed"), "error");
    } finally {
      setLoading(false);
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

  // 上传中的占位行和真文件放同一条列表里，按同样的规则排序：
  // 传完之后真文件正好落在占位行原来的位置，不会跳。
  const { entries, visibleUploads, visibleItems } = useMemo(() => {
    const uploading = uploads.filter((task) => task.dir === path);
    const pendingPaths = new Set(uploading.map((task) => task.path));
    // 覆盖同名文件时，旧文件先藏起来，只留占位行；目录不受影响。
    const baseItems = items.filter(
      (item) => item.is_dir || !pendingPaths.has(item.path),
    );
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
    return { entries: list, visibleUploads: uploading, visibleItems: baseItems };
  }, [activeQuery, items, path, uploads]);

  const totalSize = visibleItems.reduce(
    (sum, item) => (item.is_dir ? sum : sum + Number(item.size)),
    0,
  );
  const fileCount = visibleItems.filter((item) => !item.is_dir).length;
  const folderCount = visibleItems.filter((item) => item.is_dir).length;
  const activeUploadCount = visibleUploads.filter((task) => task.status !== "failed").length;
  const currentCrumb = crumbTrail[crumbTrail.length - 1];
  const parentCrumbs = crumbTrail.slice(0, -1);
  const currentName = currentCrumb ? crumbName(currentCrumb) : t("drive.root");

  const onDrop = (event: React.DragEvent) => {
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    enqueue(event.dataTransfer.files, path, itemExists);
  };

  return (
    <div
      className={styles.page}
      onDragEnter={(event) => {
        if (!event.dataTransfer.types.includes("Files")) return;
        dragDepth.current += 1;
        setDragging(true);
      }}
      onDragOver={(event) => event.preventDefault()}
      onDragLeave={() => {
        dragDepth.current -= 1;
        if (dragDepth.current <= 0) setDragging(false);
      }}
      onDrop={onDrop}
    >
      <title>{t("app.name")}</title>
      <header className={styles.topbar}>
        <div className={styles.topbarInner}>
          <Link className={styles.brand} to="/" title={t("nav.home")}>
            <BrandMark className={styles.brandMark} />
            <span className={styles.brandText}>{t("app.name")}</span>
          </Link>

          <div className={styles.topSpacer} />

          <div className={styles.search}>
            <span className={styles.searchIcon}>
              <SearchIcon size={14} />
            </span>
            <input
              ref={searchInput}
              className={styles.searchInput}
              placeholder={t("drive.searchPlaceholder")}
              value={query}
              onChange={(event) => {
                setQuery(event.target.value);
                if (!event.target.value && activeQuery) void load(path);
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter") void runSearch(query);
                if (event.key === "Escape") {
                  setQuery("");
                  (event.target as HTMLInputElement).blur();
                }
              }}
            />
            <span className={styles.kbd}>/</span>
          </div>

          <Link
            className={styles.accountBtn}
            to="/account"
            title={t("nav.accountSettings")}
            aria-label={t("nav.accountSettingsFor", { email: user.email })}
          >
            <span className={styles.avatar}>{user.email.slice(0, 1).toUpperCase()}</span>
            <span className={styles.accountEmail}>{user.email}</span>
          </Link>

          <button className={styles.primaryBtn} onClick={() => fileInput.current?.click()}>
            <UploadIcon size={14} />
            {t("drive.upload")}
            {activeUploadCount > 0 && (
              <span className={styles.uploadBadge}>{activeUploadCount}</span>
            )}
          </button>

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
        </div>
      </header>

      <main className={styles.main}>
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
                {parentCrumbs.length > 0 && (
                  <nav className={styles.breadcrumbs} aria-label={t("drive.pathLabel")}>
                    {parentCrumbs.map((crumb, index) => (
                      <span key={crumb.path || "root"} style={{ display: "contents" }}>
                        {index > 0 && (
                          <span className={styles.breadcrumbSep}>
                            <ChevronRightIcon size={12} />
                          </span>
                        )}
                        <button className={styles.crumbLink} onClick={() => goTo(crumb.path)}>
                          {crumbName(crumb)}
                        </button>
                      </span>
                    ))}
                  </nav>
                )}
                <h1 className={styles.pageTitle}>{currentName}</h1>
                <p className={styles.stats}>
                  {foldersText(folderCount)} · {filesText(fileCount)}
                  {totalSize > 0 && <span> · {formatSize(totalSize)}</span>}
                </p>
              </>
            )}
          </div>

          <div className={styles.mastheadActions}>
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
          </div>
        </div>

        <section className={styles.panel} aria-busy={loading}>
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

          {loading && (
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
                  <div className={styles.skBar} style={{ width: 16, height: 16, borderRadius: 4 }} />
                  <div className={styles.skBar} style={{ width: `${38 + index * 11}%` }} />
                  <div className={styles.skBar} />
                  <div className={styles.skBar} />
                </div>
              ))}
            </>
          )}

          {!loading &&
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
                  onOpenDir={goTo}
                />
              ),
            )}

          {!loading && entries.length === 0 && (
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

        {!loading && visibleItems.length > 0 && (fileCount > 0 || totalSize > 0) && (
          <footer className={styles.footer}>
            {fileCount > 0 && <span>{filesText(fileCount)}</span>}
            {fileCount > 0 && totalSize > 0 && <span>·</span>}
            {totalSize > 0 && <span>{formatSize(totalSize)}</span>}
          </footer>
        )}
      </main>

      {dragging && (
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
