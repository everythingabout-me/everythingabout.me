import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";
import { api, isFileOperation, type FileOperation, type Item } from "../api/client";
import type { TransferMode } from "../components/TransferDialog";
import { filesHref } from "../filesPath";
import { t } from "../i18n";
import { waitForOperation } from "./operationWait";

interface TransferJobOptions {
  /** The folder on screen: a copy re-reads it, a move navigates away from it. */
  path: string;
  /** Re-read a folder after the transfer (the listing controller's `load`). */
  load(target: string): Promise<void>;
  revisionForMutation(item: Item): Promise<string>;
  showToast(text: string, kind?: "info" | "error", sticky?: boolean): void;
}

/**
 * The move/copy dialog and the Operation it follows.
 *
 * A long move or copy answers `202` with an Operation instead of a finished entry, so the
 * dialog has to show the server's own progress and offer Cancel while it runs. The hook
 * owns the dialog's state, the one Operation it is following, and the wait loop; the page
 * keeps only the menu item that opens it.
 *
 * Contract
 * - Caller supplies the folder a copy re-reads and the revision helper.
 * - `confirm` throws on failure so the dialog can show it and stay open; success closes the
 *   dialog, reports, and then navigates (move) or reloads the folder (copy).
 */
export interface TransferJob {
  transfer: { mode: TransferMode; item: Item } | null;
  /** Entry counts the server reports while the move/copy runs. */
  progress: FileOperation["progress"] | null;
  cancelling: boolean;
  open(mode: TransferMode, item: Item): void;
  /** Dismiss without changing anything; an accepted Operation keeps running server-side. */
  close(): void;
  /** Stop the running Operation between entries. */
  cancel(): Promise<void>;
  /** Move or copy into `destination`; a failure is thrown for the dialog to report. */
  confirm(destination: string): Promise<void>;
}

export function useTransferJob({ path, load, revisionForMutation, showToast }: TransferJobOptions): TransferJob {
  const navigate = useNavigate();
  const [transfer, setTransfer] = useState<{ mode: TransferMode; item: Item } | null>(null);
  const [progress, setProgress] = useState<FileOperation["progress"] | null>(null);
  const [cancelling, setCancelling] = useState(false);
  const operation = useRef<string | null>(null);

  // Progress belongs to the dialog that is open now, never to the previous transfer.
  useEffect(() => {
    setProgress(null);
    setCancelling(false);
  }, [transfer]);

  const confirm = async (destination: string) => {
    if (!transfer) return;
    const { mode, item } = transfer;
    const target = destination === "/" ? `/${item.name}` : `${destination}/${item.name}`;
    const revision = await revisionForMutation(item);
    const result = mode === "move"
      ? await api.move(item.path, target, revision)
      : await api.copy(item.path, target, revision);
    if (isFileOperation(result) && result.state !== "succeeded") {
      if (result.state === "failed" || result.state === "cancelled") {
        throw new Error(result.error?.detail || t("drive.transferFailed"));
      }
      operation.current = result.id;
      setProgress(result.progress);
      try {
        await waitForOperation(result, setProgress);
      } catch (caught) {
        throw caught instanceof Error ? caught : new Error(t("drive.transferFailed"));
      } finally {
        operation.current = null;
      }
    }
    setTransfer(null);
    showToast(t(mode === "move" ? "drive.moved" : "drive.copied", { name: item.name }));
    if (mode === "move") navigate(filesHref(target), { replace: true });
    else void load(path);
  };

  const cancel = async () => {
    const id = operation.current;
    if (!id) return;
    setCancelling(true);
    try {
      await api.cancelOperation(id);
    } catch (error) {
      showToast(error instanceof Error ? error.message : t("drive.transferFailed"), "error");
    }
  };

  return {
    transfer,
    progress,
    cancelling,
    open: (mode, item) => setTransfer({ mode, item }),
    close: () => setTransfer(null),
    cancel,
    confirm,
  };
}
