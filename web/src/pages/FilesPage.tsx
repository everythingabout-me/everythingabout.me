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
  type ReactNode,
} from "react";
import { Link, useBlocker, useLoaderData, useLocation, useNavigate } from "react-router";
import { ApiError, api, filePath, isFileOperation, isNativeAbsent, prefetchListing, type AuthUser, type Crumb, type FileMediaMetadata, type Item, type FileOperation } from "../api/client";
import { AppHeader } from "../components/AppHeader";
import { StorageNotice } from "../components/StorageNotice";
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
import { formatNumber, intlLocale, resultsText, t } from "../i18n";
import { formatRelativeTime } from "../i18n/format";
import { useUploadQueue, type ConflictChoice, type UploadStatus, type UploadTask } from "./useUploadQueue";
import { resolveDeleteBlocker } from "./deleteBlocker";
import { droppedUploadEntries, filesToUploadEntries, foldersWithoutFiles } from "./uploadSelection";
import { buildUploadRows, failedTasks, isUploadDirectory, isUploadRowActive, type UploadRowEntry } from "./uploadRows";
import { pendingFileOperations, setUploadOwner, settleFileOperation, subscribeFileOperations, trackFileOperation } from "./uploadPersistence";
import { formatBytes, formatPreciseBytes, formatSize, formatSpeed } from "./sizeFormat";
import { cameraText, capturedAtText, coordinatesText, dimensionsText, durationText, exposureText, mayHaveMediaMetadata } from "./mediaMetadataFormat";
import styles from "./FilesPage.module.css";

/**
 * The previewer is a heavy chunk, only fetched when a file detail page actually shows a
 * preview. This import must stay at module top level so the build splits it into its own chunk.
 */
const FilePreview = lazy(() => import("../components/preview/FilePreview"));

function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
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

/**
 * How often a delete re-sends itself after the operation that refused it has already ended.
 * Each retry answers one short-lived holder — a commit or a folder creation that finished
 * between the refusal and the read — so the bound only stops a delete that keeps losing to
 * a stream of them (an import still publishing into the folder) from spinning forever.
 */
const DELETE_BLOCKER_ATTEMPTS = 32;

