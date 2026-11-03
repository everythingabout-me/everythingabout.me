/** Upload tasks and File references live only in the current page. */
import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, api, type Item, type FileOperation, type FileUploadInput, type FileUploadSession } from "../api/client";
import { t } from "../i18n";
import type { UploadEntry } from "./uploadSelection";
import { claimUploadPath } from "./uploadOwnership";
import { pendingFileOperation, setUploadOwner, settleFileOperation } from "./uploadPersistence";

const MAX_PARALLEL_UPLOADS = 4;
const MAX_PARALLEL_PARTS = 4;
const PART_UPLOAD_ATTEMPTS = 3;
const PART_RETRY_DELAY_MS = 250;
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
  status: UploadStatus;
  loaded: number;
  speed: number;
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
const relative = (path: string) => path.replace(/^\/+|\/+$/g, "");
let sequence = 0;
function taskId(): string { return `upload-${Date.now().toString(36)}-${(++sequence).toString(36)}-${Math.random().toString(36).slice(2)}`; }
function message(error: unknown): string { return error instanceof Error ? error.message : t("error.unknown"); }
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
export function useUploadQueue(onUploaded: (task: UploadTask, item: Item) => void): UploadQueue {
  const [tasks, setTasks] = useState<UploadTask[]>([]);
  const tasksRef = useRef<UploadTask[]>([]);
  const owner = useRef("");
  const pathClaims = useRef(new Map<string, () => void>());
  const mounted = useRef(true);
  const ready = useRef(false);
  const running = useRef(new Map<string, Runtime>());
  const pumpRef = useRef<() => void>(() => {});
  const finishRef = useRef(onUploaded);
  const cursor = useRef("");
  const verified = useRef(new Map<string, { file: File; parts: Map<number, string> }>());
  const pollBusy = useRef(false);
  const deadlines = useRef(new Set<ReturnType<typeof setTimeout>>());
  finishRef.current = onUploaded;
  const update = useCallback((mutate: (current: UploadTask[]) => UploadTask[]) => {
    if (!mounted.current) return;
    const next = mutate(tasksRef.current);
    tasksRef.current = next;
    for (const [id, release] of pathClaims.current) {
      if (!next.some((task) => task.id === id && task.status !== "completed")) {
        release(); pathClaims.current.delete(id);
      }
    }
    setTasks(next);
  }, []);
  const patch = useCallback((id: string, changes: Partial<UploadTask>) => {
    update((current) => current.map((task) => task.id === id ? { ...task, ...changes } : task));
  }, [update]);
  const complete = useCallback((task: UploadTask, item: Item) => {
    if (!mounted.current || !tasksRef.current.some((entry) => entry.id === task.id)) return;
    if (task.uploadId) verified.current.delete(task.uploadId);
    if (task.cancelRequested) {
      // Cancellation after publication cannot undo a committed native file.
      update((current) => current.filter((entry) => entry.id !== task.id));
    } else { patch(task.id, { status: "completed", loaded: task.size, file: null, speed: 0, error: undefined }); }
    finishRef.current(task, item);
  }, [patch, update]);
  const applyOperation = useCallback((task: UploadTask, op: FileOperation) => {
    if (op.id !== task.operationId) throw new Error("The operation identity changed.");
    if (op.state === "succeeded") {
      if (!op.result || !("kind" in op.result)) throw new Error("The operation result is incomplete.");
      complete(task, op.result);
    } else if (op.state === "failed" || op.state === "cancelled") {
      if (task.cancelRequested) {
        if (task.uploadId) verified.current.delete(task.uploadId);
        update((current) => current.filter((entry) => entry.id !== task.id));
      }
      else if (task.retryRequested) {
        patch(task.id, { status: "queued", uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, retryRequested: false, uploaded: false, loaded: 0 });
      } else patch(task.id, { status: "failed", speed: 0, terminalState: task.isDir ? undefined : op.state === "failed" ? "failed" : "aborted", error: op.error?.detail ?? t("drive.uploadFailed") });
    } else { patch(task.id, { status: "finalizing", speed: 0 }); }
  }, [complete, patch, update]);

  const run = useCallback(async (id: string, runtime: Runtime) => {
    const signal = runtime.controller.signal;
    try {
      let task = tasksRef.current.find((entry) => entry.id === id);
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
          const capabilities = await api.capabilities(parent);
          if (signal.aborted) return;
          intent = { path: task.path, size: task.size, mediaType: task.file?.type || "application/octet-stream", condition,
            ...(capabilities.sourceModifiedAt && task.modifiedAt !== undefined ? { sourceModifiedAt: new Date(task.modifiedAt).toISOString() } : {}) };
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
      task = tasksRef.current.find((entry) => entry.id === id)!;
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
        || session.partCount !== Math.ceil(session.size / session.partSize) || !Number.isSafeInteger(session.maxParallelism)
        || session.maxParallelism < 1 || !["any", "ascending"].includes(session.ordering)
        || session.transport !== undefined && !["embedded", "cos-signed"].includes(session.transport)) throw new Error("Invalid upload session geometry.");
      const parallelism = session.ordering === "ascending" ? 1 : Math.min(MAX_PARALLEL_PARTS, session.maxParallelism);
      let verification = verified.current.get(session.id);
      if (!verification || verification.file !== file) {
        verification = { file, parts: new Map() };
        verified.current.set(session.id, verification);
      }
      const chunkFor = (number: number) => {
        const start = (number - 1) * session.partSize;
        return file.slice(start, Math.min(start + session.partSize, session.size));
      };
      const verifyReceipt = async (receipt: { number: number; size: number; sha256: string }) => {
        if (!Number.isSafeInteger(receipt.number) || receipt.number < 1 || receipt.number > session.partCount) throw new Error("Invalid part receipt.");
        const chunk = chunkFor(receipt.number);
        if (receipt.size !== chunk.size || (verification.parts.get(receipt.number) !== receipt.sha256 && await sha256(chunk) !== receipt.sha256)) throw new Error(t("drive.reselectedFileChanged"));
        verification.parts.set(receipt.number, receipt.sha256);
      };
      const readReceipts = async (requestSignal: AbortSignal) => {
        const result = new Map<number, { number: number; size: number; sha256: string }>();
        let next: string | undefined;
        let watermark: number | undefined;
        do {
          const page = await api.uploadParts(session.id, next, requestSignal);
          if (watermark !== undefined && page.watermark !== watermark) throw new Error("The receipt watermark changed.");
          watermark = page.watermark;
          for (const receipt of page.items) {
            if (result.has(receipt.number)) throw new Error("Invalid part receipt.");
            await verifyReceipt(receipt);
            result.set(receipt.number, receipt);
          }
          next = page.nextCursor ?? undefined;
        } while (next);
        return result;
      };
      const receipts = await readReceipts(signal);
      if (signal.aborted) return;
      let received = [...receipts.values()].reduce((sum, receipt) => sum + receipt.size, 0);
      patch(id, { loaded: received });
      const missing: number[] = [];
      // A sliding window of `parallelism` workers drains the missing parts so one slow
      // part never idles the other connections. The turn only ends early (after one
      // window's worth of parts) when another file is queued and needs a slot.
      for (let number = 1; number <= session.partCount && (session.ordering !== "ascending" || missing.length < 1); number++) {
        if (!receipts.has(number)) missing.push(number);
      }
      if (session.ordering === "ascending" && missing.length && session.nextPart !== missing[0]) throw new Error("The next upload part changed.");
      if (missing.length) {
        const batch = new AbortController();
        const abortBatch = () => batch.abort();
        signal.addEventListener("abort", abortBatch, { once: true });
        const inFlight = new Map<number, number>();
        const began = performance.now();
        const baseline = received;
        const progress = () => {
          if (signal.aborted || batch.signal.aborted) return;
          const loaded = Math.min(session.size, received + [...inFlight.values()].reduce((sum, bytes) => sum + bytes, 0));
          patch(id, { loaded, speed: Math.max(0, loaded - baseline) * 1000 / Math.max(300, performance.now() - began) });
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
              for (let attempt = 0; attempt < PART_UPLOAD_ATTEMPTS; attempt++) {
                if (batch.signal.aborted) throw new DOMException("Upload canceled", "AbortError");
                let receipt;
                try {
                  receipt = await api.uploadPart(session.id, number, chunk, batch.signal, (sample) => {
                    if (batch.signal.aborted || receipts.has(number)) return;
                    inFlight.set(number, Math.max(inFlight.get(number) ?? 0, Math.max(0, Math.min(chunk.size, sample.loaded))));
                    progress();
                  }, session.transport);
                } catch (error) {
                  if (batch.signal.aborted) throw error;
                  if (!(error instanceof ApiError) || !([0, 408, 425, 429, 500, 502, 503, 504].includes(error.status)
                    || session.transport === "cos-signed" && error.status === 409 && error.code === "part_pending")) throw error;
                  // A lost ACK may already represent durable success. Inspect the
                  // original session and exact byte identity before any replay.
                  const status = await api.uploadStatus(session.id, batch.signal);
                  if (status.state !== "open") throw error;
                  receipt = (await readReceipts(batch.signal)).get(number);
                  if (!receipt) {
                    if (attempt + 1 === PART_UPLOAD_ATTEMPTS) throw error;
                    inFlight.delete(number); progress();
                    await retryDelay(attempt);
                    continue;
                  }
                }
                if (receipt.number !== number) throw new Error("The part receipt identity changed.");
                await verifyReceipt(receipt);
                if (batch.signal.aborted) return;
                receipts.set(number, receipt);
                received += receipt.size;
                inFlight.delete(number);
                progress();
                return;
              }
            } catch (error) {
              if (failure === undefined) failure = error;
              batch.abort();
            }
          };
          let started = 0;
          const othersWaiting = () => tasksRef.current.some((task) => task.id !== id && task.status === "queued" && !task.cancelRequested);
          const take = () => started >= parallelism && othersWaiting() ? undefined : pending.shift();
          await Promise.all(Array.from({ length: Math.min(parallelism, pending.length) }, async () => {
            for (let number = take(); number !== undefined && !batch.signal.aborted; number = take()) {
              started++;
              await uploadOne(number);
            }
          }));
        } finally { signal.removeEventListener("abort", abortBatch); }
        if (signal.aborted) return;
        if (failure !== undefined) { patch(id, { loaded: received }); throw failure; }
        patch(id, { loaded: received, status: "queued", speed: 0 });
        // Yield after one bounded part batch so later files get a turn too.
        if (receipts.size !== session.partCount) return;
      }
      patch(id, { status: "finalizing", uploaded: true, loaded: session.size, speed: 0 });
      const result = await api.commitUpload(session.id, signal);
      if (signal.aborted) return;
      if (isOperation(result)) { patch(id, { operationId: result.id }); }
      else { complete(task, result); }
    } catch (error) {
      if (runtime.controller.signal.aborted) return;
      if (error instanceof ApiError && error.code === "operation_in_progress" && error.operationId) {
        const task = tasksRef.current.find((entry) => entry.id === id);
        const waitingFor: DeletionWait = { operationId: error.operationId, path: task?.path ?? "", state: "running" };
        patch(id, { status: "waiting", waitingFor, waitMessage: waitMessage(waitingFor), speed: 0 });
      } else { patch(id, { status: "failed", error: message(error), speed: 0 }); }
    } finally {
      running.current.delete(id);
      pumpRef.current();
    }
  }, [applyOperation, complete, patch]);
  const pump = useCallback(() => {
    if (!ready.current || !mounted.current) return;
    while (running.current.size < MAX_PARALLEL_UPLOADS) {
      const eligible = tasksRef.current.filter((task) => task.status === "queued" && !task.cancelRequested && !running.current.has(task.id)
        && !tasksRef.current.some((parent) => parent.isDir && parent.status !== "completed" && task.path.startsWith(`${parent.path}/`)));
      if (!eligible.length) break;
      const ordered = tasksRef.current;
      const at = ordered.findIndex((task) => task.id === cursor.current);
      const eligibleIds = new Set(eligible.map((task) => task.id));
      const next = [...ordered.slice(at + 1), ...ordered.slice(0, at + 1)].find((task) => eligibleIds.has(task.id))!;
      cursor.current = next.id;
      const runtime = { controller: new AbortController() };
      running.current.set(next.id, runtime);
      patch(next.id, { status: "uploading", error: undefined });
      void run(next.id, runtime);
    }
  }, [patch, run]);
  pumpRef.current = pump;
  const cancel = useCallback((id: string) => {
    const root = tasksRef.current.find((task) => task.id === id);
    const targets = tasksRef.current.filter((task) => task.id === id || root?.isDir && task.path.startsWith(`${root.path}/`));
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
                update((current) => current.filter((entry) => entry.id !== task.id)); return;
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
            update((current) => current.filter((entry) => entry.id !== task.id));
          }
        } catch (error) { patch(task.id, { status: "failed", error: message(error) }); }
        finally { pumpRef.current(); }
      })();
    }
  }, [applyOperation, complete, patch, update]);
  const retry = useCallback((id: string) => {
    const task = tasksRef.current.find((entry) => entry.id === id);
    if (task?.cancelRequested) { cancel(id); return; }
    patch(id, { status: "queued", error: undefined, speed: 0, retryRequested: true,
      ...(task?.terminalState ? { uploadId: undefined, operationId: undefined, intent: undefined, createKey: taskId(), terminalState: undefined, uploaded: false, loaded: 0 } : {}) });
    pumpRef.current();
  }, [cancel, patch]);
  const enqueue = useCallback((entries: UploadEntry[], dir: string, exists: (path: string) => boolean) => {
    for (const entry of entries) {
      const path = relative(dir ? `${dir}/${entry.path}` : entry.path);
      const old = tasksRef.current.find((task) => task.path === path && task.status !== "completed");
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
      update((current) => [...current, { id, file: entry.file, isDir: !entry.file, name: entry.path, dir: relative(dir), path, size: entry.file?.size ?? 0, modifiedAt: entry.file?.lastModified,
        overwrite, status: "queued", loaded: 0, speed: 0, uploaded: false, createKey: id }]);
    }
    pumpRef.current();
  }, [patch, update]);
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
      const candidates = tasksRef.current.filter((task) => task.waitingFor || task.operationId && task.status === "finalizing");
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
            if (!mounted.current || !tasksRef.current.some((entry) => entry.id === task.id)) continue;
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
      for (const deadline of deadlines.current) clearTimeout(deadline);
      deadlines.current.clear();
      for (const runtime of running.current.values()) runtime.controller.abort();
      running.current.clear();
      for (const release of pathClaims.current.values()) release();
      pathClaims.current.clear();
    };
  }, [applyOperation, patch, update]);
  const hasActiveUploads = tasks.some((task) => ["queued", "waiting", "uploading", "finalizing"].includes(task.status));
  useEffect(() => {
    if (!hasActiveUploads) return;
    const beforeUnload = (event: BeforeUnloadEvent) => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", beforeUnload);
    return () => window.removeEventListener("beforeunload", beforeUnload);
  }, [hasActiveUploads]);
  return { tasks, hasActiveUploads, enqueue, cancel, retry };
}
