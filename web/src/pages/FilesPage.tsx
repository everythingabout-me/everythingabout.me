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
import { Link, useBlocker, useLoaderData, useLocation, useNavigate } from "react-router";
import { ApiError, api, filePath, isFileOperation, prefetchListing, type AuthUser, type Crumb, type Item, type FileOperation } from "../api/client";
import { AppHeader } from "../components/AppHeader";
import {
  ChevronDownIcon,
  ChevronRightIcon,
  CloseIcon,
  CopyIcon,
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
import { TransferDialog, type TransferMode } from "../components/TransferDialog";
import { filesHref, filesRoutePath } from "../filesPath";
import { resultsText, t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import { useUploadQueue, type UploadStatus, type UploadTask } from "./useUploadQueue";
import { droppedUploadEntries, filesToUploadEntries } from "./uploadSelection";
import { buildUploadRows, isUploadDirectory, type UploadRowEntry } from "./uploadRows";
import { pendingFileOperations, setUploadOwner, settleFileOperation, subscribeFileOperations, trackFileOperation } from "./uploadPersistence";
import styles from "./FilesPage.module.css";

/**
 * The previewer is a heavy chunk, only fetched when a file detail page actually shows a
 * preview. This import must stay at module top level so the build splits it into its own chunk.
 */
const FilePreview = lazy(() => import("../components/preview/FilePreview"));

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

/** Keeps two decimals below the unit so a per-second progress tick always moves the number. */
function formatPreciseBytes(bytes: number): string {
  if (!(bytes > 0)) return "0 B";
  if (bytes < 1024) return `${Math.round(bytes)} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = bytes / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value.toFixed(2)} ${units[index]}`;
}

/** Matches the backend: directories first, the rest sorted by lowercased name. */
function compareNames(a: string, b: string): number {
  const left = a.toLowerCase();
  const right = b.toLowerCase();
  if (left === right) return a < b ? -1 : a > b ? 1 : 0;
  return left < right ? -1 : 1;
}

function compareItems(a: Item, b: Item): number {
  if (a.kind !== b.kind) return a.kind === "folder" ? -1 : 1;
  return compareNames(a.name, b.name);
}

async function waitForOperation(operation: FileOperation): Promise<void> {
  let current = operation;
  while (current.state !== "succeeded") {
    if (current.state === "failed" || current.state === "cancelled") {
      throw new Error(current.error?.detail || t("error.saveFailed"));
    }
    await new Promise<void>((resolve) => window.setTimeout(resolve, 1000));
    current = await api.getOperation(current.id);
  }
}

function uploadStatusText(status: UploadStatus): string {
  switch (status) {
    case "queued":
      return t("drive.queued");
    case "waiting":
      return t("drive.waiting");
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
    case "waiting":
      return styles.statusQueued;
    case "failed":
      return styles.statusFailed;
    default:
      return styles.statusActive;
  }
}

/** The frontend names the root breadcrumb; the backend's "all files" string is API data only. */
function crumbName(crumb: Crumb): string {
  return crumb.path === "/" ? t("drive.root") : crumb.name;
}

/** A local trail for the first paint (and for a path the API has no breadcrumb for yet). */
function crumbsForPath(path: string): Crumb[] {
  const trail: Crumb[] = [{ name: "", path: "/" }];
  let cursor = "";
  for (const segment of path.split("/")) {
    if (!segment) continue;
    cursor += `/${segment}`;
    trail.push({ name: segment, path: cursor });
  }
  return trail;
}

function fileTypeName(item: Item): string {
  const dot = item.name.lastIndexOf(".");
  if (dot > 0 && dot < item.name.length - 1) {
    return item.name.slice(dot + 1).toUpperCase();
  }
  return item.mediaType || t("file.unknownType");
}

const ItemRow = memo(function ItemRow({
  item,
  activeQuery,
}: {
  item: Item;
  activeQuery: boolean;
}) {
  const prefetchTimer = useRef<number | null>(null);
  const cancelPrefetch = useCallback(() => {
    if (prefetchTimer.current === null) return;
    window.clearTimeout(prefetchTimer.current);
    prefetchTimer.current = null;
  }, []);
  // Only folders: opening one is a listing read, while a file is one metadata read that the
  // page needs anyway, and warming it would also ask for the children of a file.
  const folderPath = item.kind === "folder" ? item.path : null;
  // A short pause is intent; sweeping the pointer across a long list should not fetch.
  const schedulePrefetch = useCallback(() => {
    if (folderPath === null || prefetchTimer.current !== null) return;
    prefetchTimer.current = window.setTimeout(() => {
      prefetchTimer.current = null;
      prefetchListing(folderPath);
    }, 90);
  }, [folderPath]);
  useEffect(() => cancelPrefetch, [cancelPrefetch]);

  return (
    // Every entry is a real URL: folders list their contents, files open their detail page.
    <Link
      className={styles.row}
      to={filesHref(item.path)}
      title={item.name}
      onPointerEnter={schedulePrefetch}
      onPointerLeave={cancelPrefetch}
      onFocus={schedulePrefetch}
      onBlur={cancelPrefetch}
    >
      <span className={styles.glyph}>
        <GlyphFor name={item.name} mime={item.mediaType} isDir={item.kind === "folder"} />
      </span>

      <div className={styles.nameCell}>
        <span className={`${styles.name} ${item.kind === "folder" ? styles.nameDir : ""}`}>
          <span className={styles.nameLink}>{item.name}</span>
        </span>
        {activeQuery && item.parentPath && <span className={styles.pathHint}>{item.parentPath}</span>}
      </div>

      <span className={styles.size}>{item.kind === "folder" ? "" : formatSize(Number(item.size))}</span>
      <span className={styles.time}>{formatRelativeTime(item.updatedAt)}</span>
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
  const showProgress = !completed && upload.status !== "waiting" && (!isDir || upload.files > 0);
  const content = (
    <>
      <span className={styles.glyph}>
        <GlyphFor name={name} mime={item?.mediaType || task?.file?.type || null} isDir={isDir} />
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
          {!isDir && upload.status === "uploading" && (
            <span className={styles.uploadSpeed}>
              <span className={styles.uploadSep} aria-hidden="true">·</span>
              {formatSpeed(upload.speed)}
            </span>
          )}
        </div>
        {isDir ? (
          <div className={styles.uploadMeta}>
            <span>{t("drive.uploadFileCount", { count: upload.files.toLocaleString() })}</span>
            <span className={styles.uploadSep}>·</span>
            <span>{t("drive.uploadedFileCount", { count: upload.completedFiles.toLocaleString() })}</span>
          </div>
        ) : task?.error ? (
          <div className={styles.uploadMeta}>
            <span className={styles.uploadError} title={task.error}>{task.error}</span>
          </div>
        ) : null}
        {upload.waitMessage && <div className={styles.uploadWaitMessage}>{upload.waitMessage}</div>}
      </div>
      {showProgress ? (
        <div className={styles.uploadProgress}>
          <div className={styles.uploadProgressLabel}>
            <span>{formatPreciseBytes(upload.loaded)} / {formatBytes(upload.size)}</span>
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
              sent: formatPreciseBytes(upload.loaded),
              total: formatBytes(upload.size),
            })}
          >
            <div
              className={`${styles.progressFill} ${upload.status === "finalizing" ? styles.progressFillFinishing : ""}`}
              style={{ width: `${upload.percent}%` }}
            />
          </div>
        </div>
      ) : completed ? (
        <span className={styles.uploadFinishedMeta}>
          {isDir ? (upload.size > 0 ? formatBytes(upload.size) : "")
            : item ? formatRelativeTime(item.updatedAt) : t("drive.uploadCompleted")}
        </span>
      ) : <span />}
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
        {t(target.kind === "folder" ? "drive.deleteFolderTitle" : "drive.deleteFileTitle", { name: target.name })}
      </h2>
      <p id="delete-description" className={styles.deleteDescription}>
        {t(target.kind === "folder" ? "drive.deleteFolderWarning" : "drive.deleteFileWarning")}
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
  const [downloadError, setDownloadError] = useState<string | null>(null);
  useEffect(() => { setDownloadError(null); }, [file.path]);
  const download = async () => {
    setDownloadError(null);
    try { await api.download(file.path); }
    catch (error) { setDownloadError(error instanceof Error ? error.message : t("error.unknown")); }
  };
  return (
    <>
      <p className={styles.fileMeta}>
        <span>{fileTypeName(file)}</span>
        <span className={styles.statSep}>·</span>
        <span>{formatSize(Number(file.size))}</span>
        <span className={styles.statSep}>·</span>
        <span>{formatRelativeTime(file.updatedAt)}</span>
      </p>

      <section className={styles.viewerPanel}>
        {canPreviewFile(file.name, file.mediaType) ? (
          <Suspense
            fallback={
              <div className={styles.viewerLoading} role="status">
                {t("preview.loading")}
              </div>
            }
          >
            <FilePreview path={file.path} name={file.name} mediaType={file.mediaType} />
          </Suspense>
        ) : (
          <div className={styles.noPreview}>
            <span className={styles.noPreviewIcon}>
              <GlyphFor name={file.name} mime={file.mediaType} isDir={false} />
            </span>
            <span className={styles.emptyTitle}>{t("file.unsupportedTitle")}</span>
            <span className={styles.emptyText}>{t("file.unsupportedText")}</span>
            <button
              className={styles.primaryLink}
              onClick={() => void download()}
            >
              <DownloadIcon size={14} />
              {t("file.download")}
            </button>
            {downloadError && <p className={styles.downloadError} role="alert">{downloadError}</p>}
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
            <dd>{file.mediaType || fileTypeName(file)}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.size")}</dt>
            <dd>{formatSize(Number(file.size))}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.modified")}</dt>
            <dd>{formatRelativeTime(file.updatedAt)}</dd>
          </div>
          <div className={styles.detailRow}>
            <dt>{t("file.created")}</dt>
            <dd>{file.createdAt ? formatRelativeTime(file.createdAt) : "—"}</dd>
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
  const location = useLocation();
  const navigate = useNavigate();
  const path = filesRoutePath(location.pathname);

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
  const [downloadError, setDownloadError] = useState<string | null>(null);
  const downloadFile = async () => {
    setDownloadError(null);
    try { await api.download(file?.path ?? path); }
    catch (error) { setDownloadError(error instanceof Error ? error.message : t("error.unknown")); }
  };
  const [uploadMenuOpen, setUploadMenuOpen] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [renameName, setRenameName] = useState("");
  const [renameJob, setRenameJob] = useState<FileOperation | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Item | null>(null);
  const [transfer, setTransfer] = useState<{ mode: TransferMode; item: Item } | null>(null);
  const [deletionJobs, setDeletionJobs] = useState<{ operation: FileOperation; path: string }[]>([]);
  const [deletionStatusError, setDeletionStatusError] = useState(false);
  const deletionRequest = useRef(0);
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

  const refreshDeletions = useCallback(async () => {
    const request = ++deletionRequest.current;
    const known = pendingFileOperations();
    const active: { operation: FileOperation; path: string }[] = [];
    let failed = false;
    // One operation's failure must not hide the others, and a record the server no
    // longer knows is settled rather than reported: this is background bookkeeping,
    // not something the user asked for or can act on.
    await Promise.all(known.map(async (entry) => {
      try {
        const operation = await api.getOperation(entry.id);
        if (operation.state === "succeeded") settleFileOperation(operation.id);
        else active.push({ operation, path: entry.path });
      } catch (error) {
        if (error instanceof ApiError && (error.status === 404 || error.status === 410)) {
          settleFileOperation(entry.id);
          return;
        }
        failed = true;
      }
    }));
    if (request !== deletionRequest.current) return;
    setDeletionJobs(active.filter(({ operation }) => operation.kind === "delete" && operation.state !== "succeeded"));
    setDeletionStatusError(failed);
  }, []);

  useEffect(() => {
    setUploadOwner(user.id);
    void refreshDeletions();
    const unsubscribe = subscribeFileOperations(() => { void refreshDeletions(); });
    return () => { deletionRequest.current += 1; unsubscribe(); };
  }, [user.id, refreshDeletions]);

  const activeDeletions = deletionJobs.some(({ operation }) =>
    operation.state === "queued" || operation.state === "running" || operation.state === "outcome_unknown");
  useEffect(() => {
    if (!activeDeletions && !deletionStatusError) return;
    let stopped = false;
    let timer: number;
    // Poll while a deletion is running. When the status cannot be read the retry
    // backs off instead of hammering the API in a three-second loop.
    const tick = async () => {
      await refreshDeletions();
      if (!stopped) timer = window.setTimeout(tick, deletionStatusError ? 30_000 : 3_000);
    };
    timer = window.setTimeout(tick, deletionStatusError ? 30_000 : 3_000);
    return () => { stopped = true; window.clearTimeout(timer); };
  }, [activeDeletions, deletionStatusError, refreshDeletions]);

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

  /** Move or copy one entry into a folder chosen in the dialog. A move changes the current URL. */
  const applyTransfer = useCallback(
    async (mode: TransferMode, item: Item, destination: string) => {
      const target = destination === "/" ? `/${item.name}` : `${destination}/${item.name}`;
      const result = mode === "move"
        ? await api.move(item.path, target, item.revision)
        : await api.copy(item.path, target, item.revision);
      if (isFileOperation(result) && result.state !== "succeeded") {
        if (result.state === "failed" || result.state === "cancelled") {
          throw new Error(result.error?.detail || t("drive.transferFailed"));
        }
        try {
          await waitForOperation(result);
        } catch (caught) {
          throw caught instanceof Error ? caught : new Error(t("drive.transferFailed"));
        }
      }
      setTransfer(null);
      showToast(t(mode === "move" ? "drive.moved" : "drive.copied", { name: item.name }));
      if (mode === "move") navigate(filesHref(target), { replace: true });
      else void load(path);
    },
    [load, navigate, path, showToast],
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

  /** Only settled upload resources replace placeholder rows. */
  const handleUploaded = useCallback((_task: UploadTask, item: Item) => {
    completedUploads.current += 1;
    if (listingRef.current.searching || filePath(listingRef.current.path) !== item.parentPath) return;
    setItems((currentItems) => {
      const next = currentItems.filter((existing) => existing.path !== item.path);
      next.push(item);
      next.sort(compareItems);
      return next;
    });
  }, []);

  const {
    tasks: uploads,
    hasActiveUploads,
    enqueue,
    cancel: cancelUpload,
    retry: retryUpload,
  } = useUploadQueue(handleUploaded);

  const uploadBlocker = useBlocker(({ nextLocation }) =>
    hasActiveUploads && !/^\/files(?:\/|$)/.test(nextLocation.pathname));
  useEffect(() => {
    if (uploadBlocker.state !== "blocked") return;
    if (window.confirm(t("drive.leaveUploadsConfirm"))) uploadBlocker.proceed();
    else uploadBlocker.reset();
  }, [uploadBlocker]);

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
    (target: string) => items.some((item) => item.path === filePath(target)),
    [items],
  );

  const createFolder = async () => {
    const name = newName;
    if (!name) {
      setCreating(false);
      return;
    }
    try {
      const result = await api.mkdir(joinPath(path, name));
      if (isFileOperation(result)) await waitForOperation(result);
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
      isDir: item.kind === "folder",
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
  const file = !activeQuery && current && current.kind !== "folder" ? current : null;
  const notFound = !pending && !loadError && !activeQuery && !file && current === null
    && path !== "" && !uploadDirectory;
  // The current path may be a file even while search results are on screen; never upload to it.
  const canUpload =
    !pending && !loadError && !file && !notFound && !(current !== null && current.kind !== "folder");

  // A rename only ever changes the last segment, so the parent stays put.
  const parentPath = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
  const hasUnfinishedUploads = (target: string) => uploads.some((task) =>
    (filePath(task.path) === filePath(target) || filePath(task.path).startsWith(`${filePath(target)}/`)) && task.status !== "completed",
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
      const operation = await api.deletePath(target.path, target.kind === "folder", target.revision);
      deletionRequest.current += 1;
      if (operation && operation.state !== "succeeded") {
        trackFileOperation(operation, target.path);
        setDeletionJobs((jobs) => [{ operation, path: target.path }, ...jobs.filter((existing) => existing.operation.id !== operation.id)]);
        showToast(t("drive.cleanupActive"));
      } else {
        showToast(t("drive.deleted", { name: target.name }));
      }
      setDeletionStatusError(false);
      uploads.filter((task) => filePath(task.path) === target.path || filePath(task.path).startsWith(`${target.path}/`))
        .forEach((task) => cancelUpload(task.id));
      setDeleteTarget(null);
      if (filePath(listingRef.current.path) === target.path) {
        navigate(filesHref(target.parentPath ?? "/"), { replace: true });
      }
    } catch (error) {
      if (error instanceof ApiError && error.operationId) {
        const operation: FileOperation = {
          id: error.operationId, kind: "delete", state: "outcome_unknown",
          progress: { completed: 0, total: null, unit: "entries" }, result: null, error: null,
          cancelRequested: false, createdAt: new Date().toISOString(), finishedAt: null,
        };
        trackFileOperation(operation, target.path);
        void refreshDeletions();
      }
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

  /** Poll the common Operation until the native rename settles. */
  const pollRename = (id: string, to: string) => {
    const tick = async () => {
      try {
        const job = await api.getOperation(id);
        setRenameJob(job);
        if (job.state === "succeeded") {
          finishRename(to);
          return;
        }
        if (job.state === "failed" || job.state === "cancelled") {
          failRename(job.error?.detail || t("drive.renameFailed"));
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
    const name = renameName;
    if (!name) {
      cancelRename();
      return;
    }
    const from = path;
    const to = joinPath(parentPath, name);
    setRenaming(false);
    if (to === from) return;
    try {
      if (!current) return;
      const result = await api.move(from, to, current.revision);
      if (!isFileOperation(result) || result.state === "succeeded") {
        finishRename(to);
        return;
      }
      if (result.state === "failed" || result.state === "cancelled") {
        failRename(result.error?.detail || t("drive.renameFailed"));
        return;
      }
      setRenameJob(result);
      pollRename(result.id, to);
    } catch (error) {
      if (error instanceof ApiError && error.operationId) pollRename(error.operationId, to);
      else failRename(error instanceof Error ? error.message : t("drive.renameFailed"));
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
        {deletionJobs.length > 0 && (
          <aside className={styles.deletions} aria-label={t("drive.cleanupTitle")} aria-live="polite">
            {deletionJobs.map(({ operation: job, path: deletionPath }) => (
              <div key={job.id} className={styles.deletionStatus}>
                <span className={styles.deletionPath} title={deletionPath}>{deletionPath}</span>
                <span className={job.state === "failed" || job.state === "cancelled" || job.state === "outcome_unknown" ? styles.deletionAttention : undefined}>
                  {t(job.state === "failed" || job.state === "cancelled" || job.state === "outcome_unknown" ? "drive.cleanupAttention" : "drive.cleanupActive")}
                </span>
                <span className={styles.deletionProgress}>
                  {t("drive.cleanupProgress", { done: job.progress.completed.toLocaleString(), total: job.progress.total?.toLocaleString() ?? "—" })}
                </span>
                {(job.state === "failed" || job.state === "cancelled" || job.state === "outcome_unknown") && <span className={styles.deletionMessage}>{t("drive.cleanupFailed")}</span>}
              </div>
            ))}
          </aside>
        )}
        {/* The path is the title: one row, with this page's controls on the right. */}
        {downloadError && <p className={styles.downloadError} role="alert">{downloadError}</p>}
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
                        <Link
                          className={styles.crumb}
                          to={filesHref(crumb.path)}
                          onPointerEnter={() => prefetchListing(crumb.path)}
                        >
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
                            <button
                              className={styles.menuItem}
                              role="menuitem"
                              onClick={() => void downloadFile()}
                            >
                              <DownloadIcon size={14} />
                              {t("file.download")}
                            </button>
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
                          {!pending && current && path !== "" && (
                            <>
                              <button
                                className={styles.menuItem}
                                role="menuitem"
                                disabled={Boolean(renameJob)}
                                onClick={() => {
                                  setMenuOpen(false);
                                  setTransfer({ mode: "move", item: current });
                                }}
                              >
                                <FolderIcon size={14} />
                                {t("drive.moveTo")}
                              </button>
                              <button
                                className={styles.menuItem}
                                role="menuitem"
                                disabled={Boolean(renameJob)}
                                onClick={() => {
                                  setMenuOpen(false);
                                  setTransfer({ mode: "copy", item: current });
                                }}
                              >
                                <CopyIcon size={14} />
                                {t("drive.copyTo")}
                              </button>
                            </>
                          )}
                          {!pending && !loadError && current?.kind === "folder" && path !== "" && (
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
              {t("drive.renaming", { name: currentName })}
            </span>
            {(renameJob.progress.total ?? 0) > 0 && (
              <span className={styles.jobCount}>
                {renameJob.progress.completed} / {renameJob.progress.total}
              </span>
            )}
            <span className={styles.jobBar}>
              <span
                className={styles.jobBarFill}
                style={{
                  width: `${
                    (renameJob.progress.total ?? 0) > 0 ? Math.round((renameJob.progress.completed / (renameJob.progress.total || 1)) * 100) : 0
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

      {transfer && (
        <TransferDialog
          key={`${transfer.mode}:${transfer.item.path}`}
          mode={transfer.mode}
          item={transfer.item}
          onCancel={() => setTransfer(null)}
          onConfirm={(destination) => applyTransfer(transfer.mode, transfer.item, destination)}
        />
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
