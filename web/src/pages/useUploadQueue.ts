/** Upload tasks and File references live only in the current page. */
import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, FileApiError, api, type FileProblem, type Item, type FileOperation, type FileUploadInput, type FileUploadPartGrant, type FileUploadSession } from "../api/client";
import { t } from "../i18n";
import { foldersWithoutFiles, type UploadEntry } from "./uploadSelection";
import { claimUploadPath } from "./uploadOwnership";
import { pendingFileOperation, setUploadOwner, settleFileOperation } from "./uploadPersistence";

// Part requests in flight across every file. A part is long enough that even a slow
// stream keeps its connection busy, so eight 16 MiB parts already hold 128 MiB in
// flight; more only costs payload memory. This pool is the only bound on byte
// concurrency: the server admits part bodies with one process-wide pool and does
// not tell clients how many to send, so tasks share this pool instead of each
// respecting its own budget.
const PART_POOL_SIZE = 8;
// Tasks allowed to be running at all: claiming a path, allocating the session, moving
// parts and committing. Eight matches the part pool, so a wave of small files cannot
// open hundreds of upload sessions for bytes that can only stream eight at a time.
// This bounds handshakes and outstanding requests, not payload: a large file still
// gets its parts from the same eight-slot pool.
const START_POOL_SIZE = 8;
// The page is published at most this often. A file changes state several times, and
// every publication re-derives the row projection over the whole selection, so a
// 20k-file folder would publish ~100k times and never leave the main thread. Task
// state stays authoritative in the ref; the screen catches up ten times a second,
// which is faster than the eye separates, and gestures publish immediately.
const PUBLISH_INTERVAL_MS = 100;
const PART_UPLOAD_ATTEMPTS = 3;
const PART_RETRY_DELAY_MS = 250;
// A 429 means every receive slot is busy, which is the receiver asking this part to
// wait rather than a lost part: retry without spending a transfer attempt.
const PART_BUSY_WAITS = 60;
// Progress reaches the list once a second. XHR reports bytes many times per second,
// and re-rendering the whole file list for events the eye cannot separate only burns
// main-thread time while a part is streaming.
const PROGRESS_INTERVAL_MS = 1000;
// The displayed speed covers the latest ten seconds. Streamed parts report send progress
// continuously, so this window only smooths: it damps each part's socket-buffer lead and
// the gaps between receipts. A genuine stall still reaches 0 B/s, ten seconds later.
const SPEED_WINDOW_MS = 10000;
interface ProgressSample { at: number; loaded: number }
export type UploadStatus = "queued" | "waiting" | "uploading" | "finalizing" | "failed" | "completed";
export interface DeletionWait {
  operationId: string;
  path: string;
  state: "queued" | "running" | "outcome_unknown" | "failed" | "cancelled";
  statusUnavailable?: boolean;
  ancestorId?: string;
}
export interface UploadTask {
  id: string;
  file: File | null;
  isDir: boolean;
  name: string;
  dir: string;
  path: string;
  size: number;
  overwrite: boolean;
  /** The upload creates missing ancestor folders instead of waiting for their tasks. */
  parents: boolean;
  status: UploadStatus;
  loaded: number;
  speed: number;
  /**
   * How many part requests this file currently holds a slot for.
   *
   * `status === "uploading"` only means the task was started: claiming the path,
   * allocating the session, reading receipts and waiting behind an earlier file all
   * happen before this file's first byte moves, and the global pool hands every free
   * slot to the earliest-started file, so several started files can sit at 0 B/s for
   * minutes. Counting held slots is the only local fact that says "bytes can move now";
   * the row projection uses it to keep those files labelled as queued instead of
   * reporting an upload that is not uploading.
   */
  activeParts: number;
  uploaded: boolean;
  uploadId?: string;
  operationId?: string;
  createKey: string;
  intent?: FileUploadInput;
  modifiedAt?: number;
  cancelRequested?: boolean;
  terminalState?: "failed" | "aborted" | "expired";
  retryRequested?: boolean;
  allocationRejected?: boolean;
  waitingFor?: DeletionWait;
  waitMessage?: string;
  error?: string;
}
export interface UploadQueue {
  tasks: UploadTask[];
  hasActiveUploads: boolean;
  enqueue(entries: UploadEntry[], dir: string, exists: (path: string) => boolean): void;
  cancel(id: string): void;
  retry(id: string): void;
}
interface Runtime { controller: AbortController; }
interface PartWaiter { rank: number; number: number; resolve: () => void; }
const relative = (path: string) => path.replace(/^\/+|\/+$/g, "");
let sequence = 0;
// The only thing the queue asks capabilities for is whether this connection can
// carry a source modification time — a service-wide flag, not a per-folder one.
// Asking per file costs one request per file, which for a 23k-file folder is
// 23k requests that never change their answer.
let sourceModifiedAtSupport: Promise<boolean> | undefined;
function supportsSourceModifiedAt(parent: string): Promise<boolean> {
  sourceModifiedAtSupport ??= api.capabilities(parent)
    .then((capabilities) => !!capabilities.sourceModifiedAt, (error) => {
      sourceModifiedAtSupport = undefined;
      throw error;
    });
  return sourceModifiedAtSupport;
}

