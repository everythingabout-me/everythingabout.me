/**
 * 上传队列。
 *
 * 每个文件先变成一条占位任务（排队中），一次只直传一个文件。字节通过
 * STS 临时凭证由浏览器直接传给 COS，服务端只在最后 confirm 一次落索引。
 *
 * 这里刻意不把队列放进 React state 的更新函数里：上传是并发的异步流程，
 * 需要一个同步可读的 tasksRef 来判断"下一个该传谁"，tasks 只负责渲染。
 */
import { useCallback, useRef, useState } from "react";
import { api, type Item } from "../api/client";
import { t } from "../i18n";

/**
 * 同时进行的上传数。
 *
 * 一次只传一个文件：两个文件并行只会把带宽对半劈开，总时长不变，反而让
 * 两个进度条都慢吞吞地爬。串行还让单片吃满带宽，一个传完就是真传完。
 */
const MAX_PARALLEL_UPLOADS = 1;
/**
 * 单个文件内同时上传的分片数。
 *
 * 浏览器对同一个 host 最多 6 条连接；1 个文件 × 4 片占 4 条，剩下的留给
 * 列表、sign、confirm 这些请求。再多只会让分片排在队列里，进度一顿一顿的。
 */
const CHUNK_PARALLEL_LIMIT = 4;
/**
 * 分片大小，同时也是「切不切」的门槛：小于等于它的文件直接单次 putObject，
 * 更大的按它切片（16–32MB 两片、32–48MB 三片、48–64MB 四片，正好一轮并发）。
 *
 * 16MB 以下不切：分片上传要先付三次往返——ListMultipartUploads 探续传、
 * InitiateMultipartUpload、最后 CompleteMultipartUpload，RTT 35ms 时约 100ms。
 * 而 16MB 单流在国内链路上 0.3–1s 就传完了，多开连接省下的慢启动还不够贴
 * 这笔开销。
 *
 * 切片按固定 16MB 走，而不是「按文件大小平分 N 份」：平分会让大文件的单片
 * 涨到几百 MB，一片失败（SDK 重试 2 次）就要整片重传，4 路里有一路在重传时
 * 流水就空了。固定 16MB 配 4 片并发，单片拿到的是 4 路分摊后的带宽，寿命
 * 和以前 64MB 单流相当（约 1–3 秒），重传代价却只有四分之一。
 *
 * 64MB 以内分片数不超过 4，正好一次全发出去，不存在排队；再大就一片接一片
 * 补位。48GB 按 16MB 切是 3072 片，离 COS 的 10000 片上限还有余量；真到
 * 160GB 以上，SDK 会自己把切片调大（MaxPartNumber 保护）。
 */
const SLICE_SIZE = 16 * 1024 * 1024;

/**
 * 平滑的上传速度。
 *
 * SDK 给的 info.speed 是相邻两次进度回调（300ms）之间的瞬时差值，网络稍微
 * 一抖就在 0 和峰值之间来回跳。这里改用「最近几秒前进的字节 ÷ 实际耗时」，
 * 而且只用前进过的字节：分片重试失败时 SDK 会把 loaded 往回扣。
 */
const SPEED_WINDOW_MS = 3000;

class SpeedMeter {
  private samples: { at: number; loaded: number }[] = [];

  /** 记一次进度，返回单调的已传字节数和平滑速度（字节/秒）。 */
  push(rawLoaded: number, now: number): { loaded: number; speed: number } {
    const last = this.samples[this.samples.length - 1];
    const loaded = last ? Math.max(last.loaded, rawLoaded) : rawLoaded;
    if (!last || loaded > last.loaded) this.samples.push({ at: now, loaded });

    const cutoff = now - SPEED_WINDOW_MS;
    while (this.samples.length > 1 && this.samples[1].at <= cutoff) this.samples.shift();

    const first = this.samples[0];
    // 用 now 而不是最后一个采样点：卡住不动时速度会自然衰减到 0，而不是冻在旧值。
    const span = Math.max(now - first.at, 300);
    return { loaded, speed: ((loaded - first.loaded) / span) * 1000 };
  }
}

export type UploadStatus = "queued" | "uploading" | "finalizing" | "failed";

export interface UploadTask {
  id: string;
  file: File;
  name: string;
  /** 上传时所在的目录，用来决定这条占位行显示在哪个目录里。 */
  dir: string;
  path: string;
  size: number;
  overwrite: boolean;
  status: UploadStatus;
  loaded: number;
  /** 字节/秒，只有 uploading 时有意义。 */
  speed: number;
  /** 字节已经传完，只差 confirm。失败重试时可以直接跳过重传。 */
  uploaded: boolean;
  error?: string;
}

interface UploadRuntime {
  canceled: boolean;
  taskId?: string;
  cos?: CancelableCos;
  target?: { bucket: string; region: string; key: string };
  /** 取消时用它把等待中的传输 Promise 直接 reject 掉。 */
  abortTransfer?: () => void;
}

/** 只声明取消流程会用到的两个方法，避免把整个 SDK 类型带进来。 */
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

/** COS SDK 的错误对象有三种历史形状，挨个取一条能看的消息。 */
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

  /** 同步更新 ref 和 state，异步流程永远读 tasksRef。 */
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
          // 上一轮已经传完字节，只是 confirm 失败；别再传一遍。
          update((current) =>
            current.map((entry) =>
              entry.id === id ? { ...entry, status: "finalizing", speed: 0 } : entry,
            ),
          );
        } else {
          // COS SDK 只有真正上传时才需要，按需加载，首屏小很多。
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
                SliceSize: SLICE_SIZE,
                AsyncLimit: CHUNK_PARALLEL_LIMIT,
                onTaskReady: (taskId) => {
                  runtime.taskId = taskId;
                },
                onTaskStart: () => {
                  // 如果 cancel 发生在任务真正开始之前，这里补一刀。
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
        // 重试会从 0 或上次已传字节重新开始，速度窗口一并清掉。
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
              // 分片上传取消后 COS 侧会留下碎片，尽力清一次；失败也不影响界面。
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
          // 取消是尽力而为：SDK 内部报错也不能让占位行留在界面上。
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
