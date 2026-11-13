/**
 * Upload tasks and File references live for the whole tab, not for one page.
 *
 * The queue used to be page state, and the page unmounting took three things with it: an
 * in-flight selection became unreachable, a failure list disappeared the moment its "fix"
 * link navigated to the account page, and a mount-time `/api/auth/me` failure left every row
 * "queued" with nothing that could ever retry it. Owning the queue at module level fixes all
 * three at once: the engine is created on the first page that uses it and lives until the tab
 * closes, so a mount is a subscription and a navigation is only a detach.
 *
 * A module singleton rather than a context provider above the router: this SPA has no
 * providers, the queue's lifetime is the tab's rather than a route subtree's, and a plain
 * owner is exercisable without a DOM (the test harness drives the hook directly).
 */
import { useEffect, useState } from "react";
import { ApiError, FileApiError, api, type FileProblem, type FileUploadCommit, type Item, type FileOperation, type FileUploadInput, type FileUploadPartGrant, type FileUploadSession } from "../api/client";
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
// look-ahead; commits are bounded by the worker pool that settles them.
const CREATE_BATCH_SIZE = 32;
const COMMIT_BATCH_SIZE = 16;
// A settlement page leaves when it is full, or when its oldest item has waited this long.
// Sending each item the moment it arrived — what this did — made every page a page of one, so
// every file cost one request of its own and the page size never meant anything.
const SETTLE_PAGE_WAIT_MS = 500;

