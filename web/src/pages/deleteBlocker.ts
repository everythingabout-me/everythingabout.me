/**
 * Resolving the operation that a delete collided with, before the page can show a
 * user-actionable reason instead of the bare `operation_in_progress` code.
 *
 * Kept free of the API client and of UI copy so the decision can be tested directly:
 * the browser supplies reads only, and FilesPage supplies the words.
 */

export interface DeleteBlockerOperation {
  kind: string;
  state: string;
}

export interface DeleteBlockerSession {
  id: string;
  path: string;
  state: string;
}

export interface DeleteBlockerClient {
  getOperation(id: string): Promise<DeleteBlockerOperation>;
  uploadStatus(id: string): Promise<DeleteBlockerSession>;
}

export type DeleteBlocker =
  | { resolution: "retry" }
  | { resolution: "blocked"; reason: "upload"; name: string }
  | { resolution: "blocked"; reason: "other" };

const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

function statusOf(error: unknown): number | null {
  const status = (error as { status?: unknown } | null)?.status;
  return typeof status === "number" ? status : null;
}

function uploadName(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1);
}

/**
 * The server fences only the native paths an operation observes or mutates, but a delete of
 * a folder observes the whole subtree, so it can collide with anything still changing a path
 * inside it.
 *
 * An upload is such a holder only while it is committing. A session that is still `open`
 * fences nothing — it takes its guard when it commits — so the uploads a closed tab or an
 * interrupted import left behind never reach this function, and nothing here cancels
 * anything: there is no session a delete has to clear out of its way first. What can be
 * named is a publication already under way, which is not ours to undo and finishes within
 * seconds, and every other kind of operation, which is reported without a name because its
 * own Operation is the place to follow it.
 *
 * A holder that settled between the refusal and this read has released the path, so the
 * delete simply tries again.
 */
export async function resolveDeleteBlocker(
  client: DeleteBlockerClient,
  operationId: string,
): Promise<DeleteBlocker> {
  let operation: DeleteBlockerOperation;
  try {
    operation = await client.getOperation(operationId);
  } catch (error) {
    // The blocker settled between the refusal and this read; its guards are gone.
    const status = statusOf(error);
    if (status === 404 || status === 410) return { resolution: "retry" };
    // Another principal owns it (an API token's upload); this browser cannot read it, but
    // it still blocks and deserves a sentence rather than a bare code.
    if (status === 403) return { resolution: "blocked", reason: "other" };
    throw error;
  }
  if (TERMINAL.has(operation.state)) return { resolution: "retry" };
  if (operation.kind !== "upload_commit") return { resolution: "blocked", reason: "other" };

  let session: DeleteBlockerSession;
  try {
    session = await client.uploadStatus(operationId);
  } catch (error) {
    const status = statusOf(error);
    if (status === 404 || status === 410) return { resolution: "retry" };
    if (status === 403) return { resolution: "blocked", reason: "other" };
    throw error;
  }
  return { resolution: "blocked", reason: "upload", name: uploadName(session.path) };
}
