/**
 * An ephemeral upload queue. The application API owns native publication and
 * index finalization. Refreshing discards remaining local tasks.
 */
import { useCallback, useRef, useState } from "react";
import { ApiError, api, type Item } from "../api/client";
import { t } from "../i18n";
import type { UploadEntry } from "./uploadSelection";

const MAX_PARALLEL_UPLOADS = 1;

/**
 * Smoothed upload speed.
 *
 * Measure bytes advanced over the last few seconds using actual elapsed time.
 * Progress remains monotonic within one request and resets for a new transfer.
 */
const SPEED_WINDOW_MS = 3000;

class SpeedMeter {
  private samples: { at: number; loaded: number }[] = [];

  /** Record a progress sample; return monotonic loaded bytes and smoothed speed (bytes/s). */
  push(rawLoaded: number, now: number): { loaded: number; speed: number } {
    const last = this.samples[this.samples.length - 1];
    const loaded = last ? Math.max(last.loaded, rawLoaded) : rawLoaded;
    if (!last || loaded > last.loaded) this.samples.push({ at: now, loaded });

    const cutoff = now - SPEED_WINDOW_MS;
    while (this.samples.length > 1 && this.samples[1].at <= cutoff) this.samples.shift();

    const first = this.samples[0];
    // Use now, not the last sample, so a stalled upload decays to 0 instead of freezing.
    const span = Math.max(now - first.at, 300);
    return { loaded, speed: ((loaded - first.loaded) / span) * 1000 };
  }
}

export type UploadStatus = "queued" | "uploading" | "finalizing" | "failed" | "completed";

export interface UploadTask {
  id: string;
  file: File | null;
  isDir: boolean;
  name: string;
  /** Directory the upload started in; decides which directory shows this placeholder row. */
  dir: string;
  path: string;
  size: number;
  overwrite: boolean;
  status: UploadStatus;
  loaded: number;
  /** Bytes per second; only meaningful while uploading. */
  speed: number;
  /** The browser sent the body; server finalization may still be pending. */
  uploaded: boolean;
  /** A retry checks this receipt before it may allocate another transfer. */
  uploadId?: string;
  error?: string;
}

interface UploadRuntime {
  canceled: boolean;
  controller: AbortController;
}

let taskSeq = 0;

function newTaskId(): string {
  taskSeq += 1;
  return `upload-${Date.now().toString(36)}-${taskSeq.toString(36)}`;
}

/** Preserve a readable API or transport error in the upload row. */
function errorMessage(error: unknown): string {
  if (error instanceof Error && error.message) return error.message;
  if (typeof error === "string" && error) return error;
  if (error && typeof error === "object") {
    const record = error as Record<string, unknown>;
    if (typeof record.message === "string" && record.message) return record.message;
    if (typeof record.error === "string" && record.error) return record.error;
    if (record.error && typeof record.error === "object") {
      const nested = (record.error as Record<string, unknown>).Message;
      if (typeof nested === "string" && nested) return nested;
    }
    if (typeof record.code === "string" && record.code) return record.code;
  }
  return t("error.unknown");
}

export interface UploadQueue {
  tasks: UploadTask[];
  enqueue: (
    entries: UploadEntry[],
    dir: string,
    exists: (path: string) => boolean,
  ) => void;
  cancel: (id: string) => void;
  retry: (id: string) => void;
}

