/**
 * The upload queue.
 *
 * Every file first becomes a placeholder task (queued), and only one file uploads directly at a
 * time. The browser sends the bytes straight to COS with STS temporary credentials; the server
 * only confirms once at the end and writes the index.
 *
 * The queue deliberately does not live inside React state updaters: uploading is a concurrent
 * async flow, so it needs a synchronously readable tasksRef to decide "who goes next", while
 * tasks exists only for rendering.
 */
import { useCallback, useRef, useState } from "react";
import { api, type Item } from "../api/client";
import { t } from "../i18n";

/**
 * How many uploads run at once.
 *
 * One file at a time: two in parallel just split the bandwidth in half, keeping the total duration
 * while making both progress bars crawl. Serial also lets a single part soak up the bandwidth, so
 * a file that finishes is really finished.
 */
const MAX_PARALLEL_UPLOADS = 1;
/**
 * How many parts of a single file upload at once.
 *
 * Browsers allow at most 6 connections per host; 1 file x 4 parts takes 4 of them and leaves the
 * rest for listing, sign and confirm calls. More parts only queue up and make progress stutter.
 * Parts are Blob views that copy no bytes, so 4 x 128MB in flight costs no extra memory.
 */
const CHUNK_PARALLEL_LIMIT = 4;

const MB = 1024 * 1024;
/**
 * Minimum part size. Smaller parts live inside TCP slow start and still pay their own round trip
 * plus signature, which is a net loss; so if a file cannot yield parts of at least 16MB, do not
 * split it.
 *
 * Multipart also pays three round trips up front — ListMultipartUploads to probe for a resumable
 * upload, InitiateMultipartUpload, then CompleteMultipartUpload — about 100ms at 35ms RTT. A 32MB
 * single stream finishes in 0.3–1s on domestic links, while splitting it into two 8–16MB parts is
 * actually slower.
 */
const MIN_SLICE_SIZE = 16 * MB;
/**
 * Maximum part size. A failed part (the SDK retries twice) must be re-sent whole, and 128MB is
 * already an acceptable grain; 4 x 128MB means 512MB in flight (Blob views, no byte copies, no
 * extra memory).
 */
const MAX_SLICE_SIZE = 128 * MB;

/**
 * Part count: guarantee at least 16MB per part, with at most 4 parts per round; above 512MB a
 * single round cannot hold the file, so add rounds, still 4 parts each.
 *
 *   parts = min(4, floor(size / 16MB))     size <= 512MB
 *   parts = 4 * ceil(size / 512MB)         size > 512MB
 *
 * Why a multiple of 4: with a fixed 128MB slice size, 600MB becomes 128x4 + 88, and that last
 * 88MB runs on a single connection, so the tail uses only a quarter of the bandwidth. Splitting
 * evenly in multiples of 4 (600MB -> 8 x 75MB) gives every connection the same work as
 * 128x4 + 22x4, but every round is full and there is no 22MB stub that ends just as it starts.
 */
function partCountFor(size: number): number {
  const perRound = MAX_SLICE_SIZE * CHUNK_PARALLEL_LIMIT;
  if (size <= perRound) {
    return Math.min(CHUNK_PARALLEL_LIMIT, Math.floor(size / MIN_SLICE_SIZE));
  }
  return CHUNK_PARALLEL_LIMIT * Math.ceil(size / perRound);
}

/**
 * Pick the slice size from the file size.
 *
 *   <= 32MB      no split, a single putObject (two parts would be only 8–16MB, below the minimum)
 *   32–48MB      2 parts
 *   48–64MB      3 parts
 *   64MB–512MB   4 parts (16–128MB each)
 *   512MB–1GB    8 parts (two rounds of 4)
 *   over 1GB     one more round per extra 512MB
 *
 * 48GB is 384 parts over 96 rounds, still far from the COS limit of 10000 parts. Lower
 * MAX_SLICE_SIZE for a finer grain; lower MIN_SLICE_SIZE to 8MB to parallelize below 32MB too.
 * The SDK only slices when size > SliceSize, so returning size itself naturally means a single PUT.
 */
function sliceSizeFor(size: number): number {
  const parts = partCountFor(size);
  if (parts <= 1) return Math.max(size, MIN_SLICE_SIZE);
  // Round up in bytes so ceil(size / slice) never exceeds the computed part count.
  return Math.ceil(size / parts);
}

/**
 * Smoothed upload speed.
 *
 * The SDK's info.speed is the instant delta between two progress callbacks (300ms), so any network
 * jitter bounces it between 0 and the peak. Use "bytes advanced over the last few seconds divided
 * by the actual elapsed time" instead, and only count forward progress: the SDK pulls loaded back
 * when a part retry fails.
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

export type UploadStatus = "queued" | "uploading" | "finalizing" | "failed";

export interface UploadTask {
  id: string;
  file: File;
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
  /** The bytes are all up, only confirm is missing. A retry can skip re-sending them. */
  uploaded: boolean;
  error?: string;
}

interface UploadRuntime {
  canceled: boolean;
  taskId?: string;
  cos?: CancelableCos;
  target?: { bucket: string; region: string; key: string };
  /** Cancellation uses it to reject the pending transfer Promise outright. */
  abortTransfer?: () => void;
}

/** Declare only the two methods the cancel flow needs, so the whole SDK type stays out. */
interface CancelableCos {
  cancelTask(taskId: string): void;
  abortUploadTask(params: {
    Bucket: string;
    Region: string;
    Key: string;
    Level: "file";
  }): Promise<unknown>;
}

class UploadCanceledError extends Error {
  constructor() {
    super("upload canceled");
    this.name = "UploadCanceledError";
  }
}