async function waitForOperation(
  operation: FileOperation,
  onProgress?: (progress: FileOperation["progress"]) => void,
): Promise<void> {
  let current = operation;
  onProgress?.(current.progress);
  while (current.state !== "succeeded") {
    if (current.state === "cancelled") {
      throw new Error(current.error?.detail || t("drive.transferCancelled"));
    }
    if (current.state === "failed") {
      throw new Error(current.error?.detail || t("error.saveFailed"));
    }
    await new Promise<void>((resolve) => window.setTimeout(resolve, 1000));
    current = await api.getOperation(current.id);
    onProgress?.(current.progress);
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
  onResume,
}: {
  entry: UploadRowEntry;
  onCancel: (ids: string[]) => void;
  onResume: (id: string) => void;
}) {
  const { upload, task, item, name, path, isDir } = entry;
  if (!upload) return null;
  const completed = upload.status === "completed";
  const navigable = isDir || completed;
  const showProgress = !completed && upload.status !== "waiting"
    && (!isDir || upload.files > 0 || upload.folders > 1);
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
                : upload.preparing ? t("drive.uploadCreatingFolders") : uploadStatusText(upload.status)}
            </span>
          )}
          {upload.status === "uploading" && (upload.waitingForSlot || !isDir || upload.speed > 0) && (
            <span className={styles.uploadSpeed}>
              <span className={styles.uploadSep} aria-hidden="true">·</span>
              {upload.waitingForSlot ? t("drive.uploadWaitingForSlot") : formatSpeed(upload.speed)}
            </span>
          )}
        </div>
        {isDir ? (
          <div className={styles.uploadMeta}>
            <span>{t("drive.uploadFileCount", { count: upload.files.toLocaleString() })}</span>
            {upload.folders > 1 && (
              <>
                <span className={styles.uploadSep}>·</span>
                <span>{t("drive.uploadFolderCount", { count: upload.folders.toLocaleString() })}</span>
              </>
            )}
            {upload.progressUnit === "bytes" && (
              <>
                <span className={styles.uploadSep}>·</span>
                <span>{t("drive.uploadedFileCount", { count: upload.completedFiles.toLocaleString() })}</span>
              </>
            )}
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
          {/* A selected tree is counted in items, because creating its folders moves no byte
              and would otherwise hold an empty 0% bar over work that is really happening. */}
          <div className={styles.uploadProgressLabel}>
            <span>{upload.progressUnit === "items"
              ? t("drive.uploadItemProgress", {
                done: upload.completedItems.toLocaleString(),
                total: upload.items.toLocaleString(),
              })
              : `${formatPreciseBytes(upload.loaded)} / ${formatBytes(upload.size)}`}</span>
            <span className={styles.uploadPercent}>{upload.percent}%</span>
          </div>
          <div
            className={styles.progressTrack}
            role="progressbar"
            aria-label={t("drive.uploadProgressLabel", { name })}
            aria-valuemin={0}
            aria-valuemax={100}
            aria-valuenow={upload.percent}
            aria-valuetext={upload.progressUnit === "items"
              ? t("drive.uploadProgressItemsValue", {
                percent: upload.percent,
                done: upload.completedItems.toLocaleString(),
                total: upload.items.toLocaleString(),
              })
              : t("drive.uploadProgressValue", {
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
          {upload.resumeIds.length > 0 && (
            <button
              className={styles.textBtn}
              onClick={() => upload.resumeIds.forEach(onResume)}
              title={t("drive.continueUploadHint")}
            >
              {t("drive.continueUpload")}
            </button>
          )}
          {upload.cancelIds.length > 0 && (
            <button
              className={`${styles.ghostBtn} ${styles.cancelUpload}`}
              onClick={() => onCancel(upload.cancelIds)}
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

/**
 * The failures of the current transfer, as their own short list.
 *
 * A folder row can say "3 failed" and nothing more useful: the files behind that number may
 * be several levels down and the person has no reason to visit them, while the one action
 * they need — send those bytes again — belongs to the transfer, not to the folder. So retry
 * lives here and only here, per file or for all of them, and a row keeps the count as the
 * place a failure is noticed.
 *
 * The list is a projection of the queue and owns no state: it disappears with the tab, the
 * same rule the rest of the transfer list follows, so nothing here can outlive the bytes it
 * describes and disagree with the folder.
 */
const UploadFailures = memo(function UploadFailures({
  tasks,
  onRetry,
  onCancel,
}: {
  tasks: UploadTask[];
  onRetry: (id: string) => void;
  onCancel: (ids: string[]) => void;
}) {
  // The full reason only lives in a title tooltip, which cannot be selected or copied. A
  // failure a person cannot hand to someone else is a failure nobody can act on, so the
  // row copies the two things a report needs: which file, and what the server said.
  const [copied, setCopied] = useState<string | null>(null);
  if (tasks.length === 0) return null;
  const copyReason = (task: UploadTask) => {
    const text = `/${task.path}\n${task.error ?? t("drive.uploadFailed")}`;
    void navigator.clipboard?.writeText(text).then(() => {
      setCopied(task.id);
      window.setTimeout(() => setCopied((current) => (current === task.id ? null : current)), 1500);
    }).catch(() => undefined);
  };
  return (
    <div className={styles.uploadFailures}>
      <div className={styles.uploadFailuresHead}>
        <span className={styles.uploadFailuresTitle}>
          {t("drive.uploadFailuresTitle", { count: tasks.length.toLocaleString() })}
        </span>
        <button
          className={styles.textBtn}
          onClick={() => tasks.forEach((task) => onRetry(task.id))}
          title={t("drive.uploadFailuresRetryAllHint")}
        >
          {t("drive.uploadFailuresRetryAll")}
        </button>
      </div>
      {/* A failed row is not always lost bytes. A direct upload publishes at the destination
          before the commit settles it, so a failure in the index half leaves the file in
          storage and out of the list — and retrying it then meets "already exists". The
          index is a cache of that tree, so the user-facing repair is the same sync the
          account page already offers; without this line the only route was an operator. */}
      <p className={styles.uploadFailuresHint}>
        {t("drive.uploadFailuresSyncHint")}{" "}
        <Link className={styles.failuresSyncLink} to="/account#storage">
          {t("drive.uploadFailuresSyncAction")}
        </Link>
      </p>
      <ul className={styles.uploadFailuresList}>
        {tasks.map((task) => (
          <li className={styles.uploadFailure} key={task.id}>
            <span className={styles.uploadFailurePath} title={`/${task.path}`}>{`/${task.path}`}</span>
            <span className={styles.uploadFailureReason} title={task.error}>
              {task.error ?? t("drive.uploadFailed")}
            </span>
            <button
              className={`${styles.ghostBtn} ${styles.cancelUpload}`}
              onClick={() => copyReason(task)}
              title={copied === task.id ? t("drive.uploadFailureCopied") : t("drive.uploadFailureCopy")}
              aria-label={t("drive.uploadFailureCopy")}
            >
              {copied === task.id ? t("drive.uploadFailureCopied") : <CopyIcon size={12} />}
            </button>
            <button className={styles.textBtn} onClick={() => onRetry(task.id)}>
              {t("common.retry")}
            </button>
            <button
              className={`${styles.ghostBtn} ${styles.cancelUpload}`}
              onClick={() => onCancel([task.id])}
              title={t("drive.cancelUpload")}
              aria-label={t("drive.cancelUploadNamed", { name: task.path })}
            >
              <CloseIcon size={13} />
            </button>
          </li>
        ))}
      </ul>
    </div>
  );
  // A publish tick rebuilds the task array every 100ms while bytes move, and only a failure
  // appearing or leaving changes this list. Comparing by identity keeps a large failure list
  // from re-rendering ten times a second beside a transfer that has nothing new to say.
}, (previous, next) => previous.tasks.length === next.tasks.length
  && previous.tasks.every((task, index) =>
    task.id === next.tasks[index]?.id && task.error === next.tasks[index]?.error));

/**
 * The question is the whole decision: confirming closes the dialog and hands the request
 * to the page. A recursive delete spends its whole request enumerating the subtree
 * natively (one listing per folder, then a second pass that re-reads every identity), so
 * holding a disabled modal over it traps the user behind "Submitting…" for as long as the
 * folder is large, with nothing to cancel and no number to watch. The path bar and the
 * toast carry the outcome instead.
 */
function DeleteConfirmation({
  target,
  onCancel,
  onConfirm,
}: {
  target: Item;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => element?.close();
  }, []);

  return (
    <dialog
      ref={dialog}
      className={styles.deleteDialog}
      aria-labelledby="delete-title"
      aria-describedby="delete-description"
      onCancel={(event) => {
        event.preventDefault();
        onCancel();
      }}
    >
      <h2 id="delete-title" className={styles.deleteTitle}>
        {t(target.kind === "folder" ? "drive.deleteFolderTitle" : "drive.deleteFileTitle", { name: target.name })}
      </h2>
      <p id="delete-description" className={styles.deleteDescription}>
        {t(target.kind === "folder" ? "drive.deleteFolderWarning" : "drive.deleteFileWarning")}
      </p>
      <p className={styles.deletePath}>{target.path}</p>
      <div className={styles.deleteActions}>
        <button className={styles.secondaryBtn} autoFocus onClick={onCancel}>
          {t("common.cancel")}
        </button>
        <button className={styles.confirmDeleteBtn} onClick={onConfirm}>
          {t("drive.delete")}
        </button>
      </div>
    </dialog>
  );
}

/**
 * One question about a name that is already taken, and the answer for everything after it.
 *
 * It replaces the browser's own confirm, which this flow used per file. That dialog is
 * modal, so a folder dropped over an existing tree stopped the tab once per existing file —
 * and it can only answer about the file in front of it, when the question a re-import
 * really asks is "which one do you mean for all of them". The standing answers are the
 * point: they settle the questions already queued behind this one, and they let every later
 * upload say which file it may replace before its create is sent, instead of being refused
 * first. Escape answers "skip", because writing nothing is the answer that can be undone.
 */
function ConflictConfirmation({
  path,
  onAnswer,
}: {
  path: string;
  onAnswer: (choice: ConflictChoice) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const element = dialog.current;
    element?.showModal();
    return () => element?.close();
  }, []);

  return (
    <dialog
      ref={dialog}
      className={styles.deleteDialog}
      aria-labelledby="conflict-title"
      aria-describedby="conflict-description"
      onCancel={(event) => {
        event.preventDefault();
        onAnswer("skip");
      }}
    >
      <h2 id="conflict-title" className={styles.deleteTitle}>
        {t("drive.conflictTitle")}
      </h2>
      <p id="conflict-description" className={styles.deleteDescription}>
        {t("drive.conflictBody")}
      </p>
      <p className={styles.deletePath}>{path}</p>
      <div className={styles.conflictActions}>
        <button className={styles.conflictPrimary} onClick={() => onAnswer("replace-all")}>
          {t("drive.conflictReplaceAll")}
        </button>
        <button className={styles.conflictSecondary} onClick={() => onAnswer("skip-all")}>
          {t("drive.conflictKeepAll")}
        </button>
        <button className={styles.textBtn} onClick={() => onAnswer("replace")}>
          {t("drive.conflictReplace")}
        </button>
        {/* Focus starts here, like the delete dialog's Cancel: Enter must not be the
            key that overwrites a thousand files. */}
        <button className={styles.textBtn} autoFocus onClick={() => onAnswer("skip")}>
          {t("drive.conflictKeep")}
        </button>
      </div>
    </dialog>
  );
}

/**
 * What the sheet says about a standing answer.
 *
 * A decision that outlives the dialog that made it has to be visible: it replaces files for
 * as long as this import runs, and overwriting is permanent. The button beside it is the
 * way back, and the only one that changes anything — the answer already given to a file
 * stays given.
 */
function ConflictPolicyNote({ policy, onAskAgain }: { policy: "replace" | "skip"; onAskAgain: () => void }) {
  return (
    <div className={styles.conflictNote} role="status">
      <span>{t(policy === "replace" ? "drive.conflictReplacing" : "drive.conflictKeeping")}</span>
      <button className={styles.textBtn} onClick={onAskAgain}>
        {t("drive.conflictAskAgain")}
      </button>
    </div>
  );
}

/**
 * What the file says about its own capture, fetched when its page opens. The server parses
 * on the first request and caches; nothing here is loaded for a listing.
 *
 * A failure renders nothing: these rows are an addition to the details above them, and the
 * preview already reports a file whose bytes are gone.
 */
function useMediaMetadata(file: Item): FileMediaMetadata | null {
  const [media, setMedia] = useState<FileMediaMetadata | null>(null);
  const wanted = file.kind === "file" && mayHaveMediaMetadata(file.name, file.mediaType);
  useEffect(() => {
    setMedia(null);
    if (!wanted) return;
    const controller = new AbortController();
    api.mediaMetadata(file.path, controller.signal).then(
      (next) => { if (!controller.signal.aborted) setMedia(next); },
      () => {},
    );
    return () => controller.abort();
    // contentRevision: replaced bytes are a different capture.
  }, [file.path, file.contentRevision, wanted]);
  return media;
}

/** Rows appear only for facts the file states, so a screenshot adds nothing but its size. */
function MediaMetadataRows({ media }: { media: FileMediaMetadata }) {
  const camera = cameraText(media.make, media.model);
  const exposure = exposureText(media);
  const rows: [string, ReactNode][] = [];
  if (media.capturedAt) rows.push([t("file.captured"), capturedAtText(media.capturedAt, intlLocale)]);
  if (media.location) {
    const altitude = media.location.altitude;
    rows.push([t("file.location"), (
      <>
        {coordinatesText(media.location.latitude, media.location.longitude)}
        {altitude !== null && (
          <span className={styles.detailNote}>
            {t("file.altitude", { meters: formatNumber(Math.round(altitude)) })}
          </span>
        )}
      </>
    )]);
  }
  if (camera) rows.push([t("file.camera"), camera]);
  if (media.lens) rows.push([t("file.lens"), media.lens]);
  if (exposure) rows.push([t("file.exposure"), exposure]);
  if (media.width && media.height) rows.push([t("file.dimensions"), dimensionsText(media.width, media.height)]);
  if (media.durationSeconds !== null) rows.push([t("file.duration"), durationText(media.durationSeconds)]);
  return (
    <>
      {rows.map(([label, value]) => (
        <div className={styles.detailRow} key={label}>
          <dt>{label}</dt>
          <dd>{value}</dd>
        </div>
      ))}
    </>
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
  const [downloadMissing, setDownloadMissing] = useState(false);
  useEffect(() => { setDownloadError(null); setDownloadMissing(false); }, [file.path]);
  const media = useMediaMetadata(file);
  const download = async () => {
    setDownloadError(null);
    setDownloadMissing(false);
    try { await api.download(file.path); }
    catch (error) {
      if (isNativeAbsent(error)) setDownloadMissing(true);
      else setDownloadError(error instanceof Error ? error.message : t("error.unknown"));
    }
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
            {downloadMissing && <StorageNotice missingContent />}
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
          {media && <MediaMetadataRows media={media} />}
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
  // Entries read from a dropped tree so far. The walk stats every file (one round trip
  // each), so a large folder takes seconds before the queue has anything to show; this is
  // the only feedback that the drop was received and is being read.
  const [readingUploads, setReadingUploads] = useState<number | null>(null);
  const [searchOpen, setSearchOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [downloadError, setDownloadError] = useState<string | null>(null);
  const [downloadMissing, setDownloadMissing] = useState(false);
  const downloadFile = async () => {
    setDownloadError(null);
    setDownloadMissing(false);
    try { await api.download(file?.path ?? path); }
    catch (error) {
      if (isNativeAbsent(error)) setDownloadMissing(true);
      else setDownloadError(error instanceof Error ? error.message : t("error.unknown"));
    }
  };
  const [uploadMenuOpen, setUploadMenuOpen] = useState(false);
  const [renaming, setRenaming] = useState(false);
  const [renameName, setRenameName] = useState("");
  const [renameJob, setRenameJob] = useState<FileOperation | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Item | null>(null);
  // Paths whose delete request is in flight. The dialog is already gone, and this is what
  // marks the item on screen while the server enumerates a large subtree.
  const [deletingPaths, setDeletingPaths] = useState<string[]>([]);
  const [transfer, setTransfer] = useState<{ mode: TransferMode; item: Item } | null>(null);
  const [transferProgress, setTransferProgress] = useState<FileOperation["progress"] | null>(null);
  const [transferCancelling, setTransferCancelling] = useState(false);
  const transferOperation = useRef<string | null>(null);
  const [trackedDeletions, setTrackedDeletions] = useState<{ operation: FileOperation; path: string }[]>([]);
  const [deletionStatusError, setDeletionStatusError] = useState(false);
  const deletionRequest = useRef(0);
  const notifiedDeleteFailures = useRef(new Set<string>());
  // `deletingPaths` is what the screen shows; this set answers synchronously, so a second
  // click in the same tick cannot start a duplicate request.
  const deletesInFlight = useRef(new Set<string>());
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
  // The mounted view to reload after a background deletion changes the truth on screen.
  const reloadView = useRef<() => void>(() => {});
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

  // Accepted deletions are not user-facing work: the path disappears from the listing
  // at once and the native purge runs invisibly. Only an operation that ends without
  // deleting anything is worth reporting, because that row is back (or may be).
  const refreshDeletions = useCallback(async () => {
    const request = ++deletionRequest.current;
    const known = pendingFileOperations();
    const active: { operation: FileOperation; path: string }[] = [];
    let failed = false;
    // One operation's failure must not hide the others, and a record the server no
    // longer knows is settled rather than reported.
    await Promise.all(known.map(async (entry) => {
      try {
        const operation = await api.getOperation(entry.id);
        const ended = operation.state === "failed" || operation.state === "cancelled" || operation.state === "outcome_unknown";
        // A record is dropped by whatever settles its Operation, whatever kind it was: the
        // record names a path, and `pendingFileOperation` matches by path alone, so one
        // left behind by a move or a commit blocks every later upload there just as a
        // delete would. Only the report is specific to deletions, the work this page shows.
        const settled = operation.state === "succeeded" || ended;
        if (settled) settleFileOperation(operation.id);
        if (operation.kind === "delete" && ended) {
          if (!notifiedDeleteFailures.current.has(operation.id)) {
            notifiedDeleteFailures.current.add(operation.id);
            showToast(t("drive.deleteIncomplete", { path: entry.path }), "error");
            reloadView.current();
          }
          return;
        }
        if (settled) return;
        active.push({ operation, path: entry.path });
      } catch (error) {
        if (error instanceof ApiError && (error.status === 404 || error.status === 410)) {
          settleFileOperation(entry.id);
          return;
        }
        failed = true;
      }
    }));
    if (request !== deletionRequest.current) return;
    setTrackedDeletions(active.filter(({ operation }) => operation.kind === "delete"));
    setDeletionStatusError(failed);
  }, [showToast, t]);

  useEffect(() => {
    setUploadOwner(user.id);
    void refreshDeletions();
    const unsubscribe = subscribeFileOperations(() => { void refreshDeletions(); });
    return () => { deletionRequest.current += 1; unsubscribe(); };
  }, [user.id, refreshDeletions]);

  const activeDeletions = trackedDeletions.some(({ operation }) =>
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
        transferOperation.current = result.id;
        setTransferProgress(result.progress);
        try {
          await waitForOperation(result, setTransferProgress);
        } catch (caught) {
          throw caught instanceof Error ? caught : new Error(t("drive.transferFailed"));
        } finally {
          transferOperation.current = null;
        }
      }
      setTransfer(null);
      showToast(t(mode === "move" ? "drive.moved" : "drive.copied", { name: item.name }));
      if (mode === "move") navigate(filesHref(target), { replace: true });
      else void load(path);
    },
    [load, navigate, path, showToast],
  );

  /** Cancel while the move/copy is still running: the operation stops between entries. */
  const cancelTransfer = useCallback(async () => {
    const id = transferOperation.current;
    if (!id) return;
    setTransferCancelling(true);
    try {
      await api.cancelOperation(id);
    } catch (error) {
      showToast(error instanceof Error ? error.message : t("drive.transferFailed"), "error");
    }
  }, [showToast]);

  // The path is the URL (/files/Photos/2024), so back/forward and direct visits open the right place.
  useEffect(() => {
    void load(path);
  }, [path, load]);

  useEffect(() => {
    setRenaming(false);
    setMenuOpen(false);
    setDeleteTarget(null);
  }, [path]);

  // Progress belongs to the dialog that is open now, never to the previous transfer.
  useEffect(() => {
    setTransferProgress(null);
    setTransferCancelling(false);
  }, [transfer]);

  /**
   * The tree a drop is still reading.
   *
   * A dropped tree hands its files over page by page while it uploads, so the sheet holds
   * only what has been read so far and the reading carries on after the first rows exist.
   * That makes the walk something the page can — and on a cancel, must — stop: the files it
   * reads later are enqueued and started like any other, so a cancel that only reached the
   * queue would be followed by the rest of the tree uploading itself.
   */
  const uploadWalk = useRef<{ controller: AbortController; ids: Set<string> } | null>(null);

  /** Only settled upload resources replace placeholder rows. */
  const handleUploaded = useCallback((task: UploadTask, item: Item) => {
    completedUploads.current += 1;
    // A file that landed is no longer something the walk is still feeding.
    uploadWalk.current?.ids.delete(task.id);
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
    resume: resumeUpload,
    conflict,
    conflictPolicy,
    answerConflict,
    askForConflicts,
    beginImport,
  } = useUploadQueue(handleUploaded);

  // The queue's latest published state, for a gesture that is a prop of every row: reading it
  // from the render would rebuild the callback on every publish, ten times a second, and throw
  // away the rows' memoization while bytes move.
  const tasksRef = useRef<UploadTask[]>([]);
  tasksRef.current = uploads;

  const uploadBlocker = useBlocker(({ nextLocation }) =>
    hasActiveUploads && !/^\/files(?:\/|$)/.test(nextLocation.pathname));

  /**
   * Cancelling a row, answered once.
   *
   * The rows leave the sheet at once — the projection stops showing a requested cancellation
   * — which is what a person who pressed Cancel expects, and it is also why the gesture has
   * to say what it took: the rows that carried "489 / 609" are gone, so nothing on screen
   * would report that 3% of a dropped tree had landed. One sentence does, and only for a
   * gesture over more than one item: cancelling a single row is its own answer.
   *
   * A cancel that covers the tree a drop is still reading also stops the walk. Until it did,
   * the files read after the click were enqueued and started like any other, so pressing
   * Cancel on a 20k-file import cancelled the page it had reached and uploaded the rest.
   */
  const cancelUploadRow = useCallback((ids: string[]) => {
    const summary = cancelUpload(ids);
    // The batch now ends with nothing running, and the page's own notice would announce a
    // cancelled import as a finished one.
    completedUploads.current = 0;
    if (summary && summary.items > 1) {
      showToast(t("drive.uploadCancelledSummary", {
        done: summary.completedItems.toLocaleString(),
        total: summary.items.toLocaleString(),
      }));
    }
    const walk = uploadWalk.current;
    if (walk) {
      for (const id of ids) walk.ids.delete(id);
      // Nothing this walk fed is still waiting to upload, so there is nothing left to read for.
      // A failed row is not waiting either — it is a decision the failure list already asks for.
      // The tasks come from a ref because this callback must not be rebuilt by every publish:
      // it is a prop of every row.
      const feeding = tasksRef.current.some((task) =>
        walk.ids.has(task.id) && task.status !== "completed" && task.status !== "failed");
      if (!feeding) {
        uploadWalk.current = null;
        walk.controller.abort();
      }
    }
    // The rows are gone, and the folder they were describing may have existed only as those
    // rows: the listing is what shows it with the files that did land, so read it again
    // instead of leaving the destination with nothing in it.
    reloadView.current();
  }, [cancelUpload, showToast]);

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
  // A failed delete must put the restored row back; a search is not the folder listing.
  reloadView.current = () => {
    if (activeQuery) void runSearch(activeQuery);
    else void load(listingRef.current.path);
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
  // Retry is a transfer action, not a folder action: the list above the rows owns it.
  const failed = useMemo(() => failedTasks(uploads), [uploads]);
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

  /**
   * Runs the delete and reports it where the deleted item is marked on screen, never in the
   * dialog that is already closed. A recursive delete can take a minute to be *accepted* (the server
   * enumerates the subtree twice before it freezes the manifest), so every failure ends in
   * an actionable toast instead of a thrown error nobody can answer.
   */
  const applyDelete = async (target: Item) => {
    if (deletesInFlight.current.has(target.path)) {
      showToast(t("drive.deleteAlreadyRunning"), "info");
      return;
    }
    if (hasUnfinishedUploads(target.path)) {
      showToast(t("drive.deleteUploadBlocked"), "error", true);
      return;
    }
    deletesInFlight.current.add(target.path);
    setDeletingPaths((paths) => [...paths, target.path]);
    try {
      let operation: FileOperation | void;
      for (let attempt = 0; ; attempt++) {
        try {
          operation = await api.deletePath(target.path, target.kind === "folder", target.revision);
          break;
        } catch (error) {
          if (!(error instanceof ApiError) || error.code !== "operation_in_progress"
            || !error.operationId || attempt >= DELETE_BLOCKER_ATTEMPTS) throw error;
          const blocker = await resolveDeleteBlocker(api, error.operationId);
          if (blocker.resolution === "retry") continue;
          throw new Error(blocker.reason === "upload"
            ? t("drive.deleteBlockedByUpload", { name: blocker.name })
            : t("drive.deleteBlockedByOther"));
        }
      }
      deletionRequest.current += 1;
      // The accepted delete is already the user's final answer; only the native purge
      // is still running, and it is tracked only so a failure can be reported.
      if (operation && operation.state !== "succeeded") trackFileOperation(operation, target.path);
      showToast(t("drive.deleted", { name: target.name }));
      setDeletionStatusError(false);
      // Drop the row before the listing round trip; the reload then confirms it.
      setItems((current) => current.filter((item) => target.kind === "folder"
        ? item.path !== target.path && !item.path.startsWith(`${target.path}/`)
        : item.path !== target.path));
      cancelUpload(uploads
        .filter((task) => filePath(task.path) === target.path || filePath(task.path).startsWith(`${target.path}/`))
        .map((task) => task.id));
      if (filePath(listingRef.current.path) === target.path) {
        navigate(filesHref(target.parentPath ?? "/"), { replace: true });
      } else {
        reloadView.current();
      }
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      // The reason has to outlive a glance: it names the upload or operation that still
      // holds this path, and the item it refers to is still on screen behind the toast.
      showToast(error instanceof Error ? error.message : t("drive.deleteFailed"), "error", true);
    } finally {
      deletesInFlight.current.delete(target.path);
      setDeletingPaths((paths) => paths.filter((path) => path !== target.path));
    }
  };

  /** Confirming is the decision, not the wait: close the question and let the page report. */
  const confirmDelete = (target: Item) => {
    setDeleteTarget(null);
    void applyDelete(target);
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
    // Files are handed to the queue as the walk finds them, so the first bytes move while the
    // rest of the tree is still being read; only the empty folders have to wait for the end.
    //
    // A new drop replaces the walk in flight: two trees reading into one queue would make
    // "cancel this import" mean two different things. Files the earlier walk already handed
    // over keep uploading — those are rows with their own cancel, and their own sentence.
    uploadWalk.current?.controller.abort();
    const walk = { controller: new AbortController(), ids: new Set<string>() };
    uploadWalk.current = walk;
    setReadingUploads(0);
    // A new drop is a new import: the previous one's standing answer to "already exists"
    // was a decision about that selection, and this one will be asked separately.
    beginImport();
    void droppedUploadEntries(event.dataTransfer, {
      signal: walk.controller.signal,
      onProgress: setReadingUploads,
      onFiles: (files) => { for (const id of enqueue(files, path, itemExists)) walk.ids.add(id); },
    })
      .then((entries) => {
        setReadingUploads(null);
        if (uploadWalk.current === walk) uploadWalk.current = null;
        // A folder that holds a file is created by that file, so only fileless folders still
        // need a task of their own — and whether a folder holds a file is known only now.
        const folders = foldersWithoutFiles(entries).filter((entry) => entry.file === null);
        if (folders.length) enqueue(folders, path, itemExists);
      })
      .catch((error) => {
        setReadingUploads(null);
        if (uploadWalk.current === walk) uploadWalk.current = null;
        // A cancelled walk was answered by the cancel itself; only a read that failed has
        // something the person has not been told.
        if (error instanceof DOMException && error.name === "AbortError") return;
        showToast(error instanceof Error ? error.message : t("drive.readUploadFailed"), "error");
      });
  };

  const onFilesSelected = (event: React.ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    event.target.value = "";
    try {
      beginImport();
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
        {downloadMissing && <StorageNotice missingContent />}
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
                      {/* The folder you are inside has no row of its own to carry the wait. */}
                      {isLast && deletingPaths.includes(crumb.path) && (
                        <span className={`${styles.statusBadge} ${styles.statusActive}`}>
                          {t("drive.deletingNow")}
                        </span>
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
              deleteDisabled={pending || Boolean(renameJob) || deleteBlocked
                || deletingPaths.includes(file.path)}
              deleteHint={deleteBlocked ? t("drive.deleteUploadBlocked")
                : deletingPaths.includes(file.path) ? t("drive.deletingNow") : undefined}
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
                <StorageNotice />
              </div>
            </section>
          </>
        ) : (
          <>
            <section ref={panelRef} className={styles.panel} aria-busy={pending}>
              {/* A switch has no skeleton to show, so a hairline at the card's edge carries the wait. */}
              {pending && !firstLoad && <span className={styles.loadLine} aria-hidden="true" />}

              {readingUploads !== null && (
                <div className={styles.readingFiles} role="status">
                  <span className={styles.readingDot} aria-hidden="true" />
                  {t("drive.readingUploadTree", { count: readingUploads.toLocaleString() })}
                </div>
              )}

              {conflictPolicy !== "ask" && hasActiveUploads && (
                <ConflictPolicyNote policy={conflictPolicy} onAskAgain={askForConflicts} />
              )}

              <UploadFailures tasks={failed} onRetry={retryUpload} onCancel={cancelUploadRow} />

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
                      onCancel={cancelUploadRow}
                      onResume={resumeUpload}
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
                    isUploadRowActive(entry) ? (
                      <UploadRow
                        key={entry.path}
                        entry={entry}
                        onCancel={cancelUploadRow}
                        onResume={resumeUpload}
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
          progress={transferProgress}
          cancelling={transferCancelling}
          onCancel={() => setTransfer(null)}
          onCancelOperation={() => void cancelTransfer()}
          onConfirm={(destination) => applyTransfer(transfer.mode, transfer.item, destination)}
        />
      )}

      {conflict !== null && (
        // Keyed by path so a second question mounts a fresh dialog: the first one's modal
        // state belongs to the file that was answered.
        <ConflictConfirmation key={conflict} path={conflict} onAnswer={answerConflict} />
      )}

      {deleteTarget && (
        <DeleteConfirmation
          key={deleteTarget.path}
          target={deleteTarget}
          onCancel={() => setDeleteTarget(null)}
          onConfirm={() => confirmDelete(deleteTarget)}
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