export function useUploadQueue(
  onUploaded: (task: UploadTask, item: Item) => void,
): UploadQueue {
  const [tasks, setTasks] = useState<UploadTask[]>([]);
  const tasksRef = useRef<UploadTask[]>([]);
  const runningRef = useRef(0);
  const pumpingRef = useRef(false);
  const pumpRef = useRef<() => void>(() => {});
  const runtimesRef = useRef(new Map<string, UploadRuntime>());
  const metersRef = useRef(new Map<string, SpeedMeter>());
  const onUploadedRef = useRef(onUploaded);
  onUploadedRef.current = onUploaded;

  /** Update ref and state together; async flows always read tasksRef. */
  const update = useCallback((mutate: (current: UploadTask[]) => UploadTask[]) => {
    const next = mutate(tasksRef.current);
    tasksRef.current = next;
    setTasks(next);
  }, []);

  const run = useCallback(
    async (id: string) => {
      const runtime: UploadRuntime = { canceled: false, controller: new AbortController() };
      runtimesRef.current.set(id, runtime);

      try {
        const task = tasksRef.current.find((entry) => entry.id === id);
        if (!task || task.status !== "uploading") return;

        if (!task.file) {
          // Merge an existing directory, but never turn a file into a directory.
          try {
            await api.mkdir(task.path);
          } catch (error) {
            if (!(error instanceof ApiError) || error.status !== 409) throw error;
          }
          if (runtime.canceled) return;
          const { node } = await api.list(task.path);
          if (!node?.is_dir) throw new Error("a file with the same name already exists");
          if (runtime.canceled) return;
          update((current) => current.map((entry) => entry.id === id
            ? { ...entry, status: "completed", file: null, loaded: entry.size, speed: 0 }
            : entry));
          onUploadedRef.current(task, node);
          return;
        }

        const file = task.file;
        let item: Item | undefined;
        if (task.uploadId) {
          // A lost response can hide native success. Ask for the recorded result
          // before allocating or sending any replacement request body.
          update((current) => current.map((entry) => entry.id === id
            ? { ...entry, status: "finalizing", speed: 0 }
            : entry));
          try {
            item = await api.confirmUpload(task.uploadId);
          } catch (error) {
            if (!(error instanceof ApiError) || (error.status !== 404 && error.status !== 410)) throw error;
            update((current) => current.map((entry) => entry.id === id
              ? { ...entry, uploadId: undefined, uploaded: false, loaded: 0, status: "uploading" }
              : entry));
          }
          if (runtime.canceled) return;
        }

        if (!item) {
          let sign;
          try {
            sign = await api.signUpload(task.path, task.size, task.overwrite, file.type || undefined, file.lastModified);
          } catch (error) {
            if (!(error instanceof ApiError) || error.status !== 409 || task.overwrite) throw error;
            const { node } = await api.list(task.path);
            if (runtime.canceled) return;
            if (!node || node.is_dir) throw error;
            if (!window.confirm(t("drive.overwriteConfirm", { name: task.name }))) {
              update((current) => current.filter((entry) => entry.id !== id));
              return;
            }
            update((current) => current.map((entry) => entry.id === id
              ? { ...entry, overwrite: true }
              : entry));
            sign = await api.signUpload(task.path, task.size, true, file.type || undefined, file.lastModified);
          }
          if (runtime.canceled) return;
          update((current) => current.map((entry) => entry.id === id
            ? { ...entry, uploadId: sign.uploadId, uploaded: false, loaded: 0, status: "uploading" }
            : entry));
          item = await api.uploadFile(
            sign.uploadId,
            file,
            (progress) => {
              if (runtime.canceled) return;
              let meter = metersRef.current.get(id);
              if (!meter) {
                meter = new SpeedMeter();
                metersRef.current.set(id, meter);
              }
              const sample = meter.push(progress.loaded, performance.now());
              update((current) => current.map((entry) => entry.id === id && entry.status === "uploading"
                ? { ...entry, loaded: Math.min(entry.size, sample.loaded), speed: sample.speed }
                : entry));
            },
            () => {
              if (runtime.canceled) return;
              update((current) => current.map((entry) => entry.id === id
                ? { ...entry, status: "finalizing", uploaded: true, loaded: entry.size, speed: 0 }
                : entry));
            },
            runtime.controller.signal,
          );
        }
        if (runtime.canceled) return;

        update((current) => current.map((entry) => entry.id === id
          ? { ...entry, status: "completed", file: null, loaded: entry.size, speed: 0 }
          : entry));
        onUploadedRef.current(task, item);
      } catch (error) {
        if (runtime.canceled) return;
        update((current) =>
          current.map((entry) =>
            entry.id === id
              ? { ...entry, status: "failed", speed: 0, error: errorMessage(error) }
              : entry,
          ),
        );
      } finally {
        runtimesRef.current.delete(id);
        metersRef.current.delete(id);
        runningRef.current = Math.max(0, runningRef.current - 1);
        pumpRef.current();
      }
    },
    [update],
  );

  const pump = useCallback(() => {
    if (pumpingRef.current) return;
    pumpingRef.current = true;
    try {
      while (runningRef.current < MAX_PARALLEL_UPLOADS) {
        const next = tasksRef.current.find((task) =>
          task.status === "queued" && !tasksRef.current.some((parent) =>
            parent.isDir && parent.status !== "completed" && parent.id !== task.id && task.path.startsWith(`${parent.path}/`),
          ),
        );
        if (!next) break;
        runningRef.current += 1;
        // A retry restarts from 0 or from the bytes already sent, so drop the speed window too.
        metersRef.current.delete(next.id);
        update((current) =>
          current.map((task) =>
            task.id === next.id
              ? {
                  ...task,
                  status: "uploading",
                  loaded: task.uploaded ? task.size : 0,
                  speed: 0,
                  error: undefined,
                }
              : task,
          ),
        );
        void run(next.id);
      }
    } finally {
      pumpingRef.current = false;
    }
  }, [run, update]);
  pumpRef.current = pump;

  const cancel = useCallback(
    (id: string) => {
      const target = tasksRef.current.find((task) => task.id === id);
      const canceled = new Set(tasksRef.current.filter((task) =>
        task.id === id || (target?.isDir && task.path.startsWith(`${target.path}/`)),
      ).map((task) => task.id));
      for (const canceledId of canceled) {
        const runtime = runtimesRef.current.get(canceledId);
        if (!runtime) continue;
        runtime.canceled = true;
        runtime.controller.abort();
      }
      update((current) => current.filter((task) => !canceled.has(task.id)));
      pumpRef.current();
    },
    [update],
  );

  const retry = useCallback(
    (id: string) => {
      update((current) =>
        current.map((task) =>
          task.id === id && task.status === "failed"
            ? {
                ...task,
                status: "queued",
                loaded: task.uploaded ? task.size : 0,
                speed: 0,
                error: undefined,
              }
            : task,
        ),
      );
      pump();
    },
    [pump, update],
  );

  const enqueue = useCallback(
    (entries: UploadEntry[], dir: string, exists: (path: string) => boolean) => {
      if (!entries.length) return;
      if (tasksRef.current.every((task) => task.status === "completed")) update(() => []);

      const added: UploadTask[] = [];
      for (const { file, path: name } of entries) {
        const path = dir ? `${dir}/${name}` : name;
        const duplicate = tasksRef.current.find((task) => task.path === path && task.status !== "completed");

        let overwrite = false;
        if (duplicate && !file && !duplicate.file) continue;
        if (duplicate) {
          if (!window.confirm(t("drive.overwriteConfirm", { name }))) continue;
          cancel(duplicate.id);
          overwrite = true;
        } else if (file && exists(path)) {
          if (!window.confirm(t("drive.overwriteConfirm", { name }))) continue;
          overwrite = true;
        }

        added.push({
          id: newTaskId(),
          file,
          isDir: file === null,
          name,
          dir,
          path,
          size: file?.size ?? 0,
          overwrite,
          status: "queued",
          loaded: 0,
          speed: 0,
          uploaded: false,
        });
      }

      if (!added.length) return;
      const addedPaths = new Set(added.map((task) => task.path));
      update((current) => [...current.filter((task) => task.status !== "completed" || !addedPaths.has(task.path)), ...added]);
      pump();
    },
    [cancel, pump, update],
  );

  return { tasks, enqueue, cancel, retry };
}