let taskSeq = 0;

function newTaskId(): string {
  taskSeq += 1;
  return `upload-${Date.now().toString(36)}-${taskSeq.toString(36)}`;
}

/** COS SDK errors come in three historical shapes; try each for a readable message. */
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
    files: FileList | File[] | null,
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
      const runtime: UploadRuntime = { canceled: false };
      runtimesRef.current.set(id, runtime);

      try {
        const task = tasksRef.current.find((entry) => entry.id === id);
        if (!task || task.status !== "uploading") return;

        if (task.uploaded) {
          // The previous round uploaded every byte and only confirm failed; do not send it again.
          update((current) =>
            current.map((entry) =>
              entry.id === id ? { ...entry, status: "finalizing", speed: 0 } : entry,
            ),
          );
        } else {
          // The COS SDK is only needed once an upload really starts: load it on demand, which
          // keeps the first screen small.
          const { default: COS } = await import("cos-js-sdk-v5");
          if (runtime.canceled) return;

          const sign = await api.signUpload(task.path, task.size, task.overwrite);
          if (runtime.canceled) return;

          runtime.target = { bucket: sign.bucket, region: sign.region, key: sign.key };
          const cos = new COS({
            getAuthorization: (_options, callback) =>
              callback({
                TmpSecretId: sign.credentials.tmpSecretId,
                TmpSecretKey: sign.credentials.tmpSecretKey,
                SecurityToken: sign.credentials.sessionToken,
                StartTime: sign.credentials.startTime,
                ExpiredTime: sign.credentials.expiredTime,
              }),
            ProgressInterval: 300,
          });
          runtime.cos = cos as unknown as CancelableCos;

          await new Promise<void>((resolve, reject) => {
            runtime.abortTransfer = () => reject(new UploadCanceledError());
            cos.uploadFile(
              {
                Bucket: sign.bucket,
                Region: sign.region,
                Key: sign.key,
                Body: task.file,
                SliceSize: sliceSizeFor(task.size),
                AsyncLimit: CHUNK_PARALLEL_LIMIT,
                onTaskReady: (taskId) => {
                  runtime.taskId = taskId;
                },
                onTaskStart: () => {
                  // If cancel happened before the task actually started, finish it off here.
                  if (runtime.canceled && runtime.taskId) cos.cancelTask(runtime.taskId);
                },
                onProgress: (info) => {
                  if (runtime.canceled) return;
                  let meter = metersRef.current.get(id);
                  if (!meter) {
                    meter = new SpeedMeter();
                    metersRef.current.set(id, meter);
                  }
                  const sample = meter.push(info.loaded, performance.now());
                  update((current) =>
                    current.map((entry) =>
                      entry.id === id && entry.status === "uploading"
                        ? {
                            ...entry,
                            loaded: Math.min(entry.size, sample.loaded),
                            speed: sample.speed,
                          }
                        : entry,
                    ),
                  );
                },
              },
              (error) => {
                if (error) reject(new Error(errorMessage(error)));
                else resolve();
              },
            );
          });
          if (runtime.canceled) return;

          update((current) =>
            current.map((entry) =>
              entry.id === id
                ? {
                    ...entry,
                    status: "finalizing",
                    loaded: entry.size,
                    speed: 0,
                    uploaded: true,
                  }
                : entry,
            ),
          );
        }

        const item = await api.confirmUpload(
          task.path,
          task.file.type || undefined,
          undefined,
          task.file.lastModified,
        );
        if (runtime.canceled) return;

        update((current) => current.filter((entry) => entry.id !== id));
        onUploadedRef.current(task, item);
      } catch (error) {
        if (runtime.canceled || error instanceof UploadCanceledError) return;
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
        const next = tasksRef.current.find((task) => task.status === "queued");
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
      const runtime = runtimesRef.current.get(id);
      if (runtime) {
        runtime.canceled = true;
        try {
          runtime.abortTransfer?.();
          if (runtime.cos) {
            if (runtime.taskId) runtime.cos.cancelTask(runtime.taskId);
            if (runtime.target) {
              // A canceled multipart upload leaves fragments on the COS side; clean up best-effort,
              // and failure does not affect the UI.
              void runtime.cos
                .abortUploadTask({
                  Bucket: runtime.target.bucket,
                  Region: runtime.target.region,
                  Key: runtime.target.key,
                  Level: "file",
                })
                .catch(() => {});
            }
          }
        } catch {
          // Cancellation is best effort: an SDK error must not leave the placeholder row on screen.
        }
      }
      update((current) => current.filter((task) => task.id !== id));
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
    (files: FileList | File[] | null, dir: string, exists: (path: string) => boolean) => {
      const list = files ? Array.from(files) : [];
      if (!list.length) return;

      const added: UploadTask[] = [];
      for (const file of list) {
        const name = file.name.normalize("NFC");
        const path = dir ? `${dir}/${name}` : name;
        const duplicate = tasksRef.current.find((task) => task.path === path);

        let overwrite = false;
        if (duplicate) {
          if (!window.confirm(t("drive.overwriteConfirm", { name }))) continue;
          cancel(duplicate.id);
          overwrite = true;
        } else if (exists(path)) {
          if (!window.confirm(t("drive.overwriteConfirm", { name }))) continue;
          overwrite = true;
        }

        added.push({
          id: newTaskId(),
          file,
          name,
          dir,
          path,
          size: file.size,
          overwrite,
          status: "queued",
          loaded: 0,
          speed: 0,
          uploaded: false,
        });
      }

      if (!added.length) return;
      update((current) => [...current, ...added]);
      pump();
    },
    [cancel, pump, update],
  );

  return { tasks, enqueue, cancel, retry };
}
