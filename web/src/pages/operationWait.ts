import { api, type FileOperation } from "../api/client";
import { t } from "../i18n";

/**
 * Follow one common Operation until it settles, reporting its progress.
 *
 * A move, copy or folder creation can answer `202` with an Operation instead of a finished
 * entry, and the dialog that asked for it has nothing else to follow. The poll is one read
 * per second: shorter only spends requests on a server that settles in whole steps, longer
 * makes the progress bar look frozen.
 *
 * The wait is deliberately not cancellable here. The caller that offers a Cancel cancels
 * the Operation itself (`api.cancelOperation`), which makes this loop observe a terminal
 * `cancelled` state; the loop then throws the server's own reason. That keeps one rule:
 * the request that stops work is the one that answers whether it stopped.
 */
export async function waitForOperation(
  operation: FileOperation,
  onProgress?: (progress: FileOperation["progress"]) => void,
): Promise<void> {
  let current = operation;
  onProgress?.(current.progress);
  while (current.state !== "succeeded") {
    if (current.state === "cancelled") {
      throw new Error(current.error?.detail || t("drive.transferCancelled"));
    }
    if (current.state === "failed") {
      throw new Error(current.error?.detail || t("error.saveFailed"));
    }
    await new Promise<void>((resolve) => window.setTimeout(resolve, 1000));
    current = await api.getOperation(current.id);
    onProgress?.(current.progress);
  }
}
