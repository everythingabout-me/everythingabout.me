import type { Item } from "../api/client";
import type { UploadStatus, UploadTask } from "./useUploadQueue";

export interface UploadSummary {
  status: UploadStatus;
  files: number;
  completedFiles: number;
  failed: number;
  size: number;
  loaded: number;
  speed: number;
  percent: number;
  cancelIds: string[];
  retryIds: string[];
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

/** Project every selected descendant onto its immediate child in the browsed directory. */
export function buildUploadRows(items: Item[], tasks: UploadTask[], directory: string): UploadRowEntry[] {
  const dir = normalize(directory);
  const prefix = dir ? `${dir}/` : "";
  const rows = new Map<string, UploadRowEntry>();
  const groups = new Map<string, UploadTask[]>();
  for (const item of items) {
    if (normalize(item.parentPath) !== dir) continue;
    const path = normalize(item.path);
    rows.set(path, { path, name: item.name, isDir: item.kind === "folder", item });
  }
  // Latest metadata wins if the same path is selected again in an active batch.
  const unique = new Map(tasks.map((task) => [normalize(task.path), task]));
  for (const [path, task] of unique) {
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
  for (const [path, grouped] of groups) {
    const row = rows.get(path)!;
    const files = grouped.filter((task) => !task.isDir);
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
    const status: UploadStatus = failed.length ? "failed"
      : pending.some((task) => task.status === "uploading") ? "uploading"
        : pending.some((task) => task.status === "finalizing")
          ? row.isDir && pending.some((task) => task.status === "queued") ? "uploading" : "finalizing"
          : waiting.length ? "waiting" : pending.length ? "queued" : "completed";
    const cancelable = pending.filter((task) => !task.cancelRequested);
    row.upload = {
      status,
      files: files.length,
      completedFiles,
      failed: failed.length,
      size,
      loaded,
      speed: files.reduce((sum, task) => sum + (task.status === "uploading" ? task.speed : 0), 0),
      percent: pending.length === 0 ? 100 : size > 0 ? Math.floor(loaded / size * 100)
        : files.length > 0 ? Math.floor(completedFiles / files.length * 100) : 0,
      cancelIds: cancelable.length === 0 ? [] : row.task?.isDir ? [row.task.id] : cancelable.map((task) => task.id),
      retryIds: failed.map((task) => task.id),
      ...(waitingReason?.waitMessage ? { waitMessage: waitingReason.waitMessage } : {}),
    };
  }
  return [...rows.values()].sort(compareRows);
}
