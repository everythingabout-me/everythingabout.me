import {
  Suspense,
  lazy,
  memo,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { Link, useLoaderData, useNavigate, useParams } from "react-router";
import { ApiError, api, type AuthUser, type Crumb, type Item, type Job } from "../api/client";
import { AppHeader } from "../components/AppHeader";
import {
  ChevronDownIcon,
  ChevronRightIcon,
  CloseIcon,
  DownloadIcon,
  FileIcon,
  FolderIcon,
  GlyphFor,
  PencilIcon,
  PlusIcon,
  SearchIcon,
  TrashIcon,
  UploadIcon,
} from "../components/icons";
import { canPreviewFile } from "../components/preview/supported";
import { filesHref } from "../filesPath";
import { resultsText, t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import { useUploadQueue, type UploadStatus, type UploadTask } from "./useUploadQueue";
import { droppedUploadEntries, filesToUploadEntries } from "./uploadSelection";
import { buildUploadRows, isUploadDirectory, type UploadRowEntry } from "./uploadRows";
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
    case "completed":
      return t("drive.uploadCompleted");
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
  entry,
  onCancel,
  onRetry,
}: {
  entry: UploadRowEntry;
  onCancel: (id: string) => void;
  onRetry: (id: string) => void;
}) {
  const { upload, task, item, name, path, isDir } = entry;
  if (!upload) return null;
  const completed = upload.status === "completed";
  const navigable = isDir || completed;
  const showProgress = !completed && (!isDir || upload.files > 0);
  const content = (
    <>
      <span className={styles.glyph}>
        <GlyphFor name={name} mime={item?.mime || task?.file?.type || null} isDir={isDir} />
      </span>
      <div className={styles.uploadInfo}>
        <div className={styles.uploadTitle}>
          <span className={`${styles.name} ${isDir ? styles.nameDir : ""}`} title={name}>
            {name}
          </span>
          {!completed && (
            <span className={`${styles.statusBadge} ${uploadStatusClass(upload.status)}`}>
              {upload.failed > 0
                ? t("drive.uploadFailedCount", { count: upload.failed.toLocaleString() })
                : uploadStatusText(upload.status)}
            </span>
          )}
        </div>
        <div className={styles.uploadMeta}>
          {isDir ? (
            <>
              <span>{t("drive.uploadFileCount", { count: upload.files.toLocaleString() })}</span>
              <span className={styles.uploadSep}>·</span>
              <span>{t("drive.uploadedFileCount", { count: upload.completedFiles.toLocaleString() })}</span>
            </>
          ) : (
            <>
              <span>{formatBytes(upload.size)}</span>
              {upload.status === "uploading" && (
                <>
                  <span className={styles.uploadSep}>·</span>
                  <span>{formatSpeed(upload.speed)}</span>
                </>
              )}
              {task?.error && (
                <>
                  <span className={styles.uploadSep}>·</span>
                  <span className={styles.uploadError} title={task.error}>{task.error}</span>
                </>
              )}
            </>
          )}
        </div>
      </div>
      {showProgress ? (
        <div className={styles.uploadProgress}>
          <div className={styles.uploadProgressLabel}>
            <span>{formatBytes(upload.loaded)} / {formatBytes(upload.size)}</span>
            <span className={styles.uploadPercent}>{upload.percent}%</span>
          </div>
          <div
            className={styles.progressTrack}
            role="progressbar"
            aria-label={t("drive.uploadProgressLabel", { name })}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={upload.percent}
            aria-valuetext={t("drive.uploadProgressValue", {
              percent: upload.percent,
              sent: formatBytes(upload.loaded),
              total: formatBytes(upload.size),
            })}
          >
            <div
              className={`${styles.progressFill} ${upload.status === "finalizing" ? styles.progressFillFinishing : ""}`}
              style={{ width: `${upload.percent}%` }}
            />
          </div>
        </div>
      ) : (
        <span className={styles.uploadFinishedMeta}>
          {isDir ? (upload.size > 0 ? formatBytes(upload.size) : "")
            : item ? formatRelativeTime(item.updated_at) : t("drive.uploadCompleted")}
        </span>
      )}
      {isDir && <span className={styles.folderChevron}><ChevronRightIcon size={14} /></span>}
    </>
  );

  return (
    <div className={`${styles.row} ${styles.uploadRow}`}>
      {navigable ? (
        <Link className={styles.uploadLink} to={filesHref(path)}>{content}</Link>
      ) : (
        <div className={styles.uploadLink}>{content}</div>
      )}
      {!completed && (
        <div className={styles.uploadActions}>
          {upload.retryIds.length > 0 && (
            <button
              className={styles.textBtn}
              onClick={() => upload.retryIds.forEach(onRetry)}
              title={t("drive.retryUpload")}
            >
              {t("common.retry")}
            </button>
          )}
          {upload.cancelIds.length > 0 && (
            <button
              className={`${styles.ghostBtn} ${styles.cancelUpload}`}
              onClick={() => upload.cancelIds.forEach(onCancel)}
              title={t("drive.cancelUpload")}
              aria-label={t("drive.cancelUploadNamed", { name })}
            >
              <CloseIcon size={13} />
            </button>
          )}
        </div>
      )}
    </div>
  );
});

