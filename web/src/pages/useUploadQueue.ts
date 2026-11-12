/** Upload tasks and File references live only in the current page. */
import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, FileApiError, api, type FileProblem, type FileResource, type Item, type FileOperation, type FileUploadInput, type FileUploadPartGrant, type FileUploadSession } from "../api/client";
import { t } from "../i18n";
import { foldersWithoutFiles, type UploadEntry } from "./uploadSelection";
import { claimUploadPath } from "./uploadOwnership";
import { pendingFileOperation, setUploadOwner, settleFileOperation } from "./uploadPersistence";

// Part requests in flight across every file. The bucket is an HTTP/1.1 origin — measured
// against this bucket, not inferred from the dev server: it answers 1.1 even when the
// client offers h2 in ALPN, so six sockets per origin is all that can move at once. The
// pool sits above that number on purpose: a part that waits behind a CORS preflight for a
// new object key would otherwise leave a socket idle until JavaScript schedules the next
// part, and a pool of 16 keeps the browser's own socket queue non-empty instead. Payload
// is bounded by part size times this count (16 MiB × 16).
const PART_POOL_SIZE = 16;
// Sessions admitted per request, and settlements per request. A page of creates is cheap
// (one conditional URL each, signed locally), so one request may carry a whole page of the
// look-ahead; commits are bounded by the worker pool that settles them, and one page in
// flight is what keeps that pool from being flooded — a page of sixteen is half of the
// server's thirty-two settlement slots. Only one page is in flight at a time: the second
// would take the other half and starve the folder creations and deletions that share the
// pool. The two-second linger only delays a page that cannot fill, which is the tail of an
// import, not its body.
const CREATE_BATCH_SIZE = 32;
const COMMIT_BATCH_SIZE = 16;
const COMMIT_LINGER_MS = 2000;
// Sessions kept in hand ahead of the transfer window: four part pools, so the window can be
// refilled four times over before a create page (~1.7 s for 32, measured in 741f910 against
// this bucket) has to answer. The page the server admits is 32, so 64 in hand is two
// requests, which is the only cost this depth adds. Together with the window, this is the
// most sessions one tab holds open: 96, against an idle window of two hours.
const ADMIT_AHEAD = 64;
// A prepared session reserves its bytes against the owner's quota the moment it is admitted,
// so depth is bounded by bytes as well as by count: 64 small files are nothing, 64 videos
// would reserve gigabytes for a minute before their turn. One gigabyte is four waves of the
// part pool at 16 MiB, i.e. the point where the look-ahead would reserve more than the link
// can consume while the window rotates.
const ADMIT_AHEAD_BYTES = 1_000_000_000;
// Files that may be transferring at once. A slot is held from the moment a file starts
// moving until its bytes are placed; admission and settlement keep their own bounds and
// never take one, so this window does not mix stages that wait on the network with stages
// that move bytes. Two part pools: a file with one part left holds one slot, so filling
// sixteen slots can take more than sixteen files, and a second pool's worth of files absorbs
// the gap between a file's last part and the moment its successor takes the freed socket.
const TRANSFER_WINDOW = 32;
// The page is published at most this often. A file changes state several times, and
// every publication re-derives the row projection over the whole selection, so a
// 20k-file folder would publish ~100k times and never leave the main thread. Task
// state stays authoritative in the ref; the screen catches up ten times a second,
// which is faster than the eye separates, and gestures publish immediately.
const PUBLISH_INTERVAL_MS = 100;
// A phone on a bad link is the normal case, not an exception: five attempts with a delay
// that grows to eight seconds rides out roughly fifteen seconds of outage, and the same
// waiting applies both to a transient failure and to a part the service asked us to retry.
// The delay is capped so a long outage ends in a reported failure instead of a silent stall.
const PART_UPLOAD_ATTEMPTS = 5;
const PART_RETRY_DELAY_MS = 250;
const PART_RETRY_MAX_DELAY_MS = 8000;
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
// Commit is idempotent: the same session may be committed again, and the second answer is
// either the published file or the Operation that is publishing it. A lost response is
// therefore a retry, not a failed upload — the same rule a part already follows, and the
// shape every provider's finalize has: one synchronous request, retried on an uncertain
// outcome. The server settles inside that request, so the happy path returns the file.
const COMMIT_ATTEMPTS = 5;
const COMMIT_RETRY_DELAY_MS = 250;
const COMMIT_RETRY_MAX_DELAY_MS = 8000;
function waitBeforeRetry(attempt: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
    const abort = () => { clearTimeout(timer); reject(new DOMException("Upload canceled", "AbortError")); };
    const timer = setTimeout(() => { signal?.removeEventListener("abort", abort); resolve(); },
      Math.min(COMMIT_RETRY_MAX_DELAY_MS, COMMIT_RETRY_DELAY_MS * 2 ** attempt));
    signal?.addEventListener("abort", abort, { once: true });
  });
}
async function commitUpload(id: string, signal: AbortSignal | undefined, settle: (id: string, signal?: AbortSignal) => Promise<FileResource | FileOperation>): Promise<FileResource | FileOperation> {
  for (let attempt = 0; ; attempt++) {
    try { return await settle(id, signal); }
    catch (error) {
      if (signal?.aborted) throw error;
      // A 4xx other than the transient set is the server's answer about the file itself
      // (incomplete coverage, a moved parent), and retrying it would only repeat it.
      const transient = !(error instanceof ApiError) || [0, 408, 425, 429, 500, 502, 503, 504].includes(error.status);
      if (!transient || attempt + 1 >= COMMIT_ATTEMPTS) throw error;
      await waitBeforeRetry(attempt, signal);
    }
  }
}
interface ProgressSample { at: number; loaded: number }
export type UploadStatus = "queued" | "waiting" | "uploading" | "finalizing" | "failed" | "completed";
export interface FileOperationWait {
  operationId: string;
  path: string;
  state: "queued" | "running" | "outcome_unknown" | "failed" | "cancelled";
  statusUnavailable?: boolean;
  /**
   * What is holding the path. A row may only claim a deletion when the blocker is one:
   * a folder creation this import raced finishes in milliseconds, and a deletion is the
   * user's own decision, so the two must not share a sentence.
   */
  kind: "delete" | "upload" | "operation";
  /**
   * Where this wait was discovered: a stored record the page keeps for an accepted
   * delete, or the server's own `operation_in_progress` answer.
   *
   * The two have different endings. A stored record is dropped by whoever settles it —
   * the page does that as soon as it reads the Operation — so the queue watches the
   * record, not just the Operation, and resumes the moment it is gone. A conflict leaves
   * no record, so its Operation is the only thing that can end the wait, and a terminal
   * one asks the person instead of ending by itself.
   */
  from: "record" | "conflict";
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
   * slot to the earliest-started file, so a started file can sit at 0 B/s for minutes
   * while another one streams. Counting held slots is the only local fact that says
   * "bytes can move now"; the row projection uses it to name that wait beside the
   * status, which stays uploading — a file that already sent bytes must never fall
   * back to the pre-start "queued" word, because that reads as a restart.
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
  waitingFor?: FileOperationWait;
  waitMessage?: string;
  error?: string;
}
export interface UploadQueue {
  tasks: UploadTask[];
  hasActiveUploads: boolean;
  enqueue(entries: UploadEntry[], dir: string, exists: (path: string) => boolean): void;
  cancel(id: string): void;
  retry(id: string): void;
  /**
   * Clear a wait whose deletion ended without deleting anything.
   *
   * A terminal delete released its guards, so the path is writable again, but the browser
   * record that named it outlives the Operation. Dropping that record is the only thing
   * "resume" does; the server still decides whether the path can take a write, and a real
   * guard answers with its own 409. Without this, a blocked upload had no resolution in
   * the UI at all: every path into it (retry, refresh, the automatic poll) stopped at the
   * same record, so the row could only be cancelled.
   */
  resume(id: string): void;
}
interface Runtime { controller: AbortController; }
interface PartWaiter { rank: number; number: number; resolve: () => void; }
const relative = (path: string) => path.replace(/^\/+|\/+$/g, "");
/**
 * The order the file list shows: folders before files, then case-insensitive name.
 *
 * It mirrors the row projection's comparator because both answer the same question —
 * what comes first — and the queue's answer has to be the visible one. Paths rather than
 * basenames keep a selected tree depth-first and name-ordered, the order its folders
 * appear in.
 */
