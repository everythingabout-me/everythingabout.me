/**
 * Resolving the operation that a recursive delete collided with, before the dialog can
 * show a user-actionable reason instead of the bare `operation_in_progress` code.
 *
 * Kept free of the API client and of UI copy so the decision can be tested directly:
 * the browser supplies reads only, and FilesPage supplies the cancellation and words.
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
  | { resolution: "cancel"; id: string; name: string }
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
 * The server fences only the native paths an operation observes or mutates, but a recursive
 * delete observes the whole subtree. An upload session that never committed still holds its
 * target path, so deleting a folder above it conflicts with `operation_in_progress` long
 * after the upload's tab is gone (a closed tab, a failed part, a retry nobody clicked).
 *
 * The delete's confirmation is already final for that subtree, so an `open` upload session
 * inside it may be cancelled rather than turned into a dead end: its bytes would be deleted
 * by the same request. This resolves only the operation the 409 names, and only one that is
 * still `open`; a commit that has already started is not ours to undo and stays blocked with
 * a sentence naming it. A session owned by another principal is never cancelled from here,
 * because this browser cannot read it.
 */
export async function resolveDeleteBlocker(
  client: DeleteBlockerClient,
  operationId: string,
  targetPath: string,
): Promise<DeleteBlocker> {
  let operation: DeleteBlockerOperation;
  try {
    operation = await client.getOperation(operationId);
  } catch (error) {
    // The blocker settled between the refusal and this read; its guards are gone.
    const status = statusOf(error);
    if (status === 404 || status === 410) return { resolution: "retry" };
    // Another principal owns it (an API token's upload); this browser cannot read or
    // cancel it, but it still blocks and deserves a name rather than a bare code.
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
  const name = uploadName(session.path);
  const inside = targetPath === "/" || session.path === targetPath || session.path.startsWith(`${targetPath}/`);
  if (!inside || session.state !== "open") return { resolution: "blocked", reason: "upload", name };
  return { resolution: "cancel", id: session.id, name };
}