function DeleteConfirmation({
  target,
  onCancel,
  onConfirm,
}: {
  target: Item;
  onCancel: () => void;
  onConfirm: () => Promise<void>;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const submitting = useRef(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => element?.close();
  }, []);

  const confirm = async () => {
    if (submitting.current) return;
    submitting.current = true;
    setBusy(true);
    setError(null);
    try {
      await onConfirm();
    } catch (error) {
      setError(error instanceof Error ? error.message : t("drive.deleteFailed"));
    } finally {
      submitting.current = false;
      setBusy(false);
    }
  };

  return (
    <dialog
      ref={dialog}
      className={styles.deleteDialog}
      aria-labelledby="delete-title"
      aria-describedby="delete-description"
      onCancel={(event) => {
        event.preventDefault();
        if (!submitting.current) onCancel();
      }}
    >
      <h2 id="delete-title" className={styles.deleteTitle}>
        {t(target.is_dir ? "drive.deleteFolderTitle" : "drive.deleteFileTitle", { name: target.name })}
      </h2>
      <p id="delete-description" className={styles.deleteDescription}>
        {t(target.is_dir ? "drive.deleteFolderWarning" : "drive.deleteFileWarning")}
      </p>
      <p className={styles.deletePath}>{target.path}</p>
      {error && <p className={styles.deleteError} role="alert">{error}</p>}
      <div className={styles.deleteActions} aria-busy={busy}>
        <button className={styles.secondaryBtn} autoFocus disabled={busy} onClick={onCancel}>
          {t("common.cancel")}
        </button>
        <button className={styles.confirmDeleteBtn} disabled={busy} onClick={() => void confirm()}>
          {t(busy ? "drive.deleting" : "drive.delete")}
        </button>
      </div>
    </dialog>
  );
}