function taskId(): string { return `upload-${Date.now().toString(36)}-${(++sequence).toString(36)}-${Math.random().toString(36).slice(2)}`; }
// A failed row must name the same identity the server logged. Without the problem
// code and request id, "the storage read failed" cannot be matched to any backend line.
function problemText(problem: FileProblem): string {
  if (!problem.code) return problem.detail;
  return `${problem.detail} (${problem.requestId ? `${problem.code} · ${problem.requestId}` : problem.code})`;
}
function message(error: unknown): string {
  if (error instanceof FileApiError) return problemText(error.problem);
  return error instanceof Error ? error.message : t("error.unknown");
}
function isOperation(value: Item | FileOperation): value is FileOperation { return "state" in value && "progress" in value; }
function waitMessage(wait: DeletionWait): string {
  if (wait.statusUnavailable) return t("drive.deletionStatusUnavailable");
  if (wait.state === "failed" || wait.state === "cancelled") return t("drive.deletionNeedsAttention");
  return t("drive.waitingForDeletion", { path: wait.path });
}
async function sha256(blob: Blob): Promise<string> {
  const bytes = await crypto.subtle.digest("SHA-256", await blob.arrayBuffer());
  return [...new Uint8Array(bytes)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}
// Verifying a receipt reads the whole part into one ArrayBuffer for SubtleCrypto, so
// a full batch of 16 MiB parts would hold well over a hundred MiB at once. Verification is
// CPU work on bytes that are already durable, never a reason to widen the link, so
// it runs in a small queue instead of one buffer per in-flight part.
const VERIFY_SLOTS = 2;
const verifyPool = { active: 0, waiters: [] as (() => void)[] };
async function withVerifySlot<T>(run: () => Promise<T>): Promise<T> {
  if (verifyPool.active < VERIFY_SLOTS && verifyPool.waiters.length === 0) verifyPool.active++;
  else await new Promise<void>((resolve) => verifyPool.waiters.push(resolve));
  try {
    return await run();
  } finally {
    const next = verifyPool.waiters.shift();
    if (next) next(); else verifyPool.active--;
  }
}
export function useUploadQueue(onUploaded: (task: UploadTask, item: Item) => void): UploadQueue {
  // Task state is authoritative here, in a Map, and reaches React as a snapshot on a
  // cadence. One file performs several updates (queued, started, allocated, parts,
  // committed); publishing each of them re-derives the row projection over every task
  // and re-renders the page, so a 20k-file folder would publish ~100k times. The map also
  // keeps one update O(1) instead of rebuilding the whole task array per change.
  const tasks = useRef(new Map<string, UploadTask>());
  // Ids waiting for a start slot, in selection order.
  const pending = useRef(new Set<string>());
  // Directory tasks that have not completed. A task under one of them must wait, and
  // answering "is an ancestor still a pending directory" by scanning every task would
  // make each start O(tasks).
  const openDirs = useRef(new Set<string>());
  const [published, setPublished] = useState<UploadTask[]>([]);
  const flushTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const owner = useRef("");
  const pathClaims = useRef(new Map<string, () => void>());
  const mounted = useRef(true);
  const ready = useRef(false);
  const running = useRef(new Map<string, Runtime>());
  const pumpRef = useRef<() => void>(() => {});
  const finishRef = useRef(onUploaded);
  const verified = useRef(new Map<string, { file: File; parts: Map<number, string> }>());
  const pollBusy = useRef(false);
  const deadlines = useRef(new Set<ReturnType<typeof setTimeout>>());
  // Global part pool: the earliest-started file (then lowest part number) gets the
  // next free slot, so files finish in order instead of all advancing slowly.
  const partPool = useRef({ active: 0, waiters: [] as PartWaiter[] });
  const ranks = useRef(new Map<string, number>());
  const nextRank = useRef(0);
  finishRef.current = onUploaded;
  const releasePart = useCallback(() => {
    const pool = partPool.current;
    const next = pool.waiters.shift();
    if (next) next.resolve(); else pool.active--;
  }, []);
  const acquirePart = useCallback((rank: number, number: number, signal: AbortSignal) => new Promise<void>((resolve, reject) => {
    const pool = partPool.current;
    if (signal.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
    if (pool.active < PART_POOL_SIZE && !pool.waiters.length) { pool.active++; resolve(); return; }
    const waiter: PartWaiter = { rank, number, resolve: () => { signal.removeEventListener("abort", abort); resolve(); } };
    const abort = () => {
      const at = pool.waiters.indexOf(waiter);
      if (at >= 0) pool.waiters.splice(at, 1);
      reject(new DOMException("Upload canceled", "AbortError"));
    };
    signal.addEventListener("abort", abort, { once: true });
    const at = pool.waiters.findIndex((other) => other.rank > rank || other.rank === rank && other.number > number);
    pool.waiters.splice(at < 0 ? pool.waiters.length : at, 0, waiter);
  }), []);
  const publish = useCallback(() => {
    if (!mounted.current) return;
    if (flushTimer.current !== undefined) {
      clearTimeout(flushTimer.current);
      deadlines.current.delete(flushTimer.current);
      flushTimer.current = undefined;
    }
    setPublished([...tasks.current.values()]);
  }, []);
  const schedulePublish = useCallback(() => {
    if (!mounted.current || flushTimer.current !== undefined) return;
    const timer = setTimeout(() => {
      deadlines.current.delete(timer);
      flushTimer.current = undefined;
      if (mounted.current) setPublished([...tasks.current.values()]);
    }, PUBLISH_INTERVAL_MS);
    flushTimer.current = timer;
    deadlines.current.add(timer);
  }, []);
  // The start queue and the pending-directory index are derived from a task's status, so
  // they are updated in the same place, never by scanning the task list.
  const track = useCallback((task: UploadTask) => {
    if (task.status === "queued") pending.current.add(task.id);
    else pending.current.delete(task.id);
    if (task.isDir) {
      if (task.status === "completed") openDirs.current.delete(task.path);
      else openDirs.current.add(task.path);
    }
  }, []);
  const releaseClaim = useCallback((id: string) => {
    const release = pathClaims.current.get(id);
    if (!release) return;
    pathClaims.current.delete(id);
    release();
  }, []);
  const patch = useCallback((id: string, changes: Partial<UploadTask>) => {
    const current = tasks.current.get(id);
    if (!current) return;
    const next = { ...current, ...changes };
    tasks.current.set(id, next);
    track(next);
    schedulePublish();
  }, [schedulePublish, track]);
  const insert = useCallback((added: UploadTask[]) => {
    for (const task of added) { tasks.current.set(task.id, task); track(task); }
    schedulePublish();
  }, [schedulePublish, track]);
  const remove = useCallback((ids: string[]) => {
    for (const id of ids) {
      const task = tasks.current.get(id);
      if (!task) continue;
      tasks.current.delete(id);
      pending.current.delete(id);
      if (task.isDir) openDirs.current.delete(task.path);
      releaseClaim(id);
    }
    schedulePublish();
  }, [releaseClaim, schedulePublish]);
  const complete = useCallback((task: UploadTask, item: Item) => {
    if (!mounted.current || !tasks.current.has(task.id)) return;
    if (task.uploadId) verified.current.delete(task.uploadId);
    if (task.cancelRequested) {
      // Cancellation after publication cannot undo a committed native file.
      remove([task.id]);
    } else {
      patch(task.id, { status: "completed", loaded: task.size, file: null, speed: 0, error: undefined });
      releaseClaim(task.id);
    }
    finishRef.current(task, item);
  }, [patch, releaseClaim, remove]);
  const applyOperation = useCallback((task: UploadTask, op: FileOperation) => {
    if (op.id !== task.operationId) throw new Error("The operation identity changed.");
    if (op.state === "succeeded") {
      if (!op.result || !("kind" in op.result)) throw new Error("The operation result is incomplete.");
      complete(task, op.result);
    } else if (op.state === "failed" || op.state === "cancelled") {
      if (task.cancelRequested) {
        if (task.uploadId) verified.current.delete(task.uploadId);
        remove([task.id]);
      }
      else if (task.retryRequested) {
        patch(task.id, { status: "queued", uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, retryRequested: false, uploaded: false, loaded: 0 });
      } else patch(task.id, { status: "failed", speed: 0, terminalState: task.isDir ? undefined : op.state === "failed" ? "failed" : "aborted", error: op.error ? problemText(op.error) : t("drive.uploadFailed") });
    } else { patch(task.id, { status: "finalizing", speed: 0 }); }
  }, [complete, patch, remove]);

  const run = useCallback(async (id: string, runtime: Runtime) => {
    const signal = runtime.controller.signal;
    try {
      let task = tasks.current.get(id);
      if (!task) return;
      if (task.waitingFor) { patch(id, { status: "waiting" }); return; }
      const deletion = pendingFileOperation(task.path);
      if (deletion) {
        const waitingFor: DeletionWait = { operationId: deletion.id, path: deletion.path, state: "running" };
        patch(id, { status: "waiting", waitingFor, waitMessage: waitMessage(waitingFor) });
        return;
      }
      if (task.operationId) {
        const op = await api.getOperation(task.operationId, signal);
        if (!signal.aborted) applyOperation(task, op);
        return;
      }
      if (task.isDir) {
        let result: Item | FileOperation;
        try { result = await api.mkdir(task.path); }
        catch (error) {
          if (!(error instanceof ApiError) || error.code !== "name_conflict") throw error;
          result = await api.metadata(task.path);
          if (result.kind !== "folder") throw error;
        }
        if (signal.aborted) return;
        if (isOperation(result)) { patch(id, { operationId: result.id, status: "finalizing" }); }
        else { if (result.kind !== "folder") throw new Error("A file occupies the destination folder."); complete(task, result); }
        return;
      }
      if (!pathClaims.current.has(id)) {
        let release: (() => void) | null;
        const claimController = new AbortController();
        const abortClaim = () => claimController.abort();
        signal.addEventListener("abort", abortClaim, { once: true });
        try { release = await claimUploadPath(owner.current, task.path, claimController.signal); }
        catch (error) {
          if (signal.aborted) return;
          throw new Error(t("drive.uploadCoordinationUnavailable"), { cause: error });
        } finally { signal.removeEventListener("abort", abortClaim); }
        if (signal.aborted) { release?.(); return; }
        if (!release) throw new Error(t("drive.uploadAlreadyInProgress"));
        pathClaims.current.set(id, release);
      }
      let session: FileUploadSession;
      if (task.uploadId) {
        session = await api.uploadStatus(task.uploadId, signal);
      } else {
        let intent = task.intent;
        if (!intent) {
          if (!task.file) { patch(id, { status: "failed", error: t("drive.uploadFileUnavailable"), speed: 0 }); return; }
          let condition: FileUploadInput["condition"] = { mode: "create" };
          if (task.overwrite) {
            const old = await api.metadata(task.path);
            if (old.kind !== "file" || !old.contentRevision) throw new Error("The destination cannot be replaced safely.");
            condition = { mode: "replace", contentRevision: old.contentRevision };
          }
          const slash = task.path.lastIndexOf("/");
          const parent = slash < 0 ? "/" : task.path.slice(0, slash);
          const sourceModifiedAt = await supportsSourceModifiedAt(parent);
          if (signal.aborted) return;
          intent = { path: task.path, size: task.size, mediaType: task.file?.type || "application/octet-stream", condition,
            ...(task.parents && condition.mode === "create" ? { parents: true as const } : {}),
            ...(sourceModifiedAt && task.modifiedAt !== undefined ? { sourceModifiedAt: new Date(task.modifiedAt).toISOString() } : {}) };
          // Retain the exact intent/key in memory before allocation, including a lost create ACK.
          patch(id, { intent });
        }
        try { session = await api.createUpload(intent, task.createKey, signal); }
        catch (error) {
          if (error instanceof ApiError && [400, 401, 403, 409, 413, 422, 507].includes(error.status)) patch(id, { allocationRejected: true });
          if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId) {
            let blocking: FileUploadSession | undefined;
            try { blocking = await api.uploadStatus(error.operationId, signal); }
            catch (statusError) {
              // Only a definitive non-upload identity belongs in generic Operation waiting.
              if (!(statusError instanceof ApiError) || statusError.status !== 404) throw statusError;
            }
            if (signal.aborted) return;
            if (blocking?.id === error.operationId && relative(blocking.path) === task.path) {
              if (blocking.state === "open") {
                try { await api.abortUpload(blocking.id, signal); }
                catch (abortError) {
                  // A lost abort ACK must be reconciled before a fresh allocation.
                  const settled = await api.uploadStatus(blocking.id, signal);
                  if (settled.id !== blocking.id || relative(settled.path) !== task.path || !["aborted", "expired", "failed"].includes(settled.state)) throw abortError;
                  const op = await api.getOperation(blocking.id, signal);
                  if (op.id !== blocking.id || op.state !== "cancelled") throw abortError;
                }
                if (!signal.aborted) patch(id, { status: "queued", allocationRejected: false });
                return;
              }
              if (["aborted", "expired", "failed"].includes(blocking.state)) {
                const op = await api.getOperation(blocking.id, signal);
                if (signal.aborted) return;
                if (op.id === blocking.id && op.state === "cancelled") {
                  patch(id, { status: "queued", allocationRejected: false });
                  return;
                }
              }
            }
          }
          if (error instanceof ApiError && error.status === 409 && error.code === "name_conflict" && !task.overwrite) {
            const old = await api.metadata(task.path);
            if (old.kind === "file" && window.confirm(t("drive.overwriteConfirm", { name: task.name }))) {
              patch(id, { overwrite: true, intent: undefined, createKey: taskId(), status: "queued" }); return;
            }
          }
          throw error;
        }
        patch(id, { uploadId: session.id, allocationRejected: false });
      }
      if (signal.aborted) return;
      task = tasks.current.get(id)!;
      if (session.state === "committed") { if (!session.result) throw new Error("The upload result is incomplete."); complete(task, session.result); return; }
      if (session.state === "committing" || session.state === "outcome_unknown") {
        patch(id, { operationId: session.operationId, status: "finalizing", uploaded: true }); return;
      }
      if (session.state !== "open") {
        if (session.state === "failed" || session.state === "aborted" || session.state === "expired") {
          if (task.retryRequested && task.file) {
            patch(id, { status: "queued", uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, retryRequested: false, uploaded: false, loaded: 0 }); return;
          }
          patch(id, { terminalState: session.state });
        }
        throw new Error(`Upload ${session.state}; select the file again to start a new session.`);
      }
      if (!task.file) { patch(id, { status: "failed", error: t("drive.uploadFileUnavailable"), speed: 0 }); return; }
      if (task.file.size !== session.size) throw new Error("The selected file has a different size.");
      const file = task.file;
      if (!Number.isSafeInteger(session.partSize) || session.partSize < 1 || !Number.isSafeInteger(session.partCount)
        || session.partCount !== Math.ceil(session.size / session.partSize)
        || !["any", "ascending"].includes(session.ordering)) throw new Error("Invalid upload session geometry.");
      const classA = session.class === "A";
      if (classA && session.transport?.kind !== "presigned-parts") throw new Error("A class A session must advertise its presigned part window.");
      if (!classA && session.class !== "C") throw new Error("Unsupported upload transfer class.");
      if (!classA && session.transport?.kind !== "relay") throw new Error("A class C session must advertise its relay URL.");
      const parallelism = session.ordering === "ascending" ? 1 : PART_POOL_SIZE;
      const chunkFor = (number: number) => {
        const start = (number - 1) * session.partSize;
        return file.slice(start, Math.min(start + session.partSize, session.size));
      };
      const known = new Map<number, number>();
      let verification = verified.current.get(session.id);
      if (!classA && (!verification || verification.file !== file)) {
        verification = { file, parts: new Map() };
        verified.current.set(session.id, verification);
      }
      const verifyReceipt = async (receipt: { number: number; size: number; sha256: string }) => {
        if (!Number.isSafeInteger(receipt.number) || receipt.number < 1 || receipt.number > session.partCount) throw new Error("Invalid part receipt.");
        const chunk = chunkFor(receipt.number);
        if (receipt.size !== chunk.size || (verification!.parts.get(receipt.number) !== receipt.sha256 && await sha256(chunk) !== receipt.sha256)) throw new Error(t("drive.reselectedFileChanged"));
        verification!.parts.set(receipt.number, receipt.sha256);
      };
      const readParts = async (requestSignal: AbortSignal) => {
        const result = new Map<number, number>();
        let next: string | undefined;
        let watermark: number | undefined;
        do {
          const page = await api.uploadParts(session.id, next, requestSignal);
          if (!classA && watermark !== undefined && page.watermark !== watermark) throw new Error("The receipt watermark changed.");
          watermark = page.watermark;
          for (const part of page.items) {
            if (result.has(part.number)) throw new Error("Invalid part listing.");
            const chunk = chunkFor(part.number);
            if (part.size !== chunk.size) throw new Error("The provider part size changed.");
            if (classA) {
              if (!("etag" in part) || typeof part.etag !== "string") throw new Error("Invalid provider part listing.");
            } else {
              if (!("sha256" in part)) throw new Error("Invalid part receipt.");
              await verifyReceipt(part);
            }
            result.set(part.number, part.size);
          }
          next = page.nextCursor ?? undefined;
        } while (next);
        return result;
      };
      for (const [number, size] of await readParts(signal)) known.set(number, size);
      if (signal.aborted) return;
      let received = [...known.values()].reduce((sum, size) => sum + size, 0);
      patch(id, { loaded: received, speed: 0 });
      const missing: number[] = [];
      // Up to `parallelism` workers drain this file's missing parts; every request also
      // takes a slot from the global part pool, so the next queued part starts as soon
      // as any part anywhere finishes.
      for (let number = 1; number <= session.partCount && (session.ordering !== "ascending" || missing.length < 1); number++) {
        if (!known.has(number)) missing.push(number);
      }
      if (session.ordering === "ascending" && missing.length && session.nextPart !== missing[0]) throw new Error("The next upload part changed.");
      const grants = new Map<number, FileUploadPartGrant>();
      if (classA && session.transport?.kind === "presigned-parts") {
        for (const grant of session.transport.parts) grants.set(grant.number, grant);
      }
      let grantChain: Promise<void> = Promise.resolve();
      const ensureGrants = (from: number): Promise<void> => {
        const run = grantChain.then(async () => {
          if (grants.has(from) && Date.parse(grants.get(from)!.expiresAt) > Date.now()) return;
          const numbers: number[] = [];
          for (let number = from; number <= session.partCount && numbers.length < 32; number++) {
            const current = grants.get(number);
            if (!known.has(number) && (!current || Date.parse(current.expiresAt) <= Date.now())) numbers.push(number);
          }
          if (!numbers.length) return;
          for (const grant of await api.signUploadParts(session.id, numbers, signal)) {
            if (grant.number < 1 || grant.number > session.partCount || grant.size !== chunkFor(grant.number).size) throw new Error("The signed part geometry changed.");
            grants.set(grant.number, grant);
          }
        });
        grantChain = run.catch(() => {});
        return run;
      };
      if (classA && missing.length) await ensureGrants(missing[0]);
      if (missing.length) {
        const batch = new AbortController();
        const abortBatch = () => batch.abort();
        signal.addEventListener("abort", abortBatch, { once: true });
        const inFlight = new Map<number, number>();
        const samples: ProgressSample[] = [];
        const flush = () => {
          if (signal.aborted || batch.signal.aborted) return;
          const at = performance.now();
          const loaded = Math.min(session.size, received + [...inFlight.values()].reduce((sum, bytes) => sum + bytes, 0));
          samples.push({ at, loaded });
          while (samples.length > 1 && at - samples[0].at > SPEED_WINDOW_MS) samples.shift();
          const elapsed = at - samples[0].at;
          patch(id, { loaded, speed: elapsed > 0 ? Math.max(0, loaded - samples[0].loaded) * 1000 / elapsed : 0 });
        };
        // One sample per second, independent of how often XHR reports bytes: the list
        // stays readable, and a stalled link visibly falls to 0 B/s instead of freezing
        // on its last value.
        const ticker = setInterval(flush, PROGRESS_INTERVAL_MS);
        deadlines.current.add(ticker);
        const cancelProgress = () => {
          clearInterval(ticker);
          deadlines.current.delete(ticker);
        };
        const retryDelay = (attempt: number) => new Promise<void>((resolve, reject) => {
          if (batch.signal.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
          const abort = () => { clearTimeout(timer); deadlines.current.delete(timer); reject(new DOMException("Upload canceled", "AbortError")); };
          const timer = setTimeout(() => { deadlines.current.delete(timer); batch.signal.removeEventListener("abort", abort); resolve(); }, PART_RETRY_DELAY_MS * 2 ** attempt);
          deadlines.current.add(timer);
          batch.signal.addEventListener("abort", abort, { once: true });
        });
        let failure: unknown;
        try {
          const pending = [...missing];
          const uploadOne = async (number: number) => {
            const chunk = chunkFor(number);
            try {
              let attempt = 0;
              let busy = 0;
              while (attempt < PART_UPLOAD_ATTEMPTS) {
                if (batch.signal.aborted) throw new DOMException("Upload canceled", "AbortError");
                try {
                  if (classA) {
                    await ensureGrants(number);
                    const grant = grants.get(number);
                    if (!grant) throw new Error("The part grant is unavailable.");
                    await api.uploadPresignedPart(grant, chunk, batch.signal, (sample) => {
                      if (batch.signal.aborted || known.has(number)) return;
                      inFlight.set(number, Math.max(inFlight.get(number) ?? 0, Math.max(0, Math.min(chunk.size, sample.loaded))));
                    });
                    grants.delete(number);
                  } else {
                    const receipt = await api.uploadPart(session.id, number, chunk, batch.signal, (sample) => {
                      if (batch.signal.aborted || known.has(number)) return;
                      inFlight.set(number, Math.max(inFlight.get(number) ?? 0, Math.max(0, Math.min(chunk.size, sample.loaded))));
                    });
                    if (receipt.number !== number) throw new Error("The part receipt identity changed.");
                    await withVerifySlot(() => verifyReceipt(receipt));
                    known.set(number, receipt.size);
                  }
                  if (batch.signal.aborted) return;
                  if (classA) known.set(number, chunk.size);
                  received += chunk.size;
                  inFlight.delete(number);
                  return;
                } catch (error) {
                  if (batch.signal.aborted) throw error;
                  if (error instanceof ApiError && error.status === 429) {
                    // The receiver is full with other parts. Waiting is the protocol;
                    // counting this as a failed attempt would abort the whole file.
                    if (++busy > PART_BUSY_WAITS) throw error;
                    inFlight.delete(number);
                    await retryDelay(Math.min(busy, 3));
                    continue;
                  }
                  const transient = !(error instanceof ApiError) || [0, 403, 408, 425, 500, 502, 503, 504].includes(error.status);
                  if (!transient) throw error;
                  // A lost ACK may already represent durable success. Inspect the
                  // original session and exact byte identity before any replay.
                  const status = await api.uploadStatus(session.id, batch.signal);
                  if (status.state !== "open") throw error;
                  const existing = await readParts(batch.signal);
                  if (existing.has(number)) {
                    if (classA) grants.delete(number);
                    known.set(number, existing.get(number)!);
                    received = [...known.values()].reduce((sum, size) => sum + size, 0);
                    inFlight.delete(number);
                    return;
                  }
                  if (classA) grants.delete(number);
                  if (++attempt === PART_UPLOAD_ATTEMPTS) throw error;
                  inFlight.delete(number);
                  await retryDelay(attempt - 1);
                }
              }
            } catch (error) {
              if (failure === undefined) failure = error;
              batch.abort();
            }
          };
          const rank = ranks.current.get(id) ?? Number.MAX_SAFE_INTEGER;
          // Slots this file holds right now, across its workers. A worker keeps its slot
          // between its own parts, and yields it to a higher-priority part waiting for it,
          // so this count is what the row reads to decide whether the file is moving.
          let held = 0;
          const holdSlot = () => { held++; patch(id, { activeParts: held }); };
          const yieldSlot = () => { held--; releasePart(); patch(id, { activeParts: held }); };
          await Promise.all(Array.from({ length: Math.min(parallelism, pending.length) }, async () => {
            let holding = false;
            try {
              for (let number = pending.shift(); number !== undefined && !batch.signal.aborted; number = pending.shift()) {
                // Keep a held slot unless a higher-priority part is waiting for it.
                const head = partPool.current.waiters[0];
                if (holding && head && (head.rank < rank || head.rank === rank && head.number < number)) { holding = false; yieldSlot(); }
                if (!holding) {
                  try { await acquirePart(rank, number, batch.signal); }
                  catch (error) { if (failure === undefined && !signal.aborted) failure = error; return; }
                  holding = true; holdSlot();
                }
                await uploadOne(number);
              }
            } finally { if (holding) yieldSlot(); }
          }));
        } finally { signal.removeEventListener("abort", abortBatch); cancelProgress(); }
        if (signal.aborted) return;
        if (failure !== undefined) { patch(id, { loaded: received }); throw failure; }
        // Ascending sessions send one new part per turn; requeue for the next one.
        if (known.size !== session.partCount) { patch(id, { loaded: received, status: "queued", speed: 0 }); return; }
      }
      patch(id, { status: "finalizing", uploaded: true, loaded: session.size, speed: 0 });
      const result = await api.commitUpload(session.id, signal);
      if (signal.aborted) return;
      if (isOperation(result)) { patch(id, { operationId: result.id }); }
      else { complete(task, result); }
    } catch (error) {
      if (runtime.controller.signal.aborted) return;
      if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId) {
        const task = tasks.current.get(id);
        const waitingFor: DeletionWait = { operationId: error.operationId, path: task?.path ?? "", state: "running" };
        patch(id, { status: "waiting", waitingFor, waitMessage: waitMessage(waitingFor), speed: 0 });
      } else { patch(id, { status: "failed", error: message(error), speed: 0 }); }
    } finally {
      running.current.delete(id);
      pumpRef.current();
    }
  }, [acquirePart, applyOperation, complete, patch, releasePart]);
  const pump = useCallback(() => {
    if (!ready.current || !mounted.current) return;
    // Start at most START_POOL_SIZE tasks at once. The part pool still decides how many
    // bytes are in flight and the server still does not publish a per-session number to
    // respect; this window bounds the *handshakes* instead. Starting all 20k tasks of a
    // folder selection at once opened 20k upload sessions and 20k pending requests for a
    // queue that can only stream eight parts, and left the browser holding every one.
    // A child waits only for its unfinished directory task, which starts this again when
    // it settles.
    const blockedBy = (path: string): boolean => {
      let slash = path.lastIndexOf("/");
      while (slash > 0) {
        path = path.slice(0, slash);
        if (openDirs.current.has(path)) return true;
        slash = path.lastIndexOf("/");
      }
      return false;
    };
    const starts: UploadTask[] = [];
    for (const id of pending.current) {
      if (running.current.size + starts.length >= START_POOL_SIZE) break;
      const task = tasks.current.get(id);
      if (!task || task.status !== "queued" || task.cancelRequested || running.current.has(id)) {
        pending.current.delete(id);
        continue;
      }
      if (blockedBy(task.path)) continue;
      starts.push(task);
    }
    if (!starts.length) return;
    for (const task of starts) {
      pending.current.delete(task.id);
      // Pool priority follows the order files first start, kept across retries/turns.
      if (!ranks.current.has(task.id)) ranks.current.set(task.id, nextRank.current++);
      running.current.set(task.id, { controller: new AbortController() });
      patch(task.id, { status: "uploading", error: undefined });
    }
    for (const task of starts) void run(task.id, running.current.get(task.id)!);
  }, [patch, run]);
  pumpRef.current = pump;
  const cancel = useCallback((id: string) => {
    const root = tasks.current.get(id);
    const targets = [...tasks.current.values()].filter((task) => task.id === id || root?.isDir && task.path.startsWith(`${root.path}/`));
    for (const task of targets) {
      running.current.get(task.id)?.controller.abort();
      patch(task.id, { cancelRequested: true, status: "finalizing", speed: 0 });
      void (async () => {
        try {
          let operationId = task.operationId;
          let uploadId = task.uploadId;
          if (!uploadId && task.intent && !task.allocationRejected) {
            // Lost create ACK: recover the same allocation before cancelling it.
            const session = await api.createUpload(task.intent, task.createKey);
            uploadId = session.id; patch(task.id, { uploadId });
          }
          if (uploadId) {
            try { await api.abortUpload(uploadId); }
            catch (error) {
              if (!(error instanceof ApiError) || !["commit_in_progress", "already_committed", "upload_expired", "upload_gone"].includes(error.code ?? "")) throw error;
              const session = await api.uploadStatus(uploadId);
              if (session.state === "committed") {
                if (!session.result) throw new Error("The upload result is incomplete.");
                complete({ ...task, uploadId, cancelRequested: true }, session.result); return;
              }
              if (["aborted", "expired", "failed"].includes(session.state)) {
                verified.current.delete(uploadId);
                remove([task.id]); return;
              }
              operationId = session.operationId;
            }
          }
          if (operationId) {
            let result: FileOperation;
            try { result = await api.cancelOperation(operationId); }
            catch (error) {
              if (!(error instanceof ApiError) || error.code !== "already_committed") throw error;
              result = await api.getOperation(operationId);
            }
            patch(task.id, { operationId });
            applyOperation({ ...task, uploadId, operationId, cancelRequested: true }, result);
          } else {
            if (uploadId) verified.current.delete(uploadId);
            remove([task.id]);
          }
        } catch (error) { patch(task.id, { status: "failed", error: message(error) }); }
        finally { pumpRef.current(); }
      })();
    }
    // Cancelling is a gesture: the rows must answer now, not on the next publish tick.
    publish();
  }, [applyOperation, complete, patch, publish, remove]);
  const retry = useCallback((id: string) => {
    const task = tasks.current.get(id);
    if (task?.cancelRequested) { cancel(id); return; }
    patch(id, { status: "queued", error: undefined, speed: 0, retryRequested: true,
      ...(task?.terminalState ? { uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, uploaded: false, loaded: 0 } : {}) });
    publish();
    pumpRef.current();
  }, [cancel, patch, publish]);
  const enqueue = useCallback((entries: UploadEntry[], dir: string, exists: (path: string) => boolean) => {
    // A selected tree keeps only the folders that stay empty: every other folder is
    // created by the upload that first needs it, inside that upload's request.
    //
    // A 20k-file selection must cost one pass and one published state. Looking the
    // destination up in the whole task list per entry and appending one task per state
    // update made the selection quadratic: 20k entries published 40k list states, each
    // holding another copy of the growing array, and the renderer died of it.
    const taken = new Map<string, UploadTask>();
    for (const task of tasks.current.values()) if (task.status !== "completed") taken.set(task.path, task);
    const added: UploadTask[] = [];
    for (const entry of foldersWithoutFiles(entries)) {
      const path = relative(dir ? `${dir}/${entry.path}` : entry.path);
      const old = taken.get(path);
      if (old) {
        if (entry.file && !old.isDir && !old.cancelRequested && (!old.file || old.status === "failed")) {
          if (entry.file.size !== old.size) { patch(old.id, { error: t("drive.reselectedFileChanged") }); continue; }
          patch(old.id, { file: entry.file, status: "queued", error: undefined, retryRequested: true,
            ...(old.terminalState ? { uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, uploaded: false, loaded: 0 } : {}) });
        }
        continue;
      }
      const overwrite = !!entry.file && exists(path);
      if (overwrite && !window.confirm(t("drive.overwriteConfirm", { name: entry.path }))) continue;
      const id = taskId();
      const task: UploadTask = { id, file: entry.file, isDir: !entry.file, name: entry.path, dir: relative(dir), path, size: entry.file?.size ?? 0, modifiedAt: entry.file?.lastModified,
        overwrite, parents: !!entry.file && path.includes("/"), status: "queued", loaded: 0, speed: 0, activeParts: 0, uploaded: false, createKey: id };
      added.push(task);
      taken.set(path, task);
    }
    if (added.length) insert(added);
    // The user just picked the files; publish the rows before the first network hop.
    publish();
    pumpRef.current();
  }, [insert, patch, publish]);
  useEffect(() => {
    mounted.current = true;
    const controller = new AbortController();
    void api.me().then((user) => {
      if (!mounted.current || !user) return;
      owner.current = user.id;
      setUploadOwner(user.id);
      try { localStorage.removeItem(`eam.uploads.v1:${user.id}`); }
      catch { /* Obsolete records are never read, even when storage is unavailable. */ }
      ready.current = true;
      pumpRef.current();
    }).catch(() => { /* Authentication failures keep queued bytes unsent. */ });
    const timer = setInterval(() => {
      if (pollBusy.current || !mounted.current) return;
      const candidates = [...tasks.current.values()].filter((task) => task.waitingFor || task.operationId && task.status === "finalizing");
      if (!candidates.length) return;
      pollBusy.current = true;
      void (async () => {
        try {
          const results = new Map<string, FileOperation | Error>();
          for (const task of candidates) {
            const id = task.waitingFor?.operationId ?? task.operationId!;
            if (!results.has(id)) {
              const timeout = new AbortController();
              const abort = () => timeout.abort(); controller.signal.addEventListener("abort", abort, { once: true });
              const deadline = setTimeout(() => timeout.abort(), 15000);
              deadlines.current.add(deadline);
              try { results.set(id, await api.getOperation(id, timeout.signal)); }
              catch (error) { results.set(id, error instanceof Error ? error : new Error(message(error))); }
              finally { clearTimeout(deadline); deadlines.current.delete(deadline); controller.signal.removeEventListener("abort", abort); }
            }
            if (!mounted.current || !tasks.current.has(task.id)) continue;
            const result = results.get(id)!;
            if (task.waitingFor) {
              if (result instanceof Error || result.id !== id) {
                const waitingFor = { ...task.waitingFor, statusUnavailable: true };
                patch(task.id, { waitingFor, waitMessage: waitMessage(waitingFor) });
              } else if (result.state === "succeeded") {
                settleFileOperation(id); patch(task.id, { status: "queued", waitingFor: undefined, waitMessage: undefined });
              } else {
                const waitingFor: DeletionWait = { ...task.waitingFor, state: result.state, statusUnavailable: false };
                patch(task.id, { waitingFor, waitMessage: waitMessage(waitingFor) });
              }
            } else if (!(result instanceof Error)) { applyOperation(task, result); }
          }
        } finally { pollBusy.current = false; pumpRef.current(); }
      })();
    }, 2000);
    return () => {
      mounted.current = false; ready.current = false; controller.abort(); clearInterval(timer);
      if (flushTimer.current !== undefined) { clearTimeout(flushTimer.current); deadlines.current.delete(flushTimer.current); flushTimer.current = undefined; }
      // Progress tickers are intervals, so they need clearInterval even though they
      // share the deadline set with timeouts.
      for (const deadline of deadlines.current) { clearTimeout(deadline); clearInterval(deadline); }
      deadlines.current.clear();
      for (const runtime of running.current.values()) runtime.controller.abort();
      running.current.clear();
      for (const release of pathClaims.current.values()) release();
      pathClaims.current.clear();
    };
  }, [applyOperation, patch, publish, remove]);
  const hasActiveUploads = published.some((task) => ["queued", "waiting", "uploading", "finalizing"].includes(task.status));
  useEffect(() => {
    if (!hasActiveUploads) return;
    const beforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", beforeUnload);
    return () => window.removeEventListener("beforeunload", beforeUnload);
  }, [hasActiveUploads]);
  return { tasks: published, hasActiveUploads, enqueue, cancel, retry };
}
