import type { FileOperation } from "../api/client";

export interface PendingFileOperation {
  id: string;
  path: string;
}

let owner: string | undefined;
let pending: PendingFileOperation[] = [];
const listeners = new Set<() => void>();
const notify = () => { for (const listener of listeners) listener(); };
const key = () => `eam.pending-file-operations.v1:${owner}`;
const canonical = (path: string) => `/${path.replace(/^\/+|\/+$/g, "")}`;

export function setUploadOwner(id: string): void {
  owner = id;
  try {
    const saved: unknown = JSON.parse(localStorage.getItem(key()) ?? "[]");
    pending = Array.isArray(saved) ? saved.filter((item): item is PendingFileOperation =>
      !!item && typeof item.id === "string" && typeof item.path === "string").slice(0, 1000) : [];
  } catch { pending = []; }
  notify();
}
export function trackFileOperation(op: FileOperation, path: string): void {
  if (!owner || op.state === "succeeded") return;
  pending = [...pending.filter((item) => item.id !== op.id), { id: op.id, path: canonical(path) }];
  try { localStorage.setItem(key(), JSON.stringify(pending)); } catch { /* Server guards remain authoritative. */ }
  notify();
}
export function settleFileOperation(id: string): void {
  pending = pending.filter((item) => item.id !== id);
  if (owner) { try { localStorage.setItem(key(), JSON.stringify(pending)); } catch { /* Keep memory state. */ } }
  notify();
}
export function pendingFileOperation(path: string): PendingFileOperation | undefined {
  const target = canonical(path);
  return pending.find((item) => target === item.path || target.startsWith(`${item.path}/`));
}
export function pendingFileOperations(): PendingFileOperation[] { return [...pending]; }
export const untrackFileOperation = settleFileOperation;
export function subscribeFileOperations(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}
