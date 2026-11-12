import type { Item } from "../api/client";
import type { UploadStatus, UploadTask } from "./useUploadQueue";

export interface UploadSummary {
  status: UploadStatus;
  files: number;
  completedFiles: number;
  folders: number;
  completedFolders: number;
  /** Folders and files together; a large tree makes progress in these, not in bytes. */
  items: number;
  completedItems: number;
  progressUnit: "items" | "bytes";
  /** Still creating the destination folders, so no file byte has moved yet. */
  preparing: boolean;
  /** Started files are all waiting for a slot in the one global part pool. */
  waitingForSlot: boolean;
  failed: number;
  size: number;
  loaded: number;
  speed: number;
  percent: number;
  cancelIds: string[];
  /**
   * Tasks waiting on a deletion that ended without deleting anything.
   *
   * Such a wait cannot end by itself (the Operation is terminal, so its guards are gone
   * and no worker will finish it later), so the row has to offer the one action that can
   * clear the browser's record. Naming them here keeps the decision in the row that is
   * already explaining it, instead of a Retry that would only wait again.
   */
  resumeIds: string[];
  waitMessage?: string;
}

export interface UploadRowEntry {
  path: string;
  name: string;
  isDir: boolean;
  item?: Item;
  task?: UploadTask;
  upload?: UploadSummary;
}

function normalize(path: string): string {
  return path.normalize("NFC").replace(/^\/+|\/+$/g, "");
}

/** Includes implicit parents and completed folders while their batch metadata is retained. */
export function isUploadDirectory(tasks: UploadTask[], path: string): boolean {
  const target = normalize(path);
  return tasks.some((task) => {
    const taskPath = normalize(task.path);
    return (task.isDir && taskPath === target) || taskPath.startsWith(`${target}/`);
  });
}

