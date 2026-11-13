import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { ApiError, api, isFileOperation, type FileOperation, type Item } from "../api/client";
import { filesHref } from "../filesPath";
import { t } from "../i18n";
import { joinPath } from "./listingState";

interface RenameJobOptions {
  /** The route path being renamed; it is also the Operation's source. */
  path: string;
  /** What the path names now, whose revision the move must carry. */
  current: Item | null;
  /** The route path's parent, where a rename keeps the entry. */
  parentPath: string;
  revisionForMutation(target: Item): Promise<string>;
  /** A 412 means the revision was stale; the next attempt must read it again. */
  invalidateRevision(): void;
  showToast(text: string, kind?: "info" | "error", sticky?: boolean): void;
}

/**
 * The inline rename editor and the Operation a large rename settles through.
 *
 * A rename is a move within one parent, so it may answer synchronously or with an
 * Operation; when it answers with an Operation, the path is already the new one and the
 * page follows that Operation until it settles. The hook owns the editor, the job row and
 * the one timer that polls the job, so a navigation cannot leave a poll running for a name
 * nobody is looking at.
 *
 * Contract
 * - Caller supplies the route path, the entry it names and the folder-revision helpers.
 * - `submit` reads the editor's current name; a failure is reported through `showToast`
 *   and leaves no editor or job behind, exactly as the page's own `applyRename` did.
 */
export interface RenameJob {
  /** The inline name editor is open. */
  editing: boolean;
  name: string;
  /** The common Operation while an asynchronous rename runs, for the job row. */
  job: FileOperation | null;
  setName(value: string): void;
  /** Open the editor pre-filled with the current name. */
  start(name: string): void;
  cancel(): void;
  submit(): void;
}

/** A rename's first poll waits for the server to accept it; later ones are one per 1.2 s. */
const RENAME_FIRST_POLL_MS = 600;
const RENAME_POLL_MS = 1_200;

export function useRenameJob({
  path,
  current,
  parentPath,
  revisionForMutation,
  invalidateRevision,
  showToast,
}: RenameJobOptions): RenameJob {
  const navigate = useNavigate();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState("");
  const [job, setJob] = useState<FileOperation | null>(null);
  const timer = useRef<number | null>(null);

  const clearTimer = () => {
    if (timer.current) window.clearTimeout(timer.current);
    timer.current = null;
  };
  const cancel = () => {
    setEditing(false);
    setName("");
  };
  const finish = (to: string) => {
    clearTimer();
    setJob(null);
    cancel();
    showToast(t("drive.renamed", { name: to.split("/").pop() ?? to }));
    // The path is the identity: the new URL reloads the listing through the route effect.
    navigate(filesHref(to), { replace: true });
  };
  const fail = (message: string) => {
    clearTimer();
    setJob(null);
    showToast(message, "error");
  };
  /** Follow the Operation a large rename answered with, until it settles. */
  const poll = (id: string, to: string) => {
    const tick = async () => {
      try {
        const result = await api.getOperation(id);
        setJob(result);
        if (result.state === "succeeded") {
          finish(to);
          return;
        }
        if (result.state === "failed" || result.state === "cancelled") {
          fail(result.error?.detail || t("drive.renameFailed"));
          return;
        }
        timer.current = window.setTimeout(tick, RENAME_POLL_MS);
      } catch (error) {
        fail(error instanceof Error ? error.message : t("drive.renameFailed"));
      }
    };
    timer.current = window.setTimeout(tick, RENAME_FIRST_POLL_MS);
  };
  const submit = async () => {
    const next = name;
    if (!next) {
      cancel();
      return;
    }
    const from = path;
    const to = joinPath(parentPath, next);
    setEditing(false);
    if (to === from) return;
    try {
      if (!current) return;
      // The same stale revision a delete would send: renaming after an upload into this
      // folder must name its current state, not the one the page loaded with.
      const result = await api.move(from, to, await revisionForMutation(current));
      if (!isFileOperation(result) || result.state === "succeeded") {
        finish(to);
        return;
      }
      if (result.state === "failed" || result.state === "cancelled") {
        fail(result.error?.detail || t("drive.renameFailed"));
        return;
      }
      setJob(result);
      poll(result.id, to);
    } catch (error) {
      if (error instanceof ApiError && error.operationId) poll(error.operationId, to);
      else {
        // The folder changed between the read and the rename: re-read it before the
        // person's next attempt instead of repeating the refused revision.
        if (error instanceof ApiError && error.status === 412 && current?.kind === "folder") invalidateRevision();
        fail(error instanceof Error ? error.message : t("drive.renameFailed"));
      }
    }
  };

  // Navigating away closes the editor; the entry on screen is no longer the one being named.
  useEffect(() => { setEditing(false); }, [path]);
  // A rename keeps polling after the page unmounts without this.
  useEffect(() => () => clearTimer(), []);

  return {
    editing,
    name,
    job,
    setName,
    start: (value: string) => {
      setName(value);
      setEditing(true);
    },
    cancel,
    submit,
  };
}