function compareQueueOrder(a: UploadTask, b: UploadTask): number {
  if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
  const left = a.path.toLowerCase();
  const right = b.path.toLowerCase();
  if (left !== right) return left < right ? -1 : 1;
  return a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
}
let sequence = 0;
// The only thing the queue asks capabilities for is whether this connection can carry a
// source modification time — a service-wide flag, not a per-folder one. It was asked for
// each file's parent, and during an import that parent is a folder the upload itself is
// about to create: the answer was a 404 per file, clearing the memo on the way out, so
// every file failed before it allocated anything and the log filled with the same request.
// Ask the authorized root once. A probe that fails only costs the optional modification
// time — never the file, and never a second request.
let sourceModifiedAtSupport: Promise<boolean> | undefined;
function supportsSourceModifiedAt(): Promise<boolean> {
  sourceModifiedAtSupport ??= api.capabilities().then(
    (capabilities) => !!capabilities.sourceModifiedAt,
    () => false,
  );
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
/**
 * What the row says while it waits for a deletion.
 *
 * A deletion that ended `failed` or `cancelled` is not a wait. A terminal Operation has
 * already released its guards, so nothing is still holding the path and no later moment
 * will start this upload by itself: saying "waiting, it will begin automatically" left
 * the row with no action and a promise that could never be kept. The row names the
 * outcome instead, and the queue offers Continue — the one decision left, because the
 * folder may still hold what the deletion did not remove.
 */
function waitMessage(wait: FileOperationWait): string {
  if (wait.statusUnavailable) return t("drive.operationStatusUnavailable");
  const ended = wait.state === "failed" || wait.state === "cancelled";
  if (wait.kind === "delete") {
    return ended ? t("drive.deletionDidNotFinish", { path: wait.path }) : t("drive.waitingForDeletion", { path: wait.path });
  }
  if (wait.kind === "upload") return ended ? t("drive.uploadDidNotFinish") : t("drive.waitingForUpload");
  return ended ? t("drive.operationDidNotFinish") : t("drive.waitingForOperation");
}
// Without a kind the honest sentence is "another file operation", never "a deletion".
function waitKind(kind: string | undefined): FileOperationWait["kind"] {
  if (kind === "delete") return "delete";
  if (kind === "upload_commit") return "upload";
  return "operation";
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
/** One page of pending admission/settlement work. */
interface BatchState<T> {
  items: T[];
  timer?: ReturnType<typeof setTimeout>;
  flushing: boolean;
}

interface PendingCreate { input: FileUploadInput; key: string; resolve: (session: FileUploadSession) => void; reject: (error: unknown) => void; cancelled: boolean }
interface PendingSettle { id: string; resolve: (result: FileResource | FileOperation) => void; reject: (error: unknown) => void; cancelled: boolean }

export function useUploadQueue(onUploaded: (task: UploadTask, item: Item) => void): UploadQueue {
  // Task state is authoritative here, in a Map, and reaches React as a snapshot on a
  // cadence. One file performs several updates (queued, started, allocated, parts,
  // committed); publishing each of them re-derives the row projection over every task
  // and re-renders the page, so a 20k-file folder would publish ~100k times. The map also
  // keeps one update O(1) instead of rebuilding the whole task array per change.
  const tasks = useRef(new Map<string, UploadTask>());
  // Ids still waiting for a transfer slot, in selection order. A queued file may already
  // hold its session — admission is a stage of its own and does not remove it from here.
  const pending = useRef(new Set<string>());
  // Ids currently holding a transfer slot, from the moment a file starts moving bytes to
  // the moment its bytes are placed. Admission and settlement never take one, so this set
  // — not "started tasks" — is the byte concurrency the part pool hands slots to.
  const transferring = useRef(new Set<string>());
  // Admission in flight or in hand, one per file. Its presence is the right to start that
  // file without waiting for a create: whoever starts it awaits this promise instead of
  // sending a second request, and the session it carries is used once.
  const admissions = useRef(new Map<string, { controller: AbortController; promise: Promise<FileUploadSession> }>());
  // Files a look-ahead admission already refused. The window is the authority for a
  // refusal — it owns the row's error and its retry — so retrying the same refusal on
  // every completion would only turn one bad destination into an endless request loop.
  const admissionFailed = useRef(new Set<string>());
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
  // Admission and settlement pages. These collapse the two control requests a task would
  // otherwise send alone; the look-ahead fills a page with the files that will start next,
  // which is why a prepare costs one request for a whole window rather than one per file.
  // A test double without the batch calls keeps the per-file path, so the queue's own
  // behaviour stays covered by the existing suite.
  const createBatch = useRef<BatchState<PendingCreate>>({ items: [], flushing: false });
  const settleBatch = useRef<BatchState<PendingSettle>>({ items: [], flushing: false });

  const flushCreates = useCallback(async () => {
    const state = createBatch.current;
    if (state.flushing) return;
    state.flushing = true;
    const batch = state.items.splice(0, CREATE_BATCH_SIZE);
    try {
      const results = await api.createUploadBatch(batch.map(({ input, key }) => ({ input, key })));
      batch.forEach((item, index) => {
        if (item.cancelled) return;
        const result = results[index];
        if (result?.upload) item.resolve(result.upload);
        else if (result?.error) item.reject(new FileApiError(result.error));
        else item.reject(new Error("The admission page was incomplete."));
      });
    } catch (error) {
      batch.forEach((item) => { if (!item.cancelled) item.reject(error); });
    } finally {
      state.flushing = false;
      if (state.items.length) state.timer = setTimeout(() => { state.timer = undefined; void flushCreates(); }, 0);
    }
  }, []);

  // One admission call per file when the client has no batch transport (the test double),
  // otherwise a page of them: the URL is signed locally, so a page costs what one costs.
  const admitUpload = useCallback((input: FileUploadInput, key: string, signal?: AbortSignal) => new Promise<FileUploadSession>((resolve, reject) => {
    if (typeof api.createUploadBatch !== "function") { api.createUpload(input, key, signal).then(resolve, reject); return; }
    if (signal?.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
    const state = createBatch.current;
    const item: PendingCreate = { input, key, resolve, reject, cancelled: false };
    signal?.addEventListener("abort", () => { item.cancelled = true; reject(new DOMException("Upload canceled", "AbortError")); }, { once: true });
    state.items.push(item);
    if (state.items.length >= CREATE_BATCH_SIZE) {
      if (state.timer !== undefined) { clearTimeout(state.timer); state.timer = undefined; }
      void flushCreates();
    } else if (state.timer === undefined) {
      state.timer = setTimeout(() => { state.timer = undefined; void flushCreates(); }, 0);
    }
  }), [flushCreates]);

  const flushSettles = useCallback(async () => {
    const state = settleBatch.current;
    if (state.flushing) return;
    state.flushing = true;
    const batch = state.items.splice(0, COMMIT_BATCH_SIZE);
    try {
      const results = await api.commitUploadBatch(batch.map(({ id }) => id));
      batch.forEach((item, index) => {
        if (item.cancelled) return;
        const result = results[index];
        if (result?.result) item.resolve(result.result);
        else if (result?.error) item.reject(new FileApiError(result.error));
        else item.reject(new Error("The settlement page was incomplete."));
      });
    } catch (error) {
      batch.forEach((item) => { if (!item.cancelled) item.reject(error); });
    } finally {
      state.flushing = false;
      if (state.items.length) state.timer = setTimeout(() => { state.timer = undefined; void flushSettles(); }, 0);
    }
  }, []);

  const settleUpload = useCallback((id: string, signal?: AbortSignal) => new Promise<FileResource | FileOperation>((resolve, reject) => {
    if (typeof api.commitUploadBatch !== "function") { api.commitUpload(id, signal).then(resolve, reject); return; }
    if (signal?.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
    const state = settleBatch.current;
    const item: PendingSettle = { id, resolve, reject, cancelled: false };
    signal?.addEventListener("abort", () => { item.cancelled = true; reject(new DOMException("Upload canceled", "AbortError")); }, { once: true });
    state.items.push(item);
    if (state.items.length >= COMMIT_BATCH_SIZE) {
      if (state.timer !== undefined) { clearTimeout(state.timer); state.timer = undefined; }
      void flushSettles();
    } else if (state.timer === undefined) {
      state.timer = setTimeout(() => { state.timer = undefined; void flushSettles(); }, COMMIT_LINGER_MS);
    }
  }), [flushSettles]);
  const deadlines = useRef(new Set<ReturnType<typeof setTimeout>>());
  // Global part pool: the file at the head of the queue takes every slot it has parts to
  // fill, and the next part of that file is always the first waiter, so files finish one
  // after another in queue order instead of all advancing slowly. A later file joins on
  // the slots the head leaves — when it has fewer parts left than slots — never by taking
  // one away from it.
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
  // A prepared session outlives no row: dropping the row drops the session too, so a
  // cancelled or re-selected file does not leave an open session behind for its whole
  // two-hour idle window. The abort only discards the answer — the server may already
  // have created the session — which is exactly the case the create key exists for.
  const dropAdmission = useCallback((id: string) => {
    const admission = admissions.current.get(id);
    if (admission) {
      admissions.current.delete(id);
      admission.controller.abort();
    }
    admissionFailed.current.delete(id);
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
      transferring.current.delete(id);
      if (task.isDir) openDirs.current.delete(task.path);
      dropAdmission(id);
      releaseClaim(id);
    }
    schedulePublish();
  }, [dropAdmission, releaseClaim, schedulePublish]);
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

  // Admission is a stage of its own: claim the path, freeze the intent, allocate the
  // session. It runs ahead of the transfer window so a file that starts moving already
  // holds its session. The order inside it — claim, then create — is the order the server's
  // fences require: admitting a path is what ends an older open session for that path, so
  // creating before the cross-tab claim would let a second tab kill this one's session.
  const admitSession = useCallback(async (task: UploadTask, signal: AbortSignal): Promise<FileUploadSession> => {
    if (!pathClaims.current.has(task.id)) {
      let release: (() => void) | null;
      const claimController = new AbortController();
      const abortClaim = () => claimController.abort();
      signal.addEventListener("abort", abortClaim, { once: true });
      try { release = await claimUploadPath(owner.current, task.path, claimController.signal); }
      catch (error) {
        if (signal.aborted) throw error;
        throw new Error(t("drive.uploadCoordinationUnavailable"), { cause: error });
      } finally { signal.removeEventListener("abort", abortClaim); }
      if (signal.aborted) { release?.(); throw new DOMException("Upload canceled", "AbortError"); }
      if (!release) throw new Error(t("drive.uploadAlreadyInProgress"));
      pathClaims.current.set(task.id, release);
    }
    let intent = task.intent;
    if (!intent) {
      if (!task.file) throw new Error(t("drive.uploadFileUnavailable"));
      let condition: FileUploadInput["condition"] = { mode: "create" };
      if (task.overwrite) {
        const old = await api.metadata(task.path);
        if (old.kind !== "file" || !old.contentRevision) throw new Error("The destination cannot be replaced safely.");
        condition = { mode: "replace", contentRevision: old.contentRevision };
      }
      const sourceModifiedAt = await supportsSourceModifiedAt();
      if (signal.aborted) throw new DOMException("Upload canceled", "AbortError");
      intent = { path: task.path, size: task.size, mediaType: task.file?.type || "application/octet-stream", condition,
        ...(task.parents && condition.mode === "create" ? { parents: true as const } : {}),
        ...(sourceModifiedAt && task.modifiedAt !== undefined ? { sourceModifiedAt: new Date(task.modifiedAt).toISOString() } : {}) };
      // Retain the exact intent/key in memory before allocation, including a lost create ACK.
      patch(task.id, { intent });
    }
    const session = await admitUpload(intent, task.createKey, signal);
    patch(task.id, { uploadId: session.id, allocationRejected: false });
    return session;
  }, [admitUpload, patch]);

  // One admission per file, whoever asks first: the look-ahead that prepares a page and
  // the window that starts the file await the same request, so a file can never be
  // admitted twice — which for one path would end the other session.
  const ensureAdmission = useCallback((task: UploadTask) => {
    const existing = admissions.current.get(task.id);
    if (existing) return existing;
    const controller = new AbortController();
    const promise = admitSession(task, controller.signal);
    // An entry this tab prepares and then never starts (a cancellation) must not surface as
    // an unhandled rejection; whoever drives the row handles the refusal.
    promise.catch(() => {});
    const entry = { controller, promise };
    admissions.current.set(task.id, entry);
    void promise.then(
      () => { admissionFailed.current.delete(task.id); },
      () => {
        if (admissions.current.get(task.id) === entry) admissions.current.delete(task.id);
        // The window owns a refusal — it decides between waiting on the blocker, retrying
        // and failing the row — so the look-ahead steps back and lets the queue move on.
        admissionFailed.current.add(task.id);
        pumpRef.current();
      },
    );
    return entry;
  }, [admitSession]);
  // A file stops holding a transfer slot the moment its bytes are placed. The settlement
  // has its own page and its own bound, so waiting for it here would only make the next
  // file's first byte wait for this row's answer.
  const releaseWindow = useCallback((id: string) => {
    if (!transferring.current.delete(id)) return;
    pumpRef.current();
  }, []);

  const run = useCallback(async (id: string, runtime: Runtime) => {
    const signal = runtime.controller.signal;
    try {
      let task = tasks.current.get(id);
      if (!task) return;
      // The stored record is the authority for a wait, never the task's own copy of it:
      // the page settles a deletion that ended without deleting anything, and a wait
      // latched on the task would keep this attempt parked behind an Operation nobody is
      // waiting for anymore. Only the state it already observed is reused, so "Continue"
      // stays the thing that clears a finished deletion rather than every retry silently
      // overtaking it.
      const deletion = pendingFileOperation(task.path);
      if (deletion) {
        const known = task.waitingFor?.operationId === deletion.id ? task.waitingFor.state : "running";
        const waitingFor: FileOperationWait = { ...task.waitingFor, operationId: deletion.id, path: deletion.path, state: known, kind: "delete", from: "record" };
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
      let session: FileUploadSession;
      // True only when this attempt allocated the session. A session that was just created
      // reports how much it holds; one this attempt resumed may already carry parts whose
      // receipts were never seen, so only the first case may trust that count.
      let allocated = false;
      // A session the look-ahead prepared is already in hand: starting the file must not
      // ask the server again. A file with no admission yet is admitted here, so a queue
      // that has not caught up (or a resumed row) still works — that is the same request,
      // only later. Clearing the refused mark here re-arms the look-ahead for a file the
      // window is about to retry, which is the only place a refusal gets a second opinion.
      admissionFailed.current.delete(id);
      const admission = task.uploadId && !admissions.current.has(id) ? undefined : admissions.current.get(id) ?? ensureAdmission(task);
      if (!admission) {
        session = await api.uploadStatus(task.uploadId!, signal);
      } else {
        try { session = await admission.promise; }
        catch (error) {
          if (error instanceof ApiError && [400, 401, 403, 409, 413, 422, 507].includes(error.status)) patch(id, { allocationRejected: true });
          // An upload that holds the path is one that is committing: an open session fences
          // nothing, and the server ends an older open session for this path itself when it
          // admits a new one. So the only upload this refusal can name is a publication
          // already under way, and only that kind needs the session resource. Every other
          // holder (mkdir, delete, move, copy) is answered by its own Operation, so asking
          // /uploads/{id} about it was a request that could only ever 404 — one red line and
          // one round trip per conflict that could not change the outcome. A server that does
          // not name the kind still gets the probe, because then it is the only way to tell
          // the two apart.
          //
          // An Operation this branch reads is kept for the generic wait below: both ask the
          // same resource what it is doing, and a second round trip learns nothing.
          let blockerOperation: FileOperation | undefined;
          if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId
            && (error.operationKind === undefined || error.operationKind === "upload_commit")) {
            let blocking: FileUploadSession | undefined;
            try { blocking = await api.uploadStatus(error.operationId, signal); }
            catch (statusError) {
              // Only a definitive non-upload identity belongs in generic Operation waiting.
              if (!(statusError instanceof ApiError) || statusError.status !== 404) throw statusError;
            }
            if (signal.aborted) return;
            if (blocking?.id === error.operationId && relative(blocking.path) === task.path) {
              if (["aborted", "expired", "failed"].includes(blocking.state)) {
                const op = await api.getOperation(blocking.id, signal);
                blockerOperation = op;
                if (signal.aborted) return;
                if (op.id === blocking.id && op.state === "cancelled") {
                  patch(id, { status: "queued", allocationRejected: false });
                  return;
                }
              }
              if (blocking.state === "committed" && blocking.result) {
                complete(task, blocking.result);
                return;
              }
              // The path's earlier attempt is already publishing, and a second session over it
              // could only wait for this one. Adopt it instead: the poll finishes the row from
              // the same Operation and its own result. No abort and no allocation — which is
              // also what the contract forbids for committing/outcome_unknown work. Treated as
              // an unrelated blocker, this was the state that showed "waiting for deletion of
              // <a file>" and cost a poll interval per file.
              if (blocking.state === "committing" || blocking.state === "outcome_unknown" || blocking.state === "committed") {
                if (!signal.aborted) patch(id, { uploadId: blocking.id, operationId: blocking.operationId ?? error.operationId, status: "finalizing", uploaded: true, speed: 0, allocationRejected: false });
                return;
              }
            }
          }
          // A non-delete blocker is almost always one of this import's own short-lived steps:
          // the destination's missing ancestors are created inside another create, so a
          // sibling's folder creation can refuse this one for milliseconds. Ask once whether
          // it has already ended and retry; latching a poll for it cost a poll interval per
          // file and showed the row "waiting for deletion of <a file>" for a deletion that
          // never existed. A deletion is different: it is long, it is the user's decision,
          // and it must keep the wait and its own report.
          if (error instanceof ApiError && error.operationKind && error.operationKind !== "delete" && error.operationId) {
            // The resource has to be the blocking Operation: another operation's terminal
            // state, even one this attempt just read, says nothing about this path.
            const blocker = blockerOperation?.id === error.operationId
              ? blockerOperation
              : await api.getOperation(error.operationId, signal).catch(() => undefined);
            if (signal.aborted) return;
            if (blocker && blocker.id === error.operationId && ["succeeded", "failed", "cancelled"].includes(blocker.state)) {
              patch(id, { status: "queued", allocationRejected: false });
              return;
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
        allocated = true;
        admissions.current.delete(id);
      }
      patch(id, { uploadId: session.id, allocationRejected: false });
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
      // A session this attempt created holds exactly what it reported, so an empty one needs
      // no listing: one request saved per file, a quarter of a 23k-file import's traffic.
      // Anything else lists, because receipts the client never saw are the only way it can
      // know what is already durable.
      if (!allocated || session.receivedParts > 0) {
        for (const [number, size] of await readParts(signal)) known.set(number, size);
      }
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
          const timer = setTimeout(() => { deadlines.current.delete(timer); batch.signal.removeEventListener("abort", abort); resolve(); },
            Math.min(PART_RETRY_MAX_DELAY_MS, PART_RETRY_DELAY_MS * 2 ** attempt));
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
          // between its own parts, so this count is what the row reads to decide whether
          // the file is moving.
          let held = 0;
          const holdSlot = () => { held++; patch(id, { activeParts: held }); };
          const yieldSlot = () => { held--; releasePart(); patch(id, { activeParts: held }); };
          await Promise.all(Array.from({ length: Math.min(parallelism, pending.length) }, async () => {
            let holding = false;
            try {
              for (let number = pending.shift(); number !== undefined && !batch.signal.aborted; number = pending.shift()) {
                // Keep a held slot unless a part queued by an earlier file, or this file's
                // own earlier part, is waiting for it.
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
      // The bytes are placed. Give the slot back before waiting for the settlement: the
      // next admitted file starts streaming now, and this row's answer arrives on its own.
      releaseWindow(id);
      const result = await commitUpload(session.id, signal, settleUpload);
      if (signal.aborted) return;
      if (isOperation(result)) { patch(id, { operationId: result.id }); }
      else { complete(task, result); }
    } catch (error) {
      if (runtime.controller.signal.aborted) return;
      if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId) {
        const task = tasks.current.get(id);
        const waitingFor: FileOperationWait = { operationId: error.operationId, path: task?.path ?? "", state: "running", kind: waitKind(error.operationKind), from: "conflict" };
        patch(id, { status: "waiting", waitingFor, waitMessage: waitMessage(waitingFor), speed: 0 });
      } else { patch(id, { status: "failed", error: message(error), speed: 0 }); }
    } finally {
      running.current.delete(id);
      transferring.current.delete(id);
      pumpRef.current();
    }
  }, [acquirePart, applyOperation, complete, ensureAdmission, patch, releasePart, releaseWindow, settleUpload]);
  const pump = useCallback(() => {
    if (!ready.current || !mounted.current) return;
    // Two stages, two bounds. Admission runs ADMIT_AHEAD files ahead of the window, so a
    // file that starts moving already holds its session and its first byte never waits for
    // a create page; the window then only bounds files whose bytes are moving, which is
    // what the part pool can actually keep busy. A child waits only for its unfinished
    // directory task, which starts this again when it settles — and a directory is never
    // prepared ahead of its own children.
    const blockedBy = (path: string): boolean => {
      let slash = path.lastIndexOf("/");
      while (slash > 0) {
        path = path.slice(0, slash);
        if (openDirs.current.has(path)) return true;
        slash = path.lastIndexOf("/");
      }
      return false;
    };
    // The look-ahead is bounded by the bytes it holds, not only by the files: a prepared
    // session reserves its size against the owner's quota at admission, so depth has to
    // account for what is already reserved on this tab's behalf.
    let aheadBytes = 0;
    for (const id of admissions.current.keys()) aheadBytes += tasks.current.get(id)?.size ?? 0;
    for (const id of pending.current) {
      if (admissions.current.size >= ADMIT_AHEAD) break;
      if (admissions.current.has(id) || admissionFailed.current.has(id)) continue;
      const task = tasks.current.get(id);
      if (!task || task.isDir || task.status !== "queued" || task.cancelRequested) continue;
      // A file that already holds a session is resumed, not admitted again: replaying its
      // key only returns the session it already has, and what a repeated attempt needs to
      // learn first is that session's state — which the window asks for itself.
      if (task.uploadId || task.operationId || task.waitingFor) continue;
      // A path held by a recorded deletion is a wait, not an admission: the row has to say
      // so, and a session allocated ahead of that decision would be one nobody can use.
      if (pendingFileOperation(task.path)) continue;
      if (blockedBy(task.path)) continue;
      // A single file larger than the byte bound is admitted by the window itself, which
      // is the behaviour every file had before the look-ahead existed.
      if (aheadBytes + task.size > ADMIT_AHEAD_BYTES) break;
      aheadBytes += task.size;
      ensureAdmission(task);
    }
    const starts: UploadTask[] = [];
    for (const id of pending.current) {
      if (transferring.current.size + starts.length >= TRANSFER_WINDOW) break;
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
      transferring.current.add(task.id);
      patch(task.id, { status: "uploading", error: undefined });
    }
    for (const task of starts) void run(task.id, running.current.get(task.id)!);
  }, [ensureAdmission, patch, run]);
  pumpRef.current = pump;
  const cancel = useCallback((id: string) => {
    const root = tasks.current.get(id);
    const targets = [...tasks.current.values()].filter((task) => task.id === id || root?.isDir && task.path.startsWith(`${root.path}/`));
    for (const task of targets) {
      running.current.get(task.id)?.controller.abort();
      // A prepared session is this row's, so cancelling the row must discard it too: the
      // intent and key stay on the task, which is what lets the abort below recover the
      // allocation even when its acknowledgement never arrived.
      dropAdmission(task.id);
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
  }, [applyOperation, complete, dropAdmission, patch, publish, remove]);
  const retry = useCallback((id: string) => {
    const task = tasks.current.get(id);
    if (task?.cancelRequested) { cancel(id); return; }
    // A retry that starts a new session must not resume the old one, and a retry that keeps
    // its key must still get a fresh look (the prepare may have been refused before).
    if (task?.terminalState) dropAdmission(id);
    patch(id, { status: "queued", error: undefined, speed: 0, retryRequested: true,
      ...(task?.terminalState ? { uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, uploaded: false, loaded: 0 } : {}) });
    publish();
    pumpRef.current();
  }, [cancel, dropAdmission, patch, publish]);
  // Resuming is a decision, so it is a gesture: the task goes back to the queue with its
  // record gone, and the next attempt asks the server instead of the browser what the
  // path may take. A deletion that is still queued, running or of unknown outcome is not
  // resumable from here — it is still the one holding the path.
  const resume = useCallback((id: string) => {
    const task = tasks.current.get(id);
    const wait = task?.waitingFor;
    if (!task || !wait || (wait.state !== "failed" && wait.state !== "cancelled")) return;
    settleFileOperation(wait.operationId);
    patch(id, { status: "queued", waitingFor: undefined, waitMessage: undefined, speed: 0 });
    publish();
    pumpRef.current();
  }, [patch, publish]);
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
          // A new selection may reuse the path but not the session prepared for the old file;
          // a session that starts again must come from a fresh admission, and the size check
          // above is the only guarantee that its geometry would have matched.
          dropAdmission(old.id);
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
    // Queue order decides which file finishes first: inside one file parts ascend, and
    // across files each freed slot goes to the earliest-queued file asking for one. That
    // order must therefore be the one a person can see — the row order: folders first,
    // then name. A drop hands the files over in whatever order the OS listed them, which
    // the name-sorted list never shows, so keeping it lets a file further down the list
    // stream while the one above it waits, which reads as the queue picking at random.
    // This orders only the selection being added; a later selection still queues behind
    // everything already running or waiting.
    added.sort(compareQueueOrder);
    if (added.length) insert(added);
    // The user just picked the files; publish the rows before the first network hop.
    publish();
    pumpRef.current();
  }, [dropAdmission, insert, patch, publish]);
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
              // Longer than a request budget window, because that request may now wait for
              // Retry-After before it is even sent.
              const deadline = setTimeout(() => timeout.abort(), 60000);
              deadlines.current.add(deadline);
              try { results.set(id, await api.getOperation(id, timeout.signal)); }
              catch (error) { results.set(id, error instanceof Error ? error : new Error(message(error))); }
              finally { clearTimeout(deadline); deadlines.current.delete(deadline); controller.signal.removeEventListener("abort", abort); }
            }
            if (!mounted.current || !tasks.current.has(task.id)) continue;
            const result = results.get(id)!;
            if (task.waitingFor) {
              if (task.waitingFor.from === "record" && !pendingFileOperation(task.path)) {
                // The record is the authority, and it is gone: whoever kept it settled the
                // deletion and already reported the outcome. A task latched on that id
                // would otherwise sit on "waiting" for the rest of the page's life.
                patch(task.id, { status: "queued", waitingFor: undefined, waitMessage: undefined });
              } else if (result instanceof ApiError && (result.status === 404 || result.status === 410)) {
                // The server no longer knows this Operation, so nothing can still be waiting
                // on it (records are pruned after retention). Continuing needs no decision:
                // the server is the judge of the path, and a real guard answers the next
                // request with its own 409 and the Operation this queue can then wait on.
                settleFileOperation(id);
                patch(task.id, { status: "queued", waitingFor: undefined, waitMessage: undefined });
              } else if (result instanceof Error || result.id !== id) {
                const waitingFor = { ...task.waitingFor, statusUnavailable: true };
                patch(task.id, { waitingFor, waitMessage: waitMessage(waitingFor) });
              } else if (result.state === "succeeded") {
                settleFileOperation(id); patch(task.id, { status: "queued", waitingFor: undefined, waitMessage: undefined });
              } else {
                const waitingFor: FileOperationWait = { ...task.waitingFor, state: result.state, statusUnavailable: false };
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
      transferring.current.clear();
      // Prepared sessions have no row to outlive: leaving the page drops them the way it
      // dropped every other in-memory promise, and their idle window expires them.
      for (const admission of admissions.current.values()) admission.controller.abort();
      admissions.current.clear();
      admissionFailed.current.clear();
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
  return { tasks: published, hasActiveUploads, enqueue, cancel, retry, resume };
}