function compareRows(a: UploadRowEntry, b: UploadRowEntry): number {
  if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
  const left = a.name.toLowerCase();
  const right = b.name.toLowerCase();
  if (left !== right) return left < right ? -1 : 1;
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

/**
 * The state a row may show for one task.
 *
 * A started file with no part slot is waiting its turn: the pool serves the file ahead of
 * it in the queue first, so nothing moves for it — and nothing has been sent, which is
 * exactly what a person calls "queued". The exception is a file that has already sent
 * bytes and then gave its slot up to a file ahead of it: calling that queued would read as
 * a restart, so it keeps its uploading status, and its row names the wait from the slots
 * it holds instead of showing "0 B/s", which alone reads as a stalled transfer.
 */
function visibleStatus(task: UploadTask): UploadStatus {
  if (task.isDir || task.status !== "uploading") return task.status;
  return task.activeParts > 0 || task.loaded > 0 ? "uploading" : "queued";
}

/**
 * Whether the row is an upload still worth describing rather than a listing entry.
 *
 * A finished batch stays in the queue after its last file lands: its counts answer
 * `isUploadDirectory` and the parent rows of a later batch, and a folder the upload
 * created stays navigable until the index lists it. But once the listing itself carries
 * the entry, that entry is the row — its own name, its own time, its own size rule — and
 * the batch numbers describe history, not state. A completed folder used to keep the
 * upload row for as long as the page stayed open, so a folder that had just received
 * files sat in its parent reading "2 files · 2 uploaded" with the batch total where every
 * sibling shows a timestamp, which reads as an upload that never finished.
 */
export function isUploadRowActive(entry: UploadRowEntry): boolean {
  if (!entry.upload) return false;
  return entry.upload.status !== "completed" || !entry.item;
}

/**
 * Every failed task, in path order — the set the failure list offers to retry.
 *
 * A failure is noticed on a folder row but can never be handled there: the file may sit
 * twenty thousand items down a tree the person is not looking at, so a row that answered
 * "3 failed" with a Retry button asked them to act on work they could not see. This
 * projection is where retry lives, and it deliberately stays a view of the queue rather than
 * a record of its own: the tab is the whole lifetime of a failure, so a refresh clears it
 * exactly like every other transfer, with nothing left behind to disagree with the folder.
 */
export function failedTasks(tasks: UploadTask[]): UploadTask[] {
  return tasks.filter((task) => task.status === "failed").sort((a, b) => {
    const left = normalize(a.path).toLowerCase();
    const right = normalize(b.path).toLowerCase();
    if (left !== right) return left < right ? -1 : 1;
    return a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
  });
}

/** Project every selected descendant onto its immediate child in the browsed directory. */
export function buildUploadRows(items: Item[], tasks: UploadTask[], directory: string): UploadRowEntry[] {
  const dir = normalize(directory);
  const prefix = dir ? `${dir}/` : "";
  const rows = new Map<string, UploadRowEntry>();
  const groups = new Map<string, UploadTask[]>();
  for (const item of items) {
    if (item.parentPath === null || normalize(item.parentPath) !== dir) continue;
    const path = normalize(item.path);
    rows.set(path, { path, name: item.name, isDir: item.kind === "folder", item });
  }
  // Latest metadata wins if the same path is selected again in an active batch.
  const unique = new Map(tasks.map((task) => [normalize(task.path), task]));
  for (const [path, task] of unique) {
    // A cancelled task is no longer part of the import. Cancelling is a decision about the
    // gesture, not a state one row can be in: the sheet answers it by letting the row go at
    // once, so everything left on screen describes bytes that are still going to land.
    //
    // Keeping the row was what a large import showed after a cancel — a folder reading
    // "Finishing" over a shrunken subset, 489 / 609, 80%, where 609 was whatever had not
    // been torn down yet rather than the tree the person dropped. The numbers could only
    // move: each teardown removed a row, so the denominator fell under the bar and the
    // percentage rose or fell with it, and the row finally settled as "completed" — a
    // cancelled 20k-file import announcing itself as a finished 489-file one.
    //
    // A teardown that then fails keeps its task, as a `failed` one: `failedTasks` lists it
    // with a retry, because a session still open holds its reservation against the owner's
    // quota and needs the person's decision, not a row that pretends to be progress.
    if (task.cancelRequested) continue;
    if (!path.startsWith(prefix)) continue;
    const relative = path.slice(prefix.length);
    if (!relative) continue;
    const slash = relative.indexOf("/");
    const name = slash < 0 ? relative : relative.slice(0, slash);
    const childPath = `${prefix}${name}`;
    let row = rows.get(childPath);
    if (!row) {
      row = { path: childPath, name, isDir: slash >= 0 || task.isDir };
      rows.set(childPath, row);
    }
    if (path === childPath) row.task = task;
    const grouped = groups.get(childPath);
    if (grouped) grouped.push(task);
    else groups.set(childPath, [task]);
  }
  // A slot held anywhere else is the only thing a start can wait behind. One fact per
  // projection, not one scan per row: every row asks the same question.
  const slotHeldElsewhere = tasks.some((task) => task.status === "uploading" && task.activeParts > 0);
  for (const [path, grouped] of groups) {
    const row = rows.get(path)!;
    const files = grouped.filter((task) => !task.isDir);
    // The row itself already stands for the selected folder; its descendants are what the
    // counts describe, so the numbers under the name add up to the items beside the bar.
    const self = row.task?.isDir ? row.task : undefined;
    const folders = grouped.filter((task) => task.isDir && task !== self);
    const pending = grouped.filter((task) => task.status !== "completed");
    const failed = pending.filter((task) => task.status === "failed");
    const waiting = pending.filter((task) => task.status === "waiting");
    const waitingReason = waiting.find((task) => task.waitingFor?.statusUnavailable)
      ?? waiting.find((task) => task.waitingFor?.state === "failed" || task.waitingFor?.state === "cancelled")
      ?? waiting[0];
    const size = files.reduce((sum, task) => sum + task.size, 0);
    const loaded = files.reduce((sum, task) => sum + (
      task.status === "completed" ? task.size : Math.max(0, Math.min(task.size, task.loaded))
    ), 0);
    const completedFiles = files.filter((task) => task.status === "completed").length;
    const completedFolders = folders.filter((task) => task.status === "completed").length;
    // A handful of files is a byte problem: the transfer itself is what moves. A selected
    // tree is an item problem: creating its folders moves no byte at all, and a long folder
    // phase would otherwise hold an empty 0% bar over work that is very much happening.
    // Folders therefore count as items once a batch is large enough for the count to be
    // the honest unit, and the row reports the same unit it counts.
    const items = folders.length + files.length;
    const completedItems = completedFolders + completedFiles;
    // A batch of empty files moves no byte either, so count its items instead.
    const progressUnit: "items" | "bytes" = items >= 20 || size === 0 ? "items" : "bytes";
    // The child rows wait for their directory, so no file has started while folders are pending.
    const fileStarted = files.some((task) =>
      task.status === "uploading" || task.status === "finalizing" || task.status === "completed");
    const preparing = folders.length > 1 && completedFolders < folders.length && !fileStarted && failed.length === 0;
    // A batch row speaks for its whole selection, so one started file keeps it uploading.
    // `visibleStatus` names one file's own wait for a part slot, which is right for a file
    // row; a folder that adopted it fell back to "queued" for the few hundred milliseconds
    // between a wave of files being settled and the next wave's sessions being admitted.
    // No wave boundary holds a slot, so a 23k-file import flickered to "queued" and back on
    // every one of them, and the word a person reads there is "it has not started" — the
    // one reading an import that is merely between waves must not produce.
    const running = (task: UploadTask) => row.isDir
      ? task.status === "uploading"
      : visibleStatus(task) === "uploading";
    const status: UploadStatus = failed.length ? "failed"
      : pending.some(running) ? "uploading"
        : pending.some((task) => visibleStatus(task) === "finalizing")
          ? row.isDir && pending.some((task) => visibleStatus(task) === "queued") ? "uploading" : "finalizing"
          : waiting.length ? "waiting" : pending.length ? "queued" : "completed";
    const cancelable = pending.filter((task) => !task.cancelRequested);
    row.upload = {
      status,
      files: files.length,
      completedFiles,
      folders: folders.length,
      completedFolders,
      items,
      completedItems,
      progressUnit,
      preparing,
      // A row that still says uploading while none of its own files holds a slot is
      // waiting for another file to finish; only then is the wait worth naming.
      waitingForSlot: status === "uploading" && slotHeldElsewhere
        && files.some((task) => task.status === "uploading" && task.activeParts === 0)
        && files.every((task) => task.status !== "uploading" || task.activeParts === 0),
      failed: failed.length,
      size,
      loaded,
      speed: files.reduce((sum, task) => sum + (visibleStatus(task) === "uploading" ? task.speed : 0), 0),
      percent: pending.length === 0 ? 100
        : progressUnit === "items" ? (items > 0 ? Math.floor(completedItems / items * 100) : 0)
          : size > 0 ? Math.floor(loaded / size * 100) : 0,
      cancelIds: cancelable.length === 0 ? [] : row.task?.isDir ? [row.task.id] : cancelable.map((task) => task.id),
      resumeIds: waiting
        .filter((task) => task.waitingFor?.state === "failed" || task.waitingFor?.state === "cancelled")
        .map((task) => task.id),
      ...(waitingReason?.waitMessage ? { waitMessage: waitingReason.waitMessage } : {}),
    };
  }
  return [...rows.values()].sort(compareRows);
}