// Sessions kept in hand ahead of the transfer stage: four pages of thirty-two, so the stage
// can be refilled four times over before a create page (~0.6 s for 32 against this bucket) has
// to answer, and a page that arrives late costs no part slot. Together with the claims, this
// is the most sessions one tab holds open: 192, against an idle window of two hours — the
// depth is what "always sixteen moving" costs, and the byte bound below is what keeps it
// honest for files that are not tiny.
const ADMIT_AHEAD = 128;
// A prepared session reserves its bytes against the owner's quota the moment it is admitted,
// so depth is bounded by bytes as well as by count: 64 small files are nothing, 64 videos
// would reserve gigabytes for a minute before their turn. One gigabyte is four waves of the
// part pool at 16 MiB, i.e. the point where the look-ahead would reserve more than the link
// can consume while the window rotates.
const ADMIT_AHEAD_BYTES = 1_000_000_000;
// Files the transfer stage may hold at once, whether they are moving bytes or waiting for a
// session. A claim is what gives a queued file an owner: it keeps a refusal answering to one
// row instead of being retried by the window, and it bounds the Blobs and sessions one tab
// holds. It deliberately does not wait for a session — the claim is taken, then the file waits
// for its create page and for a part slot inside its own run — because a window slot taken
// before the session existed was a slot held by a file that could not send a byte, which is
// what made an import run in waves with every row already answered by the provider.
//
// Four times the part pool, so the queue behind the sixteen slots never empties while a page
// of thirty-two admissions is in flight: the whole point of the depth is a slot that frees is
// filled by a file that already has its session.
const CLAIM_WINDOW = 64;
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
async function commitUpload(id: string, signal: AbortSignal | undefined, settle: (id: string, signal?: AbortSignal) => Promise<FileUploadCommit | FileOperation>): Promise<FileUploadCommit | FileOperation> {
  for (let attempt = 0; ; attempt++) {
    try { return await settle(id, signal); }
    catch (error) {
      if (signal?.aborted) throw error;
      // A page-level failure is the queue's to retry, not this file's: retrying it here
      // would re-enqueue the row in a queue that is already paused, once per attempt.
      if (isQueuePaused(error)) throw error;
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
  /**
   * The content revision a conflict probe saw at the destination.
   *
   * A replace condition names the exact bytes it may overwrite, so building one normally
   * costs a metadata read per file. The refused create that discovered the conflict has
   * just paid for that read, and carrying it here is what makes the second attempt of a
   * file one request instead of two. The server still checks it at the write: a file that
   * changed in between is refused, never silently replaced.
   */
  observedRevision?: string;
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
  /**
   * The upload's server-side session is known to be in a terminal state.
   *
   * Recorded where the queue can establish it: a session read as `committed`, or a
   * successful commit, whose published file is that session's terminal outcome. A row that
   * merely reads `completed` is not evidence by itself — see `isFinishedUpload` — because a
   * local `discard` must never drop a row while a session it names could still hold the
   * owner's quota.
   */
  sessionTerminal?: boolean;
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
/**
 * What one cancel gesture took with it.
 *
 * A cancelled row leaves the sheet at once (the projection drops it), so the counts it was
 * carrying leave with it. The gesture has to report them itself, or an import stopped at 3%
 * of twenty thousand files is indistinguishable from one that never held more than six
 * hundred — and the person is left reading 489 / 609, 80% for a folder they cancelled.
 */
export interface UploadCancelSummary {
  /** Files and the folders that needed a task of their own, in the cancelled selection. */
  items: number;
  completedItems: number;
}
/**
 * What a person decided about a destination that already exists.
 *
 * The "all" answers exist because one conflict is never the whole story. A folder being
 * re-uploaded meets an existing file at almost every path, and asking per file is how an
 * Apple Health export turned into thousands of dialogs that blocked the tab: the browser's
 * own confirm is modal, cannot say "for the rest", and every refusal cost a create the
 * server had to answer before it could ask. The standing answer also makes the later
 * uploads cheaper — their create carries the replace condition from the first attempt
 * instead of being refused first — and it lasts exactly as long as the import that made it.
 */
export type ConflictChoice = "replace" | "replace-all" | "skip" | "skip-all";
/** The standing answer, or `ask` when every conflict is its own question. */
export type ConflictPolicy = "ask" | "replace" | "skip";
export interface UploadQueue {
  tasks: UploadTask[];
  hasActiveUploads: boolean;
  /** The ids of the tasks this selection created; a path that was already queued adds none. */
  enqueue(entries: UploadEntry[], dir: string): string[];
  /**
   * Cancel one row, or a whole selection of rows in one gesture.
   *
   * A folder row is a projection: it names every descendant file, not a task of its own, so
   * the sheet hands the whole list over. Resolving each id separately walked the queue once
   * per id and published once per id, which is quadratic in the cancelled tree — the shape
   * that froze the tab when a large import was stopped.
   *
   * Cancelling is a decision about the whole gesture, not a per-row status: `uploadRows`
   * drops a requested cancellation from the projection immediately, so the gesture answers
   * with what it covered and the page reports it once. A teardown that then fails keeps its
   * task — as a failure, where the person can retry the cancel — because a still-open
   * session holds its reservation against the owner's quota until someone ends it.
   */
  cancel(ids: string | string[]): UploadCancelSummary | null;
  /**
   * Drop rows whose server-side session is demonstrably finished, without a request.
   *
   * A deletion that was already accepted cannot be undone, so the completed rows it covered
   * are history. Sending each one to `cancel` spent two requests per file — an abort the
   * server answers `409 already_committed` and the status read that confirms it — just to
   * remove a row the queue already knew was over: about 2,000 requests for a 1,000-file
   * folder, against the owner's rate budget at the exact moment a large delete completed.
   *
   * `demonstrably` is the rule, not `status === "completed"`: only a completed row whose
   * session is known terminal (or a completed folder row, which never allocated one) is
   * dropped. A completed row whose session fate the queue did not establish is left in the
   * queue and returned in the answer, so the caller can cancel it and let the server decide
   * instead of guessing. Removing a row changes no pause state and touches no batch entry:
   * a finished row has no pending admission, settlement or Operation of its own.
   *
   * Returns the ids still in the queue that were not dropped, in any order; the caller
   * decides whether to cancel them.
   */
  discard(ids: string | string[]): string[];
  retry(id: string): void;
  /**
   * Why the whole queue is stopped, or null while it is running.
   *
   * One message for one cause: a page-level failure (the server unreachable, a whole batch
   * answering 5xx/401/403/429, an answer that omitted an item) is the queue's condition,
   * not a file's. The rows it carried stay queued, so the page shows this sentence once
   * instead of a wall of identical row failures.
   */
  pauseMessage: string | null;
  /**
   * Retry the page that stopped the queue.
   *
   * This is where a queue-level failure's retry lives now. A per-file failure is still
   * answered by that row's `retry`, because it names a file and its own decision; a page
   * failure names no row, so the only action is to send the stopped page again — the rows
   * it carried were never failed, and files parked behind the pause resume with it.
   */
  resumeQueue(): void;
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
  /**
   * The destination this queue is asking about, or null when nobody is being asked.
   *
   * The question is in the page, not in the browser's confirm: that dialog blocks the tab,
   * cannot offer the standing answer, and is raised from inside the queue's own loop, so
   * one folder of existing files stopped everything until every dialog was dismissed.
   */
  conflict: string | null;
  /** This import's standing answer, so the sheet can show it and take it back. */
  conflictPolicy: ConflictPolicy;
  /** Answer the question on screen; the "all" answers also settle the ones behind it. */
  answerConflict(choice: ConflictChoice): void;
  /** Stop applying the standing answer: the next conflict asks again. */
  askForConflicts(): void;
  /**
   * A new gesture.
   *
   * The standing answer belongs to the import that made it. A drop made an hour later
   * inherits nothing: replacing a file is permanent, and a decision nobody was asked about
   * this time is exactly the silent overwrite the create condition exists to prevent.
   */
  beginImport(): void;
}
/**
 * The engine behind the exported hook: the queue API plus the few lifecycle and ownership
 * operations only the hook and the test harness need.
 */
interface QueueOwner extends UploadQueue {
  /** Attach the first page and start the tab-lifetime work; idempotent across remounts. */
  start(): void;
  /** End the tab: abort everything and drop the ownership claims. Never called by the SPA. */
  dispose(): void;
  subscribe(listener: () => void): () => void;
  /** A page's settle handler, present only while that page is mounted. */
  onCompleted(handler: (task: UploadTask, item: Item) => void): () => void;
  snapshot(): UploadQueue;
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
/**
 * Whether this row can leave the queue without asking the server anything.
 *
 * `status === "completed"` alone is not proof: a completion path that never established
 * the session's fate would leave `sessionTerminal` unset, and dropping such a row locally
 * could strand an open session holding the owner's quota. Those rows go through `cancel`,
 * which asks the server, instead of being guessed away. A completed folder row is the other
 * safe case: it never allocated a session, so there is nothing to abort or read.
 */
export function isFinishedUpload(task: UploadTask): boolean {
  if (task.status !== "completed") return false;
  return task.sessionTerminal === true || (task.isDir && !task.uploadId);
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
/**
 * A page failed as a whole, so no row may claim it.
 *
 * The batch transport carries up to 32 admissions or 16 settlements in one request, and the
 * request either answers every item or fails once: the connection was lost, the whole page
 * answered 5xx (or 401/403 after the session expired, or 429), or the answer did not
 * describe every item. That failure says nothing about any single file's destination, and
 * attributing it to each carried file turned one outage into a page of red rows, each of
 * which had to be retried by hand even after the cause cleared. It is carried as its own
 * error so the row's attempt can put itself back in the queue instead of failing.
 */
class QueuePaused extends Error {
  constructor(text: string) {
    super(text);
    this.name = "QueuePaused";
  }
}
function isQueuePaused(error: unknown): error is QueuePaused {
  return error instanceof QueuePaused;
}
function isOperation(value: Item | FileOperation): value is FileOperation { return "state" in value && "progress" in value; }
/**
 * What is at this path now, or null when nothing is.
 *
 * A destination that was taken can be gone by the time the queue asks about it — a re-import
 * runs while someone else edits the same tree. A 404 is an answer (the name is free again);
 * every other failure is a read that failed and still belongs to the row's error.
 */
async function observedEntry(path: string): Promise<Item | null> {
  return api.metadata(path).catch((error: unknown) => {
    if (error instanceof ApiError && error.status === 404) return null;
    throw error;
  });
}
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
}

interface PendingCreate { input: FileUploadInput; key: string; resolve: (session: FileUploadSession) => void; reject: (error: unknown) => void; cancelled: boolean }
interface PendingSettle { id: string; resolve: (result: FileUploadCommit | FileOperation) => void; reject: (error: unknown) => void; cancelled: boolean }

/** One look-ahead admission in flight or in hand, one per file. */
interface AdmissionEntry { controller: AbortController; promise: Promise<FileUploadSession>; ready: boolean }
/** The receipt hashes a relay session's parts were verified against, for reselection. */
interface VerifiedFile { file: File; parts: Map<number, string> }
/** The global part pool: slots in flight and the queue ordered by file rank, then part. */
interface PartPool { active: number; waiters: PartWaiter[] }

/**
 * The cells and operations one attempt's steps may use — and nothing else.
 *
 * The steps are module-level functions rather than nested closures so each stage can be
 * read on its own. This interface is the whole of what they share with the engine: the
 * maps and sets are the engine's own cells (shared by reference, never copied), the two
 * getters read cells that are reassigned during the tab's lifetime, and the callbacks are
 * the engine's already-published operations.
 */
interface UploadRunHost {
  readonly tasks: Map<string, UploadTask>;
  readonly admissions: Map<string, AdmissionEntry>;
  readonly admissionFailed: Set<string>;
  readonly pathClaims: Map<string, () => void>;
  readonly verified: Map<string, VerifiedFile>;
  readonly deadlines: Set<ReturnType<typeof setTimeout>>;
  readonly partPool: PartPool;
  readonly ranks: Map<string, number>;
  readonly transferring: Set<string>;
  /** The authenticated owner, empty until the mount-time session check resolves. */
  owner(): string;
  /** The import's standing answer to "this destination already exists". */
  conflictPolicy(): ConflictPolicy;
  patch(id: string, changes: Partial<UploadTask>): void;
  complete(task: UploadTask, item: Item): void;
  skip(id: string): void;
  applyOperation(task: UploadTask, operation: FileOperation): void;
  admitUpload(input: FileUploadInput, key: string, signal?: AbortSignal): Promise<FileUploadSession>;
  /** Ask the engine for the next turn: a step that finished something schedules the pump. */
  requestPump(): void;
  askConflict(path: string): Promise<"replace" | "skip">;
  settleUpload(id: string, signal?: AbortSignal): Promise<FileUploadCommit | FileOperation>;
  releaseWindow(id: string): void;
  acquirePart(rank: number, number: number, signal: AbortSignal): Promise<void>;
  releasePart(): void;
}

/**
 * Step: a recorded deletion at this path owns the destination, so this attempt waits.
 *
 * Contract
 * - Caller: the row exists, no bytes have moved for this attempt, and the path may be
 *   blocked. Nothing else needs to be read first.
 * - Returns `true` when the row is now `waiting` and the attempt is over, `false` when the
 *   path is free and the attempt may continue.
 * - The browser's durable record is the authority, never the task's latched copy: a record
 *   the page has already settled is not a wait, while a terminal state this row observed
 *   earlier is kept so "Continue" stays the gesture that clears it.
 * - Writes only `status`, `waitingFor` and `waitMessage`; sends no request.
 */
function waitOnRecordedDeletion(host: UploadRunHost, id: string, task: UploadTask): boolean {
  const deletion = pendingFileOperation(task.path);
  if (!deletion) return false;
  const known = task.waitingFor?.operationId === deletion.id ? task.waitingFor.state : "running";
  const waitingFor: FileOperationWait = { ...task.waitingFor, operationId: deletion.id, path: deletion.path, state: known, kind: "delete", from: "record" };
  host.patch(id, { status: "waiting", waitingFor, waitMessage: waitMessage(waitingFor) });
  return true;
}

/**
 * Step: read the Operation a row already names and apply its answer.
 *
 * Contract
 * - Caller: the row has an `operationId` (a publication or folder creation this queue is
 *   already following) and no recorded deletion.
 * - Returns `true` when this attempt is over, `false` when the row has no Operation and a
 *   later stage applies.
 * - Sends exactly one `GET /operations/{id}`; never allocates, transfers or aborts. A read
 *   failure propagates to `abandonAttempt`.
 */
async function adoptRecordedOperation(host: UploadRunHost, task: UploadTask, signal: AbortSignal): Promise<boolean> {
  if (!task.operationId) return false;
  const operation = await api.getOperation(task.operationId, signal);
  if (!signal.aborted) host.applyOperation(task, operation);
  return true;
}

/**
 * Step: create the folder a directory row stands for.
 *
 * Contract
 * - Caller: `task.isDir` is true and no deletion or Operation holds the path.
 * - Completes the row from a synchronous answer, or records the Operation and leaves the
 *   row `finalizing` for the queue's own poll. Either way the attempt is over on return.
 * - A `409 name_conflict` is a folder that already exists, not a failure: the destination is
 *   re-read and only a folder answers for it. Every other failure propagates to
 *   `abandonAttempt`.
 * - Holds no transfer slot and moves no byte.
 */
async function createDirectoryTask(host: UploadRunHost, id: string, task: UploadTask, signal: AbortSignal): Promise<void> {
  let result: Item | FileOperation;
  try { result = await api.mkdir(task.path); }
  catch (error) {
    if (!(error instanceof ApiError) || error.code !== "name_conflict") throw error;
    result = await api.metadata(task.path);
    if (result.kind !== "folder") throw error;
  }
  if (signal.aborted) return;
  if (isOperation(result)) { host.patch(id, { operationId: result.id, status: "finalizing" }); }
  else { if (result.kind !== "folder") throw new Error("A file occupies the destination folder."); host.complete(task, result); }
}

/** An open session to move bytes into, or `null` when admission ended the attempt. */
type AdmissionResult = { session: FileUploadSession; allocated: boolean } | null;

/**
 * Admission's request side: claim the path, freeze the intent, allocate the session.
 *
 * Contract
 * - Caller: an authenticated owner and a file row whose intent may or may not exist yet.
 * - The cross-tab claim is taken before the create, because admitting a path is what ends
 *   an older open session for that path: creating before the claim would let a second tab
 *   kill this one's session.
 * - Returns the session and records its `uploadId` on the row; throws the refusal every
 *   time (there is no "handled" answer here — that is the window's decision in
 *   `admitUploadAttempt`, so the look-ahead never answers a refusal a row should own).
 * - The intent and create key are retained in memory before allocation, so a lost create
 *   ACK can be recovered by replaying the same idempotent request.
 */
async function prepareUploadSession(host: UploadRunHost, task: UploadTask, signal: AbortSignal): Promise<FileUploadSession> {
  if (!host.pathClaims.has(task.id)) {
    let release: (() => void) | null;
    const claimController = new AbortController();
    const abortClaim = () => claimController.abort();
    signal.addEventListener("abort", abortClaim, { once: true });
    try { release = await claimUploadPath(host.owner(), task.path, claimController.signal); }
    catch (error) {
      if (signal.aborted) throw error;
      throw new Error(t("drive.uploadCoordinationUnavailable"), { cause: error });
    } finally { signal.removeEventListener("abort", abortClaim); }
    if (signal.aborted) { release?.(); throw new DOMException("Upload canceled", "AbortError"); }
    if (!release) throw new Error(t("drive.uploadAlreadyInProgress"));
    host.pathClaims.set(task.id, release);
  }
  let intent = task.intent;
  if (!intent) {
    if (!task.file) throw new Error(t("drive.uploadFileUnavailable"));
    let condition: FileUploadInput["condition"] = { mode: "create" };
    if (task.overwrite && task.observedRevision) {
      // The refusal that raised the question observed the destination, so this row can
      // name the exact bytes it may replace without another read.
      condition = { mode: "replace", contentRevision: task.observedRevision };
    } else if (host.conflictPolicy() === "replace") {
      // The person answered "replace every file that already exists" for this import, and
      // that answer is the authorization: this row names no revision at all. Naming one
      // means reading the destination before the create can be sent — one round trip per
      // file — and naming the wrong one (an index left behind by an interrupted import)
      // is refused forever. The server observes the destination and binds that revision
      // instead, so the write stays conditional on the bytes it found.
      condition = { mode: "overwrite" };
    } else if (task.overwrite) {
      // A replace condition names the bytes it is allowed to overwrite, and a standing
      // "replace" is a decision about a destination that can be gone by the time this
      // file's turn comes — a re-imported tree edited between the drop and this row. An
      // absent destination cannot be named (412 target_changed), so the row creates
      // instead: the same thing the person would do by hand, and not a failure that has
      // to be answered again for every file behind it.
      const old = await observedEntry(task.path);
      if (old) {
        if (old.kind !== "file" || !old.contentRevision) throw new Error("The destination cannot be replaced safely.");
        condition = { mode: "replace", contentRevision: old.contentRevision };
      }
    }
    const sourceModifiedAt = await supportsSourceModifiedAt();
    if (signal.aborted) throw new DOMException("Upload canceled", "AbortError");
    intent = { path: task.path, size: task.size, mediaType: task.file?.type || "application/octet-stream", condition,
      ...(task.parents && condition.mode === "create" ? { parents: true as const } : {}),
      ...(sourceModifiedAt && task.modifiedAt !== undefined ? { sourceModifiedAt: new Date(task.modifiedAt).toISOString() } : {}) };
    // Retain the exact intent/key in memory before allocation, including a lost create ACK.
    host.patch(task.id, { intent });
  }
  const session = await host.admitUpload(intent, task.createKey, signal);
  host.patch(task.id, { uploadId: session.id, allocationRejected: false });
  return session;
}

/**
 * One admission per file, whoever asks first: the look-ahead that prepares a page and the
 * window that starts the file await the same request, so a file can never be admitted
 * twice — which for one path would end the other session.
 *
 * Contract
 * - Caller: the pump (look-ahead or window), for a file row that is eligible to start.
 * - Returns the existing entry when one exists; otherwise starts `prepareUploadSession` and
 *   registers it before returning, so a second caller joins instead of asking again.
 * - A refusal removes the entry and marks the file "already refused" unless it was a
 *   page-level pause; the look-ahead never answers the refusal itself, because a refusal is
 *   the row's (the window decides between waiting, retrying and failing).
 * - A prepared session this tab never starts is aborted by the row's teardown, never here.
 */
function ensureUploadAdmission(host: UploadRunHost, task: UploadTask): AdmissionEntry {
  const existing = host.admissions.get(task.id);
  if (existing) return existing;
  const controller = new AbortController();
  const promise = prepareUploadSession(host, task, controller.signal);
  // An entry this tab prepares and then never starts (a cancellation) must not surface as
  // an unhandled rejection; whoever drives the row handles the refusal.
  promise.catch(() => {});
  const entry: AdmissionEntry = { controller, promise, ready: false };
  host.admissions.set(task.id, entry);
  void promise.then(
    () => {
      entry.ready = true;
      host.admissionFailed.delete(task.id);
      // A file starts the moment its own session is in hand, not when its wave does: a
      // transfer slot handed to a file that is still waiting for a create page is a slot
      // that is not moving bytes.
      host.requestPump();
    },
    (error) => {
      if (host.admissions.get(task.id) === entry) host.admissions.delete(task.id);
      // A paused page is not this destination's refusal: the look-ahead must be free to
      // prepare this file again once the queue resumes, so the paused error does not make
      // it "already refused".
      if (!isQueuePaused(error)) host.admissionFailed.add(task.id);
      // The window owns a refusal — it decides between waiting on the blocker, retrying
      // and failing the row — so the look-ahead steps back and lets the queue move on.
      host.requestPump();
    },
  );
  return entry;
}

/**
 * Step: admission — claim the path, freeze the intent, obtain the session.
 *
 * Contract
 * - Caller: a file row (not a directory) with no recorded deletion or Operation wait, and
 *   an authenticated owner. The admission may run ahead of the transfer window; the
 *   caller's snapshot is the row as the attempt started.
 * - Returns the session and whether *this attempt* allocated it. Only a session this
 *   attempt created may trust its reported `receivedParts`; a resumed one must list.
 * - Returns `null` when admission itself decided the row's next state: an allocation
 *   refusal marked for the failure list, a committing publication adopted, a blocker
 *   Operation watched or already ended, a lost replace target re-queued, or a name conflict
 *   answered (replace or skip). Every other refusal is thrown for `abandonAttempt`.
 * - Ordering is the server's fence: the cross-tab claim is taken before the create, so
 *   admitting a path is what ends an older open session for it.
 * - Sends at most one create (or one status read when the row already holds a session),
 *   plus at most one read of the Operation that holds the path.
 */
async function admitUploadAttempt(host: UploadRunHost, id: string, task: UploadTask, signal: AbortSignal): Promise<AdmissionResult> {
  // A session the look-ahead prepared is already in hand: starting the file must not ask
  // the server again. A file with no admission yet is admitted here, so a queue that has
  // not caught up (or a resumed row) still works — that is the same request, only later.
  // Clearing the refused mark here re-arms the look-ahead for a file the window is about
  // to retry, which is the only place a refusal gets a second opinion.
  host.admissionFailed.delete(task.id);
  const entry = task.uploadId && !host.admissions.has(task.id) ? undefined : host.admissions.get(task.id) ?? ensureUploadAdmission(host, task);
  if (!entry) return { session: await api.uploadStatus(task.uploadId!, signal), allocated: false };
  let session: FileUploadSession;
  try { session = await entry.promise; }
  catch (error) {
    if (error instanceof ApiError && [400, 401, 403, 409, 413, 422, 507].includes(error.status)) host.patch(id, { allocationRejected: true });
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
    // An Operation this branch reads is kept for the generic wait below: both ask
    // the same resource what it is doing, and a second round trip learns nothing.
    let blockerOperation: FileOperation | undefined;
    if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId
      && (error.operationKind === undefined || error.operationKind === "upload_commit")) {
      let blocking: FileUploadSession | undefined;
      try { blocking = await api.uploadStatus(error.operationId, signal); }
      catch (statusError) {
        // Only a definitive non-upload identity belongs in generic Operation waiting.
        if (!(statusError instanceof ApiError) || statusError.status !== 404) throw statusError;
      }
      if (signal.aborted) return null;
      if (blocking?.id === error.operationId && relative(blocking.path) === task.path) {
        if (["aborted", "expired", "failed"].includes(blocking.state)) {
          const operation = await api.getOperation(blocking.id, signal);
          blockerOperation = operation;
          if (signal.aborted) return null;
          if (operation.id === blocking.id && operation.state === "cancelled") {
            host.patch(id, { status: "queued", allocationRejected: false });
            return null;
          }
        }
        if (blocking.state === "committed" && blocking.result) {
          host.patch(id, { sessionTerminal: true });
          host.complete(task, blocking.result);
          return null;
        }
        // The path's earlier attempt is already publishing, and a second session over it
        // could only wait for this one. Adopt it instead: the poll finishes the row from
        // the same Operation and its own result. No abort and no allocation — which is
        // also what the contract forbids for committing/outcome_unknown work. Treated as
        // an unrelated blocker, this was the state that showed "waiting for deletion of
        // <a file>" and cost a poll interval per file.
        if (blocking.state === "committing" || blocking.state === "outcome_unknown" || blocking.state === "committed") {
          if (!signal.aborted) host.patch(id, { uploadId: blocking.id, operationId: blocking.operationId ?? error.operationId, status: "finalizing", uploaded: true, speed: 0, allocationRejected: false });
          return null;
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
      if (signal.aborted) return null;
      if (blocker && blocker.id === error.operationId && ["succeeded", "failed", "cancelled"].includes(blocker.state)) {
        host.patch(id, { status: "queued", allocationRejected: false });
        return null;
      }
    }
    if (error instanceof ApiError && error.status === 412 && error.code === "target_changed" && task.overwrite && task.observedRevision) {
      // The destination the replace condition named is gone — the tree was edited
      // between the conflict probe and this attempt. Nothing is left to replace, so
      // the row creates; leaving it failed would turn a deleted file into an import
      // that has to be answered again.
      host.patch(id, { overwrite: false, observedRevision: undefined, intent: undefined, createKey: taskId(), status: "queued" });
      return null;
    }
    if (error instanceof ApiError && error.status === 409 && error.code === "name_conflict" && !task.overwrite) {
      // The refusal already names the revision the server observed at the destination,
      // which is the only value a replacement can prove it may overwrite: reading the
      // same file again would spend a round trip per file to learn what this answer
      // just said. A refusal without it (a provider's own 409, a folder) still reads.
      const named = error instanceof FileApiError ? error.problem.observedRevision : undefined;
      const old = named === undefined ? await observedEntry(task.path) : { kind: "file" as const, contentRevision: named };
      if (!old) { host.patch(id, { status: "queued", intent: undefined, createKey: taskId() }); return null; }
      if (old.kind === "file") {
        if (await host.askConflict(task.path) === "replace") {
          host.patch(id, { overwrite: true, observedRevision: old.contentRevision ?? undefined, intent: undefined, createKey: taskId(), status: "queued" }); return null;
        }
        host.skip(id); return null;
      }
    }
    throw error;
  }
  host.admissions.delete(id);
  return { session, allocated: true };
}

/**
 * What to do with the session admission returned.
 *
 * `stop` means the row's next state is already written; `settle` means the bytes are
 * already at the destination (a retried publication whose object the server recognized);
 * `upload` means a transfer slot should move the missing parts first.
 */
type TransferPlan =
  | { kind: "stop" }
  | { kind: "settle"; task: UploadTask; session: FileUploadSession }
  | { kind: "upload"; task: UploadTask; session: FileUploadSession; file: File; allocated: boolean };

/**
 * Step: decide what the session admission returned still needs.
 *
 * Contract
 * - Caller: admission returned a session and the patched row was re-read, so `task` is the
 *   snapshot the transfer will use.
 * - A `committed` session completes the row; a `committing` or `outcome_unknown` one is
 *   adopted by its Operation; a terminal one fails — or re-queues when the person asked
 *   for a retry; an `open` one moves bytes unless the server says they are already there.
 * - Returns the plan; throws when an open session's file is gone, the wrong size, or an
 *   unknown state arrives, and the abandon step reports it.
 * - Sends no request and takes no transfer slot; the row's `uploadId` is already recorded.
 */
function takeUploadSession(host: UploadRunHost, id: string, task: UploadTask, admission: { session: FileUploadSession; allocated: boolean }): TransferPlan {
  const { session } = admission;
  if (session.state === "committed") {
    if (!session.result) throw new Error("The upload result is incomplete.");
    host.patch(id, { sessionTerminal: true });
    host.complete(task, session.result);
    return { kind: "stop" };
  }
  if (session.state === "committing" || session.state === "outcome_unknown") {
    host.patch(id, { operationId: session.operationId, status: "finalizing", uploaded: true });
    return { kind: "stop" };
  }
  if (session.state !== "open") {
    if (session.state === "failed" || session.state === "aborted" || session.state === "expired") {
      if (task.retryRequested && task.file) {
        host.patch(id, { status: "queued", uploadId: undefined, operationId: undefined, intent: undefined, observedRevision: undefined, createKey: taskId(), terminalState: undefined, retryRequested: false, uploaded: false, loaded: 0 });
        return { kind: "stop" };
      }
      host.patch(id, { terminalState: session.state });
    }
    throw new Error(`Upload ${session.state}; select the file again to start a new session.`);
  }
  if (!task.file) {
    host.patch(id, { status: "failed", error: t("drive.uploadFileUnavailable"), speed: 0 });
    return { kind: "stop" };
  }
  if (task.file.size !== session.size) throw new Error("The selected file has a different size.");
  // A retried publication whose bytes already landed: the server recognized its own
  // object at the destination and admitted a session that only has to settle it.
  // Uploading again would be refused by the create condition, so the row goes straight
  // to commit — the same trailing steps every other row takes after its bytes are in.
  if (session.bytesPresent) return { kind: "settle", task, session };
  return { kind: "upload", task, session, file: task.file, allocated: admission.allocated };
}

/**
 * Step: transfer/parts — make every part of an open session durable.
 *
 * Contract
 * - Caller: an `open` session, the row's current File, and a size that matches the
 *   session. The row holds no transfer slot yet.
 * - Takes a transfer slot for this file (the caller's `finally` releases it on every path)
 *   and holds a part slot only while a part is actually in flight; the global pool orders
 *   files by their first-start rank.
 * - Returns `true` when every part is durable and the row may settle, `false` when the
 *   attempt is over (aborted, or an ascending session still has parts and the row went back
 *   to the queue as `queued`).
 * - Reads receipts before sending anything it cannot prove is missing; a lost ACK is
 *   resolved by a status + receipts read, never by a blind resend. A failure that survives
 *   the bounded retries (or a non-transient one) throws after reporting the bytes it has.
 * - Progress is published at most once per `PROGRESS_INTERVAL_MS`, and the speed sample
 *   covers `SPEED_WINDOW_MS`.
 * - Only a session this attempt allocated may trust `receivedParts`; any other session is
 *   listed first.
 */
async function moveUploadParts(
  host: UploadRunHost,
  id: string,
  session: FileUploadSession,
  file: File,
  allocated: boolean,
  signal: AbortSignal,
): Promise<boolean> {
  // The session is in hand, so this file can move bytes and takes a transfer slot now.
  // Taking it at claim time instead put slots on files that were still waiting for their
  // create page, and the part pool then had nothing to hand out.
  host.transferring.add(id);
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
  let verification = host.verified.get(session.id);
  if (!classA && (!verification || verification.file !== file)) {
    verification = { file, parts: new Map() };
    host.verified.set(session.id, verification);
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
  if (signal.aborted) return false;
  let received = [...known.values()].reduce((sum, size) => sum + size, 0);
  host.patch(id, { loaded: received, speed: 0 });
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
      host.patch(id, { loaded, speed: elapsed > 0 ? Math.max(0, loaded - samples[0].loaded) * 1000 / elapsed : 0 });
    };
    // One sample per second, independent of how often XHR reports bytes: the list
    // stays readable, and a stalled link visibly falls to 0 B/s instead of freezing
    // on its last value.
    const ticker = setInterval(flush, PROGRESS_INTERVAL_MS);
    host.deadlines.add(ticker);
    const cancelProgress = () => {
      clearInterval(ticker);
      host.deadlines.delete(ticker);
    };
    const retryDelay = (attempt: number) => new Promise<void>((resolve, reject) => {
      if (batch.signal.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
      const abort = () => { clearTimeout(timer); host.deadlines.delete(timer); reject(new DOMException("Upload canceled", "AbortError")); };
      const timer = setTimeout(() => { host.deadlines.delete(timer); batch.signal.removeEventListener("abort", abort); resolve(); },
        Math.min(PART_RETRY_MAX_DELAY_MS, PART_RETRY_DELAY_MS * 2 ** attempt));
      host.deadlines.add(timer);
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
      const rank = host.ranks.get(id) ?? Number.MAX_SAFE_INTEGER;
      // Slots this file holds right now, across its workers. A worker keeps its slot
      // between its own parts, so this count is what the row reads to decide whether
      // the file is moving.
      let held = 0;
      const holdSlot = () => { held++; host.patch(id, { activeParts: held }); };
      const yieldSlot = () => { held--; host.releasePart(); host.patch(id, { activeParts: held }); };
      await Promise.all(Array.from({ length: Math.min(parallelism, pending.length) }, async () => {
        let holding = false;
        try {
          for (let number = pending.shift(); number !== undefined && !batch.signal.aborted; number = pending.shift()) {
            // Keep a held slot unless a part queued by an earlier file, or this file's
            // own earlier part, is waiting for it.
            const head = host.partPool.waiters[0];
            if (holding && head && (head.rank < rank || head.rank === rank && head.number < number)) { holding = false; yieldSlot(); }
            if (!holding) {
              try { await host.acquirePart(rank, number, batch.signal); }
              catch (error) { if (failure === undefined && !signal.aborted) failure = error; return; }
              holding = true; holdSlot();
            }
            await uploadOne(number);
          }
        } finally { if (holding) yieldSlot(); }
      }));
    } finally { signal.removeEventListener("abort", abortBatch); cancelProgress(); }
    if (signal.aborted) return false;
    if (failure !== undefined) { host.patch(id, { loaded: received }); throw failure; }
    // Ascending sessions send one new part per turn; requeue for the next one.
    if (known.size !== session.partCount) { host.patch(id, { loaded: received, status: "queued", speed: 0 }); return false; }
  }
  return true;
}

/**
 * Step: settle — publish the uploaded bytes and adopt the answer.
 *
 * Contract
 * - Caller: every part is durable (or the server recognized them), and no settle has been
 *   sent for this attempt yet.
 * - Marks the row `finalizing`, gives the transfer slot back before waiting, then sends the
 *   idempotent commit (bounded retries, abandoned on abort). A file answer completes the
 *   row; an Operation answer is recorded for the poll to follow.
 * - Returns nothing; a commit failure that survives retries propagates to `abandonAttempt`.
 *   A page-level settlement failure reaches it as `QueuePaused` and re-queues the row.
 */
async function settleUploadSession(host: UploadRunHost, id: string, task: UploadTask, session: FileUploadSession, signal: AbortSignal): Promise<void> {
  host.patch(id, { status: "finalizing", uploaded: true, loaded: session.size, speed: 0 });
  // The bytes are placed. Give the slot back before waiting for the settlement: the
  // next admitted file starts streaming now, and this row's answer arrives on its own.
  host.releaseWindow(id);
  const result = await commitUpload(session.id, signal, host.settleUpload);
  if (signal.aborted) return;
  if (isOperation(result)) { host.patch(id, { operationId: result.id }); }
  else {
    // The commit answered with the published file: whatever the session still staged is now
    // the file at the destination, so its commit is terminal and no abort can apply.
    host.patch(id, { sessionTerminal: true });
    host.complete(task, result);
  }
}

/**
 * Step: retry/abandon — map a failed attempt to the row's next state.
 *
 * Contract
 * - Caller: the attempt threw and was not aborted. `error` may be anything a stage (or the
 *   server) produced.
 * - A `QueuePaused` page failure never fails a row: the row goes back to `queued` with its
 *   session, intent and create key intact, and the queue's one message explains the stop.
 * - A live `operation_in_progress` becomes a `waiting` row that names the blocker's kind;
 *   a name conflict becomes the replace/skip question (or a failure when the destination
 *   is not a file); everything else fails the row with the server's Problem text.
 * - Returns nothing; may read the destination once and may ask the conflict question.
 */
async function abandonAttempt(host: UploadRunHost, id: string, error: unknown): Promise<void> {
  if (isQueuePaused(error)) {
    // The page this file was carried by failed as a whole, so this row is not a
    // failure. It returns to the queue with its session, intent and create key intact —
    // a settle resumes by asking the server about that session again, and an admission
    // replays the same idempotent create — while the one message explains the stop.
    host.patch(id, { status: "queued", speed: 0 });
    return;
  }
  if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId) {
    const task = host.tasks.get(id);
    const waitingFor: FileOperationWait = { operationId: error.operationId, path: task?.path ?? "", state: "running", kind: waitKind(error.operationKind), from: "conflict" };
    host.patch(id, { status: "waiting", waitingFor, waitMessage: waitMessage(waitingFor), speed: 0 });
  } else if (error instanceof ApiError && error.status === 409 && error.code === "name_conflict" && !(host.tasks.get(id)?.overwrite)) {
    // A direct publication that lost the race for the destination is the same decision
    // the create endpoint reports: replace the file, or leave it. The session is over
    // either way — the provider refused the write — so a skip ends it rather than
    // leaving an open session nobody will ever commit.
    const current = host.tasks.get(id);
    const old = current ? await observedEntry(current.path) : null;
    if (current && !old) {
      // The name is free again: the publication lost to a file that is no longer there.
      host.patch(id, { uploadId: undefined, intent: undefined, createKey: taskId(), status: "queued", error: undefined, speed: 0 });
    } else if (current && old && old.kind === "file") {
      if (await host.askConflict(current.path) === "replace") {
        host.patch(id, { overwrite: true, observedRevision: old.contentRevision ?? undefined, intent: undefined, uploadId: undefined, createKey: taskId(), status: "queued", error: undefined, speed: 0 });
      } else { host.skip(id); }
    } else {
      host.patch(id, { status: "failed", error: message(error), speed: 0 });
    }
  } else { host.patch(id, { status: "failed", error: message(error), speed: 0 }); }
}


function createQueue(): QueueOwner {
  // The engine outlives every page, so its cells are plain closure state rather than hook
  // slots. The two shims below keep `useRef`/`useCallback` spellings at the call sites, which
  // is what lets the transfer code below stay byte-for-byte the implementation it was.
  const useRef = <T>(value: T): { current: T } => ({ current: value });
  const useCallback = <T>(callback: T, _dependencies?: unknown[]): T => callback;
  const subscribers = new Set<() => void>();
  const completionHandlers = new Set<(task: UploadTask, item: Item) => void>();
  const notify = () => { for (const listener of subscribers) listener(); };
  const subscribe = (listener: () => void) => {
    subscribers.add(listener);
    return () => { subscribers.delete(listener); };
  };
  const onCompleted = (handler: (task: UploadTask, item: Item) => void) => {
    completionHandlers.add(handler);
    return () => { completionHandlers.delete(handler); };
  };
  // The published snapshot is the queue's only rendered state: one array on a cadence, not
  // one hook slot per field, so the page re-renders exactly as often as it did when the
  // queue was page state.
  let published: UploadTask[] = [];
  let policy: ConflictPolicy = "ask";
  let conflict: string | null = null;
  let pauseMessage: string | null = null;
  const setPolicy = (next: ConflictPolicy) => { policy = next; notify(); };
  const setConflict = (next: string | null) => { conflict = next; notify(); };
  const setPauseMessage = (next: string | null) => { pauseMessage = next; notify(); };
  // Native tab-close guard. It belongs to the engine, not to a page, so it is attached while
  // bytes are outstanding and never removed by a navigation.
  let guardAttached = false;
  const beforeUnload = (event: BeforeUnloadEvent) => {
    event.preventDefault();
    event.returnValue = "";
  };
  const activeStatuses: UploadStatus[] = ["queued", "waiting", "uploading", "finalizing"];
  const syncBeforeUnload = () => {
    const active = published.some((task) => activeStatuses.includes(task.status));
    if (active === guardAttached) return;
    guardAttached = active;
    if (active) window.addEventListener("beforeunload", beforeUnload);
    else window.removeEventListener("beforeunload", beforeUnload);
  };
  const setPublished = (next: UploadTask[]) => {
    published = next;
    syncBeforeUnload();
    notify();
  };

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
  const admissions = useRef(new Map<string, { controller: AbortController; promise: Promise<FileUploadSession>; ready: boolean }>());
  // Files a look-ahead admission already refused. The window is the authority for a
  // refusal — it owns the row's error and its retry — so retrying the same refusal on
  // every completion would only turn one bad destination into an endless request loop.
  const admissionFailed = useRef(new Set<string>());
  // Directory tasks that have not completed. A task under one of them must wait, and
  // answering "is an ancestor still a pending directory" by scanning every task would
  // make each start O(tasks).
  const openDirs = useRef(new Set<string>());
  // The standing answer and the questions waiting behind it. The ref is what the queue
  // reads (a conflict is answered from inside an async attempt, never from a render), the
  // published field is only what the sheet shows.
  const policyRef = useRef<ConflictPolicy>("ask");
  const conflicts = useRef<{ path: string; resolve: (choice: "replace" | "skip") => void }[]>([]);
  const conflictOpen = useRef(false);
  const flushTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const owner = useRef("");
  const pathClaims = useRef(new Map<string, () => void>());
  const mounted = useRef(true);
  const ready = useRef(false);
  const running = useRef(new Map<string, Runtime>());
  const pumpRef = useRef<() => void>(() => {});
  const pumpQueued = useRef(false);
  const verified = useRef(new Map<string, { file: File; parts: Map<number, string> }>());
  const pollBusy = useRef(false);
  // Admission and settlement pages. These collapse the two control requests a task would
  // otherwise send alone; the look-ahead fills a page with the files that will start next,
  // which is why a prepare costs one request for a whole window rather than one per file.
  // A test double without the batch calls keeps the per-file path, so the queue's own
  // behaviour stays covered by the existing suite.
  const createBatch = useRef<BatchState<PendingCreate>>({ items: [] });
  const settleBatch = useRef<BatchState<PendingSettle>>({ items: [] });
  // A page-level failure stops the queue until someone resumes it. The ref is what the
  // flush loops and the pump read; the published field is the one message the page renders.
  const paused = useRef<{ message: string } | null>(null);
  // One session check at a time: the banner's Resume may be pressed while the previous
  // retry is still in flight.
  const resuming = useRef(false);

  // The rule this records: a failure is queue-level exactly when it is not one file's
  // answer — the page itself was rejected (transport failure, a non-2xx page status such as
  // 5xx/401/403/429, or an incomplete 200) rather than a successful page carrying an item's
  // Problem. The first kind is recorded here once; the second still reaches that item's row.
  const pauseQueue = useCallback((error: unknown) => {
    if (paused.current) return;
    paused.current = { message: message(error) };
    if (mounted.current) setPauseMessage(paused.current.message);
  }, []);

  // Every filled page leaves at once; a partial one leaves on the next turn of the loop,
  // which is what a look-ahead that fills a page synchronously produces anyway.
  //
  // Pages are deliberately not serialized. Sending one and waiting for it made the import's
  // admission rate one page per page latency — 32 files per 5.8 s, about six sessions a
  // second, with the other twenty-six waiting for that page's slowest item — and a page that
  // never returned stopped every later file behind it, so the sheet read 0 / 23,373 with the
  // transfer stage completely empty and nothing on screen to say why. The depth is already
  // bounded where it belongs: the look-ahead admits at most ADMIT_AHEAD sessions and
  // ADMIT_AHEAD_BYTES bytes, so the pages in flight cannot outrun the work that asked for
  // them. The settlement path has the same shape for the same reason.
  const flushCreates = useCallback(() => {
    const state = createBatch.current;
    // A paused queue keeps its items rather than sending them into the failure that just
    // stopped it; `resumeQueue` flushes them once the cause is gone.
    if (paused.current) return;
    while (state.items.length) {
      const batch = state.items.splice(0, CREATE_BATCH_SIZE);
      void (async () => {
        try {
          const results = await api.createUploadBatch(batch.map(({ input, key }) => ({ input, key })));
          batch.forEach((item, index) => {
            if (item.cancelled) return;
            const result = results[index];
            if (result?.upload) item.resolve(result.upload);
            else if (result?.error) item.reject(new FileApiError(result.error));
            else {
              // A page that answered 200 must describe every item it carried. One that did
              // not left this file with no answer, which is the page's failure to answer —
              // still not something the file's own destination did.
              const incomplete = new Error("The admission page was incomplete.");
              pauseQueue(incomplete);
              item.reject(new QueuePaused(message(incomplete)));
            }
          });
        } catch (error) {
          // The page never answered: pause once and hand every carried item the paused
          // error, so each attempt returns to the queue instead of failing its row.
          pauseQueue(error);
          batch.forEach((item) => { if (!item.cancelled) item.reject(new QueuePaused(message(error))); });
        } finally {
          if (state.items.length) flushCreates();
        }
      })();
    }
  }, [pauseQueue]);

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

  // A page leaves when it is full; a partial page leaves when its oldest item has waited
  // SETTLE_PAGE_WAIT_MS. Sending every item the moment it arrived — what this did — made every
  // page a page of one, so the page size never applied and each settlement cost a request of
  // its own: an import's settlement requests matched its file count one to one, and the page
  // latency climbed from 0.6 s to 10 s as that queue grew.
  //
  // Nothing here bounds how many pages are outstanding. The server's pool is the queue —
  // Operations, a thousand slots, its own retry and deferral — and a page that waits for that
  // pool is the server reporting saturation, not something a client constant should mask.
  const flushSettles = useCallback(() => {
    const state = settleBatch.current;
    if (paused.current) return;
    while (state.items.length) {
      const batch = state.items.splice(0, COMMIT_BATCH_SIZE);
      void (async () => {
        try {
          const results = await api.commitUploadBatch(batch.map(({ id }) => id));
          batch.forEach((item, index) => {
            if (item.cancelled) return;
            const result = results[index];
            if (result?.result) item.resolve(result.result);
            else if (result?.error) item.reject(new FileApiError(result.error));
            else {
              const incomplete = new Error("The settlement page was incomplete.");
              pauseQueue(incomplete);
              item.reject(new QueuePaused(message(incomplete)));
            }
          });
        } catch (error) {
          // A settlement page is the same page-level shape as an admission page: the
          // publication it was reporting is untouched, so pausing loses no bytes and a
          // resume asks the same session again.
          pauseQueue(error);
          batch.forEach((item) => { if (!item.cancelled) item.reject(new QueuePaused(message(error))); });
        } finally {
          if (state.items.length) flushSettles();
        }
      })();
    }
  }, [pauseQueue]);

  const settleUpload = useCallback((id: string, signal?: AbortSignal) => new Promise<FileUploadCommit | FileOperation>((resolve, reject) => {
    if (typeof api.commitUploadBatch !== "function") { api.commitUpload(id, signal).then(resolve, reject); return; }
    if (signal?.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
    const state = settleBatch.current;
    const item: PendingSettle = { id, resolve, reject, cancelled: false };
    signal?.addEventListener("abort", () => { item.cancelled = true; reject(new DOMException("Upload canceled", "AbortError")); }, { once: true });
    state.items.push(item);
    // A full page goes now; a partial one waits for company, which the timer bounds.
    if (state.items.length >= COMMIT_BATCH_SIZE) {
      if (state.timer !== undefined) { clearTimeout(state.timer); state.timer = undefined; }
      flushSettles();
    } else if (state.timer === undefined) {
      state.timer = setTimeout(() => { state.timer = undefined; flushSettles(); }, SETTLE_PAGE_WAIT_MS);
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
  // Decide once per turn, on the state the bulk change left behind.
  //
  // A pump is not a notification: it works from the pending set, so it acts on whatever is
  // queued at the moment it runs. A gesture that changes thousands of tasks in one loop —
  // cancelling a selected tree — would otherwise run it once per task, and each pass would
  // act on the part of the selection the loop had not reached yet: claim the next file's
  // path through Web Locks and admit a session for it, only for the same loop to abort both
  // a moment later. That is one claim per cancelled file, all discarded, competing with the
  // aborts that are actually finishing the gesture. One pass after the loop sees the final
  // state and does nothing. Gestures keep their own synchronous pump (enqueue, retry,
  // cancel still call `pump` directly); everything that merely finished something asks for
  // the next turn.
  const requestPump = useCallback(() => {
    if (pumpQueued.current) return;
    pumpQueued.current = true;
    queueMicrotask(() => { pumpQueued.current = false; pumpRef.current(); });
  }, []);
  /**
   * Resolve the authenticated owner, the gate every queued byte waits behind.
   *
   * A failed read (or a null answer) used to be swallowed, which left every row at "queued"
   * forever with no action that could retry it. It is thrown instead: the caller turns it
   * into the queue-level pause, and Resume asks again after the session is restored.
   */
  const resolveOwner = async (): Promise<boolean> => {
    if (ready.current) return true;
    const user = await api.me();
    if (!mounted.current) return false;
    if (!user) throw new Error(t("drive.uploadQueueSignedOut"));
    owner.current = user.id;
    setUploadOwner(user.id);
    try { localStorage.removeItem(`eam.uploads.v1:${user.id}`); }
    catch { /* Obsolete records are never read, even when storage is unavailable. */ }
    ready.current = true;
    return true;
  };
  /**
   * The queue's retry: send the page a queue-level failure stopped, once the cause is gone.
   *
   * A page failure names no file, so no row's `retry` can answer it. The rows it carried
   * were never failed — they went back to "queued" — and pages queued behind the pause were
   * kept rather than sent, so resuming is only clearing the stop and flushing them again.
   * The one stop whose cause is checked elsewhere is the mount-time session read: there
   * resuming first retries that read, because no page can be sent without an owner.
   */
  const resumeQueue = useCallback(() => {
    if (!paused.current) return;
    // The mount-time session check is part of the queue's start, not a file's attempt: until
    // it succeeds there is no owner, so resuming means retrying that check first. Only its
    // success clears the stop; a still-broken session leaves the same banner up with the
    // newest reason instead of letting the pump run into another 401.
    if (!ready.current) {
      if (resuming.current) return;
      resuming.current = true;
      void resolveOwner().then((resolved) => {
        resuming.current = false;
        if (resolved && mounted.current) resumeQueue();
      }, (error) => {
        resuming.current = false;
        if (!mounted.current || !paused.current) return;
        paused.current = { message: message(error) };
        setPauseMessage(paused.current.message);
      });
      return;
    }
    paused.current = null;
    if (mounted.current) setPauseMessage(null);
    flushCreates();
    flushSettles();
    requestPump();
  }, [flushCreates, flushSettles, requestPump]);
  /**
   * Ask about one existing destination, or answer from the standing decision.
   *
   * One question at a time: the pump stops starting new files while it is on screen, so a
   * person answers the file in front of them instead of a stack of identical dialogs, and
   * the questions already queued behind it are settled by an "all" answer. Each waiter is
   * one file, not one gesture: answering "replace" for the file that was asked about must
   * not decide the next one, which is the difference between the two answers.
   */
  const askConflict = useCallback((path: string) => new Promise<"replace" | "skip">((resolve) => {
    if (policyRef.current !== "ask") { resolve(policyRef.current); return; }
    conflicts.current.push({ path, resolve });
    if (conflicts.current.length === 1) {
      conflictOpen.current = true;
      setConflict(path);
    }
  }), []);
  const answerConflict = useCallback((choice: ConflictChoice) => {
    const decision = choice === "replace" || choice === "replace-all" ? "replace" : "skip";
    const standing = choice === "replace-all" || choice === "skip-all";
    if (standing) {
      policyRef.current = decision;
      setPolicy(decision);
    }
    conflicts.current.shift()?.resolve(decision);
    if (standing) for (const waiter of conflicts.current.splice(0)) waiter.resolve(decision);
    conflictOpen.current = conflicts.current.length > 0;
    setConflict(conflicts.current[0]?.path ?? null);
    requestPump();
  }, [requestPump]);
  const askForConflicts = useCallback(() => {
    policyRef.current = "ask";
    setPolicy("ask");
  }, []);
  const beginImport = useCallback(() => {
    // A question already on screen belongs to the import that raised it; a new gesture
    // must not answer it from underneath the person reading it.
    if (conflictOpen.current) return;
    policyRef.current = "ask";
    setPolicy("ask");
  }, []);
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
  /**
   * Drop the rows the server has already finished with, and answer the ones it has not.
   *
   * The `discard` contract is on `UploadQueue`; this is its implementation. It reuses
   * `remove`, so every cell that tracks a row — pending starts, transfer slots, the pending
   * directory index, admissions, path claims — leaves with it in one place, and it publishes
   * at once because a deletion gesture must be visible on this paint rather than the next.
   */
  const discard = useCallback((selected: string | string[]): string[] => {
    const ids = typeof selected === "string" ? [selected] : selected;
    const finished: string[] = [];
    const remaining: string[] = [];
    for (const id of ids) {
      const task = tasks.current.get(id);
      if (!task) continue;
      if (isFinishedUpload(task)) finished.push(id);
      else remaining.push(id);
    }
    if (finished.length) {
      remove(finished);
      publish();
    }
    return remaining;
  }, [publish, remove]);
  /**
   * Leave an existing destination alone.
   *
   * A skip is the person's answer, not a failure, so the row leaves the sheet the way a
   * cancelled one does instead of ending the import with a wall of red for files that are
   * already exactly where they should be. A session this file had already opened goes with
   * it: an open session holds a reservation against the owner's quota until it expires,
   * and nothing will ever use this one.
   */
  const skip = useCallback((id: string) => {
    const task = tasks.current.get(id);
    if (task?.uploadId) void api.abortUpload(task.uploadId).catch(() => {});
    remove([id]);
  }, [remove]);
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
    // A page that is not mounted has no handler: its listing is stale anyway when it
    // returns, and the row above already records that the file landed.
    for (const handler of completionHandlers) handler(task, item);
  }, [patch, releaseClaim, remove]);
  const applyOperation = useCallback((task: UploadTask, op: FileOperation) => {
    if (op.id !== task.operationId) throw new Error("The operation identity changed.");
    if (op.state === "succeeded") {
      if (!op.result || !("kind" in op.result)) throw new Error("The operation result is incomplete.");
      // A succeeded publication is this session's terminal outcome: the file it staged is
      // now at the destination, so an abort would be a request the server can only refuse.
      if (!task.isDir) patch(task.id, { sessionTerminal: true });
      complete(task, op.result);
    } else if (op.state === "failed" || op.state === "cancelled") {
      if (task.cancelRequested) {
        if (task.uploadId) verified.current.delete(task.uploadId);
        remove([task.id]);
      }
      else if (task.retryRequested) {
        patch(task.id, { status: "queued", uploadId: undefined, operationId: undefined, intent: undefined, observedRevision: undefined, createKey: taskId(), terminalState: undefined, retryRequested: false, uploaded: false, loaded: 0 });
      } else patch(task.id, { status: "failed", speed: 0, terminalState: task.isDir ? undefined : op.state === "failed" ? "failed" : "aborted", error: op.error ? problemText(op.error) : t("drive.uploadFailed") });
    } else { patch(task.id, { status: "finalizing", speed: 0 }); }
  }, [complete, patch, remove]);

  // A file stops holding a transfer slot the moment its bytes are placed. The settlement
  // has its own page and its own bound, so waiting for it here would only make the next
  // file's first byte wait for this row's answer.
  const releaseWindow = useCallback((id: string) => {
    if (!transferring.current.delete(id)) return;
    requestPump();
  }, [requestPump]);

  /**
   * The cells and operations `run`'s steps may use. Built once with the engine, so the
   * maps and sets below are the engine's own cells, shared by reference and never copied;
   * the two getters read the cells that are reassigned during the tab's lifetime.
   */
  const runHost: UploadRunHost = {
    tasks: tasks.current,
    admissions: admissions.current,
    admissionFailed: admissionFailed.current,
    pathClaims: pathClaims.current,
    verified: verified.current,
    deadlines: deadlines.current,
    partPool: partPool.current,
    ranks: ranks.current,
    transferring: transferring.current,
    owner: () => owner.current,
    conflictPolicy: () => policyRef.current,
    patch,
    complete,
    skip,
    applyOperation,
    admitUpload,
    askConflict,
    settleUpload,
    releaseWindow,
    acquirePart,
    releasePart,
    requestPump,
  };
  // One admission per file, whoever asks first: the look-ahead that prepares a page and
  // the window that starts the file await the same request, so a file can never be admitted
  // twice — which for one path would end the other session. The mechanics live with the
  // admission steps; this is only the engine-side name the pump calls.
  const ensureAdmission = useCallback((task: UploadTask) => ensureUploadAdmission(runHost, task), [runHost]);

  /**
   * One attempt at one row: the steps below, in the order the server's fences require.
   *
   * This function is the sequence and the attempt's lifecycle, nothing else. Each stage owns
   * its decisions, its requests and its contract comment; the `finally` is the only thing
   * every outcome shares, because a transfer slot and a running marker belong to the
   * attempt rather than to any one stage.
   */
  const run = useCallback(async (id: string, runtime: Runtime) => {
    const signal = runtime.controller.signal;
    try {
      // The snapshot this attempt starts from. A stage that changes the row is followed by
      // a fresh read, so no stage acts on a copy an earlier stage already superseded.
      const task = tasks.current.get(id);
      if (!task) return;
      if (waitOnRecordedDeletion(runHost, id, task)) return;
      if (await adoptRecordedOperation(runHost, task, signal)) return;
      if (task.isDir) {
        await createDirectoryTask(runHost, id, task, signal);
        return;
      }
      const admission = await admitUploadAttempt(runHost, id, task, signal);
      if (!admission) return;
      runHost.patch(id, { uploadId: admission.session.id, allocationRejected: false });
      if (signal.aborted) return;
      const started = tasks.current.get(id);
      if (!started) return;
      const plan = takeUploadSession(runHost, id, started, admission);
      if (plan.kind === "stop") return;
      if (plan.kind === "upload") {
        const moved = await moveUploadParts(runHost, id, plan.session, plan.file, plan.allocated, signal);
        if (!moved) return;
      }
      await settleUploadSession(runHost, id, plan.task, plan.session, signal);
    } catch (error) {
      if (signal.aborted) return;
      await abandonAttempt(runHost, id, error);
    } finally {
      running.current.delete(id);
      transferring.current.delete(id);
      requestPump();
    }
  }, [runHost, requestPump]);
  const pump = useCallback(() => {
    if (!ready.current || !mounted.current) return;
    // A stopped queue starts nothing. Rows already moving may finish their bytes, but a page
    // that failed for a whole batch is not something a new file should walk into, and the
    // parked rows must stay queued until someone resumes.
    if (paused.current) return;
    // A question on screen is the queue's turn to wait. Files already moving keep moving —
    // the question is about a destination, not about stopping bytes — but nothing new is
    // started, so the person answers the conflict they can see instead of one from a stack
    // of dialogs they did not ask for.
    if (conflictOpen.current) return;
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
      if (running.current.size + starts.length >= CLAIM_WINDOW) break;
      const task = tasks.current.get(id);
      if (!task || task.status !== "queued" || task.cancelRequested) {
        pending.current.delete(id);
        continue;
      }
      // A row whose own attempt is still unwinding is skipped, not forgotten: answering a
      // conflict resolves the attempt, and the pump that answer requests may run one
      // microtask before that attempt reaches its `finally`. Dropping the id here would
      // then strand a queued row nothing would ever start; keeping it costs one no-op pass
      // because the attempt's own `finally` asks for the next turn.
      if (running.current.has(id)) continue;
      if (blockedBy(task.path)) continue;
      starts.push(task);
    }
    if (!starts.length) return;
    for (const task of starts) {
      pending.current.delete(task.id);
      // Pool priority follows the order files first start, kept across retries/turns.
      if (!ranks.current.has(task.id)) ranks.current.set(task.id, nextRank.current++);
      running.current.set(task.id, { controller: new AbortController() });
      // No transfer slot yet: a claim says who owns the row, not that the file can move.
      patch(task.id, { status: "uploading", error: undefined });
    }
    for (const task of starts) void run(task.id, running.current.get(task.id)!);
  }, [ensureAdmission, patch, run]);
  pumpRef.current = pump;
  const cancel = useCallback((selected: string | string[]) => {
    const ids = typeof selected === "string" ? [selected] : selected;
    // One pass for the selection: a folder row may name thousands of descendants, and
    // resolving each one on its own would walk the queue once per file.
    const wanted = new Set(ids);
    const subtrees: string[] = [];
    for (const id of ids) {
      const root = tasks.current.get(id);
      if (root?.isDir) subtrees.push(`${root.path}/`);
    }
    const targets: UploadTask[] = [];
    for (const task of tasks.current.values()) {
      if (wanted.has(task.id) || subtrees.some((prefix) => task.path.startsWith(prefix))) targets.push(task);
    }
    // Counted before anything is torn down: `uploadRows` stops showing a requested
    // cancellation at once, and a row that is gone cannot say what it covered. An empty
    // gesture answers null so the page reports nothing it did not do.
    const summary: UploadCancelSummary = {
      items: targets.length,
      completedItems: targets.filter((task) => task.status === "completed").length,
    };
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
        finally { requestPump(); }
      })();
    }
    // Cancelling is a gesture: the rows must answer now, not on the next publish tick.
    publish();
    return targets.length === 0 ? null : summary;
  }, [applyOperation, complete, dropAdmission, patch, publish, remove, requestPump]);
  const retry = useCallback((id: string) => {
    const task = tasks.current.get(id);
    if (task?.cancelRequested) { cancel(id); return; }
    // A retry that starts a new session must not resume the old one, and a retry that keeps
    // its key must still get a fresh look (the prepare may have been refused before).
    if (task?.terminalState) dropAdmission(id);
    patch(id, { status: "queued", error: undefined, speed: 0, retryRequested: true,
      ...(task?.terminalState ? { uploadId: undefined, operationId: undefined, intent: undefined, observedRevision: undefined, createKey: taskId(), terminalState: undefined, uploaded: false, loaded: 0 } : {}) });
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
  const enqueue = useCallback((entries: UploadEntry[], dir: string) => {
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
            ...(old.terminalState ? { uploadId: undefined, operationId: undefined, intent: undefined, observedRevision: undefined, createKey: taskId(), terminalState: undefined, uploaded: false, loaded: 0 } : {}) });
        }
        continue;
      }
      // Whether this destination already exists is the server's answer to give, and giving
      // it here would be wrong twice: the listing knows this folder's own children and
      // nothing deeper (a re-imported tree is all nested files), and what it says now may be
      // stale by the time the file's turn comes. A standing "replace" answer is not a
      // property of this row at all — it is the import's, so admission applies it when the
      // row starts rather than freezing it at selection time.
      const id = taskId();
      const task: UploadTask = { id, file: entry.file, isDir: !entry.file, name: entry.path, dir: relative(dir), path, size: entry.file?.size ?? 0, modifiedAt: entry.file?.lastModified,
        overwrite: false, parents: !!entry.file && path.includes("/"), status: "queued", loaded: 0, speed: 0, activeParts: 0, uploaded: false, createKey: id };
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
    // The new ids let a caller that feeds the queue from a live source — a dropped tree being
    // read page by page — follow what it has handed over, so cancelling that import can stop
    // the source instead of cancelling the page it had reached.
    return added.map((task) => task.id);
  }, [dropAdmission, insert, patch, publish]);
  // The tab's lifetime work starts with the first page that uses the queue and survives that
  // page. `start` is idempotent, so every remount is a no-op here; `dispose` is the one real
  // end (a closed tab), which nothing in the SPA calls.
  let started = false;
  let pollController: AbortController | null = null;
  let pollTimer: ReturnType<typeof setInterval> | null = null;
  const start = () => {
    if (started) return;
    started = true;
    mounted.current = true;
    const controller = new AbortController();
    pollController = controller;
    void resolveOwner().then((resolved) => {
      if (resolved) pumpRef.current();
    }, (error) => {
      // A mount-time session check that fails is the queue's condition, not each file's: the
      // rows stay queued and the banner's Resume retries the check before sending a byte.
      if (mounted.current) pauseQueue(error);
    });
    pollTimer = setInterval(() => {
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
        } finally { pollBusy.current = false; requestPump(); }
      })();
    }, 2000);
  };
  const dispose = () => {
    if (!started) return;
    started = false;
    mounted.current = false;
    ready.current = false;
    pollController?.abort();
    if (pollTimer !== null) clearInterval(pollTimer);
    if (flushTimer.current !== undefined) { clearTimeout(flushTimer.current); deadlines.current.delete(flushTimer.current); flushTimer.current = undefined; }
    // Progress tickers are intervals, so they need clearInterval even though they
    // share the deadline set with timeouts.
    for (const deadline of deadlines.current) { clearTimeout(deadline); clearInterval(deadline); }
    deadlines.current.clear();
    for (const runtime of running.current.values()) runtime.controller.abort();
    running.current.clear();
    transferring.current.clear();
    // Prepared sessions have no row to outlive a closed tab: they are aborted here and the
    // idle window expires them. A navigation never reaches this, which is what keeps a
    // selected session available to the page that comes back.
    for (const admission of admissions.current.values()) admission.controller.abort();
    admissions.current.clear();
    admissionFailed.current.clear();
    for (const release of pathClaims.current.values()) release();
    pathClaims.current.clear();
    // A question nobody is left to answer must not leave its file awaiting a promise that
    // can no longer settle: closing the tab answers every one of them with the answer
    // that writes nothing.
    for (const waiter of conflicts.current.splice(0)) waiter.resolve("skip");
    conflictOpen.current = false;
    if (guardAttached) { window.removeEventListener("beforeunload", beforeUnload); guardAttached = false; }
  };
  const snapshot = (): UploadQueue => ({
    tasks: published,
    hasActiveUploads: published.some((task) => activeStatuses.includes(task.status)),
    enqueue, cancel, discard, retry, resume, resumeQueue,
    pauseMessage,
    conflict,
    conflictPolicy: policy,
    answerConflict,
    askForConflicts,
    beginImport,
  });
  return {
    get tasks() { return published; },
    get hasActiveUploads() { return published.some((task) => activeStatuses.includes(task.status)); },
    get pauseMessage() { return pauseMessage; },
    get conflict() { return conflict; },
    get conflictPolicy() { return policy; },
    enqueue, cancel, discard, retry, resume, resumeQueue, answerConflict, askForConflicts, beginImport,
    start, dispose, subscribe, onCompleted, snapshot,
  };
}

let queueOwner: QueueOwner | null = null;
function uploadQueueOwner(): QueueOwner {
  queueOwner ??= createQueue();
  return queueOwner;
}
/**
 * End the queue the way a closed tab does. Nothing in the SPA calls this: the whole point of
 * owning the queue at module level is that a page navigation does not end it. The test
 * harness calls it to model the one teardown that still aborts in-flight work.
 */
export function disposeUploadQueue(): void {
  queueOwner?.dispose();
}
/**
 * Subscribe the calling page to the tab-lifetime queue.
 *
 * The engine is created by the first page that uses it and outlives every page after that,
 * so an unmount only removes this subscription and its settle handler. A failed or absent
 * owner is not a render concern: it pauses the queue and the banner offers Resume.
 */
export function useUploadQueue(onUploaded: (task: UploadTask, item: Item) => void): UploadQueue {
  const queue = uploadQueueOwner();
  const [, bump] = useState(0);
  useEffect(() => {
    const unsubscribe = queue.subscribe(() => bump((value) => value + 1));
    const stop = queue.onCompleted(onUploaded);
    queue.start();
    return () => { unsubscribe(); stop(); };
  }, [queue, onUploaded]);
  return queue.snapshot();
}
