import { useCallback, useEffect, useRef, useState } from "react";
import { ApiError, api, type FileOperation } from "../api/client";
import { t } from "../i18n";
import { pendingFileOperations, setUploadOwner, settleFileOperation, subscribeFileOperations } from "./uploadPersistence";

export interface TrackedDeletion {
  operation: FileOperation;
  path: string;
}

interface DeletionTrackerOptions {
  /** The authenticated owner whose durable records this page follows. */
  owner: string;
  showToast(text: string, kind?: "info" | "error", sticky?: boolean): void;
  /** Re-read the view behind a deleted row: a failure can put it back. */
  reload(): void;
}

/**
 * The page's view of the deletions the server is still purging.
 *
 * An accepted deletion is not user-facing work: the path disappears from the listing at
 * once and the native purge runs invisibly. Only an operation that ends without deleting
 * anything is worth reporting, because that row may be back on screen — so this hook owns
 * the poll that notices it, and nothing else in the page has to.
 *
 * Contract
 * - Caller guarantees one authenticated owner and stable `showToast`/`reload` callbacks;
 *   the hook owns the durable records' subscription, the poll and its backoff.
 * - `refresh` is the only read; `accepted` drops an answer that the just-accepted delete
 *   made stale, and `clearStatusError` lets the page clear a stale read failure.
 * - A failed or unknown outcome is reported once per Operation, never per poll.
 */
export interface DeletionTracker {
  /** Accepted deletions still running, as the breadcrumb marks them. */
  tracked: TrackedDeletion[];
  /** Whether the last status read failed; the poll then backs off instead of hammering. */
  statusError: boolean;
  /** Re-read every recorded Operation now; one failed read never hides the others. */
  refresh(): Promise<void>;
  /** A delete was just accepted: an older refresh answer is now stale. */
  accepted(): void;
  /** The server answered a delete; clear a stale status error. */
  clearStatusError(): void;
}

/** One poll while a purge runs; a failing status read backs off instead of looping. */
const DELETION_POLL_MS = 3_000;
const DELETION_POLL_BACKOFF_MS = 30_000;

export function useDeletionTracker({ owner, showToast, reload }: DeletionTrackerOptions): DeletionTracker {
  const [tracked, setTracked] = useState<TrackedDeletion[]>([]);
  const [statusError, setStatusError] = useState(false);
  const request = useRef(0);
  // One report per Operation, however many polls see its failed state.
  const notified = useRef(new Set<string>());

  const refresh = useCallback(async () => {
    const ticket = ++request.current;
    const known = pendingFileOperations();
    const active: TrackedDeletion[] = [];
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
          if (!notified.current.has(operation.id)) {
            notified.current.add(operation.id);
            showToast(t("drive.deleteIncomplete", { path: entry.path }), "error");
            reload();
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
    if (ticket !== request.current) return;
    setTracked(active.filter(({ operation }) => operation.kind === "delete"));
    setStatusError(failed);
  }, [reload, showToast]);

  useEffect(() => {
    setUploadOwner(owner);
    void refresh();
    const unsubscribe = subscribeFileOperations(() => { void refresh(); });
    return () => { request.current += 1; unsubscribe(); };
  }, [owner, refresh]);

  const activeDeletions = tracked.some(({ operation }) =>
    operation.state === "queued" || operation.state === "running" || operation.state === "outcome_unknown");
  useEffect(() => {
    if (!activeDeletions && !statusError) return;
    let stopped = false;
    let timer: number;
    // Poll while a deletion is running. When the status cannot be read the retry backs off
    // instead of hammering the API in a three-second loop.
    const tick = async () => {
      await refresh();
      if (!stopped) timer = window.setTimeout(tick, statusError ? DELETION_POLL_BACKOFF_MS : DELETION_POLL_MS);
    };
    timer = window.setTimeout(tick, statusError ? DELETION_POLL_BACKOFF_MS : DELETION_POLL_MS);
    return () => { stopped = true; window.clearTimeout(timer); };
  }, [activeDeletions, statusError, refresh]);

  return {
    tracked,
    statusError,
    refresh,
    accepted: useCallback(() => { request.current += 1; }, []),
    clearStatusError: useCallback(() => setStatusError(false), []),
  };
}