/** A file's own page: the path bar above it already names the file, so this adds only facts. */
function FileDetail({ file, onDelete, deleteDisabled, deleteHint }: {
  file: Item;
  onDelete: () => void;
  deleteDisabled: boolean;
  deleteHint?: string;
}) {
  return (
    <>
      <p className={styles.fileMeta}>
        <span>{fileTypeName(file)}</span>
        <span className={styles.statSep}>·</span>
        <span>{formatSize(Number(file.size))}</span>
        <span className={styles.statSep}>·</span>
        <span>{formatRelativeTime(file.updated_at)}</span>
      </p>

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
        <div className={styles.detailActions}>
          <button
            className={`${styles.secondaryBtn} ${styles.deleteBtn}`}
            onClick={onDelete}
            disabled={deleteDisabled}
            title={deleteHint}
          >
            <TrashIcon size={14} />
            {t("file.delete")}
          </button>
        </div>
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
  const [searchOpen, setSearchOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [uploadMenuOpen, setUploadMenuOpen] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [renameName, setRenameName] = useState("");
  const [renameJob, setRenameJob] = useState<Job | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Item | null>(null);
  const deleting = useRef(false);
  const menuTrigger = useRef<HTMLButtonElement>(null);

  const fileInput = useRef<HTMLInputElement>(null);
  const folderInput = useRef<HTMLInputElement>(null);
  const searchInput = useRef<HTMLInputElement>(null);
  const renameInput = useRef<HTMLInputElement>(null);
  const renameTimer = useRef<number | null>(null);
  const toastTimer = useRef<number | null>(null);
  const dragDepth = useRef(0);
  const completedUploads = useRef(0);
  // Every list / search request gets a ticket; a response older than the newest ticket is dropped.
  const requestId = useRef(0);
  // The upload callback is async: by completion the user may have moved to another directory.
  const listingRef = useRef({ path, searching: false });
  listingRef.current = { path, searching: Boolean(activeQuery) };
  const panelRef = useRef<HTMLElement>(null);
  // The card's height just before a switch, so it can resize instead of jumping.
  const heightFrom = useRef<{ key: string; height: number } | null>(null);
  // A first paint has no previous listing to grow from.
  const loadedPathRef = useRef<string | null>(null);
  loadedPathRef.current = loadedPath;

  const rememberPanelHeight = useCallback((key: string) => {
    const panel = panelRef.current;
    if (!panel || loadedPathRef.current === null) return;
    heightFrom.current = { key, height: panel.getBoundingClientRect().height };
  }, []);

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
        rememberPanelHeight(`${target}|`);
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
    [navigate, rememberPanelHeight],
  );

  // The path is the URL (/files/Photos/2024), so back/forward and direct visits open the right place.
  useEffect(() => {
    void load(path);
  }, [path, load]);

  useEffect(() => {
    setRenaming(false);
    setMenuOpen(false);
    setDeleteTarget(null);
  }, [path]);

  /** Once a file's confirm succeeds, swap its placeholder row for the returned node. */
  const handleUploaded = useCallback((_task: UploadTask, item: Item) => {
    completedUploads.current += 1;
    if (listingRef.current.searching || listingRef.current.path !== item.parent) return;
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

  const unfinishedUploadCount = uploads.filter((task) => task.status !== "completed").length;

  // Completed metadata stays available for folder counts after the final confirmation.
  useEffect(() => {
    if (unfinishedUploadCount > 0 || completedUploads.current === 0) return;
    completedUploads.current = 0;
    showToast(t("drive.uploadCompleted"));
  }, [unfinishedUploadCount, showToast]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        setCreating(false);
        setMenuOpen(false);
        setUploadMenuOpen(false);
        setRenaming(false);
        setToast(null);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  // "/" opens the search field, except while typing in one.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target?.tagName === "INPUT" || target?.tagName === "TEXTAREA" || target?.isContentEditable;
      if (event.key === "/" && !typing) {
        event.preventDefault();
        setSearchOpen(true);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  useEffect(() => {
    if (!searchOpen) return;
    searchInput.current?.focus();
  }, [searchOpen]);

  // Focus and pre-select the name, so a rename starts from the current name, not an empty box.
  useEffect(() => {
    if (!renaming) return;
    renameInput.current?.focus();
    renameInput.current?.select();
  }, [renaming]);

  // A rename keeps polling after the page unmounts without this.
  useEffect(
    () => () => {
      if (renameTimer.current) window.clearTimeout(renameTimer.current);
    },
    [],
  );

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
      rememberPanelHeight(`${path}|${value}`);
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

  // A switch keeps the outgoing listing on screen until the next one arrives, so the card never
  // collapses into a skeleton; a reload of the same directory must not dim what is already there.
  const pending = loading || loadedPath !== path;
  const firstLoad = loadedPath === null;
  // The URL already moved on, so the rows on screen belong to the directory being left.
  const navigating = loadedPath !== null && loadedPath !== path;
  const listPath = navigating ? (loadedPath ?? path) : path;

  const entries = useMemo<UploadRowEntry[]>(() => {
    // Search results preserve their API ordering and do not mix in a folder's placeholders.
    if (activeQuery) return items.map((item) => ({
      path: item.path,
      name: item.name,
      isDir: item.is_dir,
      item,
    }));
    return buildUploadRows(items, uploads, listPath);
  }, [activeQuery, items, listPath, uploads]);
  const visibleUploads = entries.filter((entry) => entry.upload);
  const uploadDirectory = isUploadDirectory(uploads, path);

  const listKey = `${loadedPath ?? ""}|${activeQuery}`;

  // A folder switch changes the card's height by whole rows (six folders to an empty state is
  // over 200px), and that jump reads as a blink even when the rows fade. Resize the card along
  // with the fade, from the height measured just before the new listing rendered.
  useLayoutEffect(() => {
    const panel = panelRef.current;
    const from = heightFrom.current;
    if (!panel || from === null || from.key !== listKey) return;
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      heightFrom.current = null;
      return;
    }
    const to = panel.getBoundingClientRect().height;
    if (Math.abs(to - from.height) < 2) {
      heightFrom.current = null;
      return;
    }
    panel.style.transition = "none";
    panel.style.height = `${from.height}px`;
    // Flush the starting height before handing the change to the transition.
    void panel.offsetHeight;
    panel.style.transition = "height 0.24s cubic-bezier(0.22, 1, 0.36, 1)";
    panel.style.height = `${to}px`;
    const timer = window.setTimeout(() => {
      panel.style.transition = "";
      panel.style.height = "";
      if (heightFrom.current === from) heightFrom.current = null;
    }, 260);
    return () => {
      window.clearTimeout(timer);
      panel.style.transition = "";
      panel.style.height = "";
    };
  }, [listKey]);

  // Nothing worth keeping on screen (first paint, or an empty folder): show the skeleton instead.
  const showSkeleton = firstLoad || (navigating && entries.length === 0);

  // Before the first listing arrives, build the trail from the URL so the title and breadcrumbs
  // do not flash "all files" on a deep link.
  const trail = crumbTrail.length > 0 && loadedPath === path ? crumbTrail : crumbsForPath(path);
  const currentCrumb = trail[trail.length - 1];
  const currentName = currentCrumb ? crumbName(currentCrumb) : t("drive.root");

  const activeUploadCount = uploads.filter((task) => task.status !== "failed"
    && task.status !== "completed" && !task.isDir).length;
  const file = !activeQuery && current && !current.is_dir ? current : null;
  const notFound = !pending && !loadError && !activeQuery && !file && current === null
    && path !== "" && !uploadDirectory;
  // The current path may be a file even while search results are on screen; never upload to it.
  const canUpload =
    !pending && !loadError && !file && !notFound && !(current !== null && !current.is_dir);

  // A rename only ever changes the last segment, so the parent stays put.
  const parentPath = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
  const hasUnfinishedUploads = (target: string) => uploads.some((task) =>
    (task.path === target || task.path.startsWith(`${target}/`)) && task.status !== "completed",
  );
  const deleteBlocked = hasUnfinishedUploads(path);

  const startDelete = (target: Item) => {
    // The menu item unmounts; return keyboard focus to its persistent trigger on cancel.
    if (menuOpen) menuTrigger.current?.focus();
    setMenuOpen(false);
    setDeleteTarget(target);
  };

  const applyDelete = async (target: Item) => {
    if (deleting.current) return;
    if (hasUnfinishedUploads(target.path)) throw new Error(t("drive.deleteUploadBlocked"));
    deleting.current = true;
    try {
      await api.deletePath(target.path, target.is_dir);
      // Retained upload metadata must not recreate rows for content that was just deleted.
      uploads.filter((task) => task.path === target.path || task.path.startsWith(`${target.path}/`))
        .forEach((task) => cancelUpload(task.id));
      setDeleteTarget(null);
      showToast(t("drive.deleted", { name: target.name }));
      // A browser back/forward navigation may have left the deleted path while the request ran.
      if (listingRef.current.path === target.path) {
        navigate(filesHref(target.parent), { replace: true });
      }
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
      }
      throw error;
    } finally {
      deleting.current = false;
    }
  };

  const startCreateFolder = () => {
    setMenuOpen(false);
    setCreating(true);
    setNewName("");
  };

  const startRename = () => {
    setMenuOpen(false);
    setRenameName(currentName);
    setRenaming(true);
  };

  const cancelRename = () => {
    setRenaming(false);
    setRenameName("");
  };

  const clearRenameTimer = () => {
    if (renameTimer.current) window.clearTimeout(renameTimer.current);
    renameTimer.current = null;
  };

  const finishRename = (to: string) => {
    clearRenameTimer();
    setRenameJob(null);
    cancelRename();
    showToast(t("drive.renamed", { name: to.split("/").pop() ?? to }));
    // The path is the identity: the new URL reloads the listing through the route effect.
    navigate(filesHref(to), { replace: true });
  };

  const failRename = (message: string) => {
    clearRenameTimer();
    setRenameJob(null);
    showToast(message, "error");
  };

  /** A directory rename answers 202 first; the job row shows how many objects are left. */
  const pollRename = (id: string, to: string) => {
    const tick = async () => {
      try {
        const { job } = await api.renameStatus(id);
        setRenameJob(job);
        if (job.status === "done") {
          finishRename(to);
          return;
        }
        if (job.status === "failed" || job.status === "cancelled") {
          failRename(job.error || t("drive.renameFailed"));
          return;
        }
        renameTimer.current = window.setTimeout(tick, 1200);
      } catch (error) {
        failRename(error instanceof Error ? error.message : t("drive.renameFailed"));
      }
    };
    renameTimer.current = window.setTimeout(tick, 600);
  };

  const applyRename = async () => {
    const name = renameName.trim().normalize("NFC");
    if (!name) {
      cancelRename();
      return;
    }
    const from = path;
    const to = joinPath(parentPath, name);
    setRenaming(false);
    if (to === from) return;
    try {
      const { job } = await api.rename(from, to);
      if (!job || job.status === "done") {
        finishRename(to);
        return;
      }
      if (job.status === "failed" || job.status === "cancelled") {
        failRename(job.error || t("drive.renameFailed"));
        return;
      }
      setRenameJob(job);
      pollRename(job.id, to);
    } catch (error) {
      failRename(error instanceof Error ? error.message : t("drive.renameFailed"));
    }
  };

  const onDrop = (event: React.DragEvent) => {
    event.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    // Capture entries while the drop data store is still readable, before awaiting traversal.
    void droppedUploadEntries(event.dataTransfer)
      .then((entries) => enqueue(entries, path, itemExists))
      .catch((error) => showToast(
        error instanceof Error ? error.message : t("drive.readUploadFailed"), "error",
      ));
  };

  const onFilesSelected = (event: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    event.target.value = "";
    try {
      enqueue(filesToUploadEntries(files), path, itemExists);
    } catch (error) {
      showToast(error instanceof Error ? error.message : t("drive.readUploadFailed"), "error");
    }
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
        onChange={onFilesSelected}
      />
      <input
        ref={folderInput}
        type="file"
        multiple
        hidden
        {...{ webkitdirectory: "" }}
        onChange={onFilesSelected}
      />

      <main className={styles.main}>
        {/* The path is the title: one row, with this page's controls on the right. */}
        <div className={styles.topbar}>
          <div className={styles.pathArea}>
            {activeQuery ? (
              <h1 className={styles.pathTitle}>
                {t("drive.resultsFor", { query: activeQuery, results: resultsText(items.length) })}
              </h1>
            ) : (
              <nav className={styles.crumbs} aria-label={t("drive.pathLabel")}>
                {trail.map((crumb, index) => {
                  const isLast = index === trail.length - 1;
                  return (
                    <span key={crumb.path || "root"} className={styles.crumbItem}>
                      {index > 0 && (
                        <span className={styles.crumbSep}>
                          <ChevronRightIcon size={13} />
                        </span>
                      )}
                      {!isLast ? (
                        <Link className={styles.crumb} to={filesHref(crumb.path)}>
                          {crumbName(crumb)}
                        </Link>
                      ) : renaming ? (
                        <input
                          ref={renameInput}
                          className={styles.renameInput}
                          value={renameName}
                          aria-label={t("drive.renamePlaceholder")}
                          placeholder={t("drive.renamePlaceholder")}
                          onChange={(event) => setRenameName(event.target.value)}
                          onBlur={cancelRename}
                          onKeyDown={(event) => {
                            if (event.key === "Enter") void applyRename();
                            if (event.key === "Escape") cancelRename();
                          }}
                        />
                      ) : (
                        <h1 className={styles.crumb}>{crumbName(crumb)}</h1>
                      )}
                    </span>
                  );
                })}
                {!activeQuery && !notFound && (
                  <div className={styles.menuWrap}>
                    {/* The menu belongs to the folder you are in, so it hangs off its name. */}
                    <button
                      ref={menuTrigger}
                      className={styles.menuTrigger}
                      onClick={() => setMenuOpen((open) => !open)}
                      aria-haspopup="menu"
                      aria-expanded={menuOpen}
                      aria-label={t("drive.actions")}
                      title={t("drive.actions")}
                    >
                      <ChevronDownIcon size={15} />
                    </button>
                    {menuOpen && (
                      <>
                        {/* A click anywhere outside the popover just closes it. */}
                        <div
                          className={styles.menuScrim}
                          aria-hidden="true"
                          onClick={() => setMenuOpen(false)}
                        />
                        <div className={styles.menu} role="menu">
                          {!pending && !file && (
                            <button
                              className={styles.menuItem}
                              role="menuitem"
                              onClick={startCreateFolder}
                            >
                              <PlusIcon size={14} />
                              {t("drive.newFolder")}
                            </button>
                          )}
                          {!pending && file && (
                            <a
                              className={styles.menuItem}
                              role="menuitem"
                              href={api.downloadHref(file.path)}
                              target="_blank"
                              rel="noreferrer"
                            >
                              <DownloadIcon size={14} />
                              {t("file.download")}
                            </a>
                          )}
                          {!pending && (file || path !== "") && (
                            <button
                              className={styles.menuItem}
                              role="menuitem"
                              onClick={startRename}
                              disabled={Boolean(renameJob)}
                            >
                              <PencilIcon size={14} />
                              {t("drive.rename")}
                            </button>
                          )}
                          {!pending && !loadError && current?.is_dir && path !== "" && (
                            <button
                              className={`${styles.menuItem} ${styles.deleteBtn}`}
                              role="menuitem"
                              onClick={() => startDelete(current)}
                              disabled={Boolean(renameJob) || deleteBlocked}
                              title={deleteBlocked ? t("drive.deleteUploadBlocked") : undefined}
                            >
                              <TrashIcon size={14} />
                              {t("drive.delete")}
                            </button>
                          )}
                        </div>
                      </>
                    )}
                  </div>
                )}
              </nav>
            )}

            {activeQuery && (
              <button className={styles.clearLink} onClick={() => void load(path)}>
                {t("drive.clear")}
              </button>
            )}
          </div>

          <div className={styles.topActions}>
            {searchOpen || activeQuery ? (
              <div className={styles.search}>
                <span className={styles.searchIcon}>
                  <SearchIcon size={15} />
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
                  onBlur={() => {
                    if (!query) setSearchOpen(false);
                  }}
                  onKeyDown={(event) => {
                    if (event.key === "Enter") void runSearch(query);
                    if (event.key === "Escape") {
                      setSearchOpen(false);
                      setQuery("");
                      if (activeQuery) void load(path);
                      event.currentTarget.blur();
                    }
                  }}
                />
              </div>
            ) : (
              <button
                className={styles.iconBtn}
                onClick={() => setSearchOpen(true)}
                title={t("common.search")}
                aria-label={t("common.search")}
              >
                <SearchIcon size={16} />
              </button>
            )}

            {canUpload && (
              <div className={styles.menuWrap}>
                <button
                  className={styles.uploadBtn}
                  onClick={() => setUploadMenuOpen((open) => !open)}
                  aria-expanded={uploadMenuOpen}
                >
                  <UploadIcon size={14} />
                  {t("drive.upload")}
                  {activeUploadCount > 0 && (
                    <span className={styles.uploadBadge}>{activeUploadCount}</span>
                  )}
                  <ChevronDownIcon size={12} />
                </button>
                {uploadMenuOpen && (
                  <>
                    <button
                      className={styles.menuScrim}
                      aria-label={t("common.close")}
                      onClick={() => setUploadMenuOpen(false)}
                    />
                    <div className={`${styles.menu} ${styles.uploadMenu}`}>
                      <button
                        className={styles.menuItem}
                        onClick={() => {
                          setUploadMenuOpen(false);
                          fileInput.current?.click();
                        }}
                      >
                        <FileIcon size={14} />
                        {t("drive.uploadFiles")}
                      </button>
                      <button
                        className={styles.menuItem}
                        onClick={() => {
                          setUploadMenuOpen(false);
                          folderInput.current?.click();
                        }}
                      >
                        <FolderIcon size={14} />
                        {t("drive.uploadFolder")}
                      </button>
                    </div>
                  </>
                )}
              </div>
            )}
          </div>
        </div>

        {renameJob && (
          <div className={styles.jobRow} role="status">
            <span className={styles.jobLabel}>
              {t("drive.renaming", { name: renameJob.from.split("/").pop() || renameJob.from })}
            </span>
            {renameJob.total > 0 && (
              <span className={styles.jobCount}>
                {renameJob.done} / {renameJob.total}
              </span>
            )}
            <span className={styles.jobBar}>
              <span
                className={styles.jobBarFill}
                style={{
                  width: `${
                    renameJob.total > 0 ? Math.round((renameJob.done / renameJob.total) * 100) : 0
                  }%`,
                }}
              />
            </span>
          </div>
        )}

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
          <div
            key={file.path}
            inert={navigating}
            className={`${styles.enter} ${navigating ? styles.dim : ""}`}
          >
            <FileDetail
              file={file}
              onDelete={() => startDelete(file)}
              deleteDisabled={pending || Boolean(renameJob) || deleteBlocked}
              deleteHint={deleteBlocked ? t("drive.deleteUploadBlocked") : undefined}
            />
          </div>
        ) : notFound ? (
          <>
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
            <section ref={panelRef} className={styles.panel} aria-busy={pending}>
              {/* A switch has no skeleton to show, so a hairline at the card's edge carries the wait. */}
              {pending && !firstLoad && <span className={styles.loadLine} aria-hidden="true" />}

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

              {showSkeleton ? (
                <>
                  {visibleUploads.map((entry) => (
                    <UploadRow
                      key={entry.path}
                      entry={entry}
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
              ) : (
                // The key is the directory (or query), so a new listing mounts once and fades in;
                // an in-place refresh of the same listing keeps the rows it already has.
                <div
                  key={listKey}
                  inert={navigating}
                  className={`${styles.list} ${styles.enter} ${navigating ? styles.dim : ""}`}
                >
                  {entries.map((entry) =>
                    entry.upload && (entry.isDir || entry.upload.status !== "completed" || !entry.item) ? (
                      <UploadRow
                        key={entry.path}
                        entry={entry}
                        onCancel={cancelUpload}
                        onRetry={retryUpload}
                      />
                    ) : entry.item ? (
                      <ItemRow
                        key={entry.path}
                        item={entry.item}
                        activeQuery={Boolean(activeQuery)}
                      />
                    ) : null,
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

      {deleteTarget && (
        <DeleteConfirmation
          key={deleteTarget.path}
          target={deleteTarget}
          onCancel={() => setDeleteTarget(null)}
          onConfirm={() => applyDelete(deleteTarget)}
        />
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
