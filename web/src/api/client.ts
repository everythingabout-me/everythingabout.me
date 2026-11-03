/** The only backend entry point. Legacy account/event calls use envelopes; files use resources/Problems. */

export interface DeletionConflict {
  code?: string;
  operationId?: string;
  path?: string;
  deletionStatus?: "pending" | "running" | "failed";
}

export class ApiError extends Error implements DeletionConflict {
  readonly code?: string;
  readonly operationId?: string;
  readonly path?: string;
  readonly deletionStatus?: DeletionConflict["deletionStatus"];

  constructor(
    message: string,
    readonly status: number,
    metadata: DeletionConflict = {},
  ) {
    super(message);
    this.name = "ApiError";
    this.code = metadata.code;
    this.operationId = metadata.operationId;
    this.path = metadata.path;
    this.deletionStatus = metadata.deletionStatus;
  }
}

export interface FileProblem {
  type: string;
  title: string;
  status: number;
  code: string;
  detail: string;
  requestId: string;
  operationId?: string;
}

export class FileApiError extends ApiError {
  constructor(readonly problem: FileProblem) {
    super(problem.detail, problem.status, problem);
    this.name = "FileApiError";
  }
}

export interface FileResource {
  path: string;
  parentPath: string;
  name: string;
  kind: "file" | "folder";
  revision: string;
  contentRevision: string | null;
  size: number | null;
  mediaType: string | null;
  sourceModifiedAt: string | null;
  createdAt: string;
  updatedAt: string;
  checksum: { algorithm: string; value: string } | null;
}

export interface FileCollection {
  items: FileResource[];
  nextCursor: string | null;
}

export interface FileQuota {
  logicalBytes: number;
  logicalLimitBytes: number;
  physicalBytes: number;
  physicalLimitBytes: number;
}

export interface FileReservations {
  logicalBytes: number;
  physicalBytes: number;
}

export interface FileEffect {
  number: number;
  action: "mkdir" | "copy" | "move" | "delete";
  state: string;
  source?: string;
  target?: string;
}

export interface FileOperation {
  id: string;
  kind: "mkdir" | "move" | "copy" | "delete" | "upload_commit" | "thumbnail";
  state: "queued" | "running" | "outcome_unknown" | "succeeded" | "failed" | "cancelled";
  progress: { completed: number; total: number | null; unit: string };
  result: FileResource | { deleted: boolean } | { effects: FileEffect[]; remaining: FileEffect[] } | null;
  error: FileProblem | null;
  cancelRequested: boolean;
  createdAt: string;
  finishedAt: string | null;
}

export interface FilePrincipal {
  principal: { kind: "session" | "token" };
  scope: { path: string };
  read: boolean;
  write: boolean;
  usage: { bytes: number; files: number; indexed: boolean };
  quota: FileQuota | null;
  reservations: FileReservations | null;
  csrfToken?: string;
}

export type FileUploadTransport = "embedded" | "cos-signed";

export interface FileUploadSession {
  id: string;
  transport?: FileUploadTransport;
  path: string;
  state: "open" | "committing" | "committed" | "outcome_unknown" | "failed" | "aborted" | "expired";
  size: number;
  partSize: number;
  partCount: number;
  maxParallelism: number;
  ordering: "any" | "ascending";
  nextPart: number | null;
  receivedParts: number;
  receivedBytes: number;
  createdAt: string;
  expiresAt: string;
  partsUrl: string;
  commitUrl: string;
  operationId: string;
  result: FileResource | null;
}

export interface FileUploadReceipt {
  number: number;
  size: number;
  sha256: string;
  checksumSource?: "client-sha256-cos-md5";
}

export interface FileUploadAuthorization {
  method: "PUT";
  url: string;
  headers: Record<string, string>;
  expiresAt: string;
}

export interface FileUploadReceipts {
  items: FileUploadReceipt[];
  nextCursor: string | null;
  watermark: number;
}

export interface FileCapabilities {
  path: string;
  read: boolean;
  write: boolean;
  range: boolean;
  pinnedRead: boolean;
  strongContentValidators: boolean;
  thumbnails: boolean;
  uploads: boolean;
  uploadTransport?: FileUploadTransport;
  sourceModifiedAt: boolean;
  checksums: string[];
  maxFileBytes: number | null;
  maxSinglePutBytes: number;
  maxPartBytes: number | null;
  pagination: { defaultLimit: number; maxLimit: number };
  names: { caseSensitive: boolean; maxSegmentBytes: number; maxPathBytes: number; maxDepth: number };
  sessionExpirySeconds: number | null;
  sessionMaxLifetimeSeconds: number | null;
  maxUploadParts: number;
  maxUploadParallelism: number;
  terminalRetentionSeconds: number | null;
  quota: FileQuota | null;
  reservations: FileReservations | null;
}

export interface AuthUser {
  id: string;
  email: string;
  hasPassword: boolean;
  createdAt: string;
  emailChangedAt: string | null;
}

/** File pages consume the public resource without a compatibility DTO. */
export type Item = FileResource;

export interface Crumb {
  name: string;
  path: string;
}

export interface ListResult {
  path: string;
  parent: string;
  breadcrumb: Crumb[];
  /** The current file/folder, or null when the path is missing. */
  node: Item | null;
  items: Item[];
}

export interface SearchResult {
  items: Item[];
}

export interface EventMedia {
  path: string;
  url?: string;
  name?: string;
  video?: boolean;
}

export interface EventMetric {
  label: string;
  value: string;
  unit?: string;
}

/** Card content. `type` decides which fields are set; see docs/design/events.md section 3. */
export interface EventPayload {
  body?: string;
  meta?: string;
  media?: EventMedia[];
  metrics?: EventMetric[];
  html?: string;
  height?: number;
}

export type EventType = "photo" | "note" | "metric" | "html";

export interface EventSource {
  kind: "native" | "auto" | "app";
  name?: string;
  disconnected?: boolean;
}

/** One event on the timeline. */
export interface TimelineEvent {
  id: string;
  type: EventType;
  title: string;
  starts_at: string;
  ends_at?: string;
  local_date: string;
  precision: "day" | "minute";
  detail_url?: string;
  source: EventSource;
  payload: EventPayload;
}

export interface StreamStatus {
  enabled: boolean;
  pending: number;
  indexed_files: number;
  earliest_month?: string;
  latest_month?: string;
  last_generated?: string;
}

export interface EventPage {
  events: TimelineEvent[];
  next?: string;
  status: StreamStatus;
}

export interface SaveLink {
  id: string;
  name: string;
  code: string;
  path: string;
  createdAt: string;
  lastUsedAt: string | null;
  revoked: boolean;
}

export interface ActivityItem {
  action: string;
  path: string;
  status: number;
  source: string;
  title: string;
  createdAt: string;
}

/** One Connect credential: an app (QR) or a temporary token (text). */
export type ConnectionKind = "app" | "token";

export interface Connection {
  id: string;
  name: string;
  kind: ConnectionKind;
  prefix: string;
  read: boolean;
  write: boolean;
  events: boolean;
  createdAt: string;
  lastUsedAt: string | null;
  expiresAt: string | null;
  revoked: boolean;
  revokedAt: string | null;
}

export interface ConnectData {
  tokens: Connection[];
  api: string;
}

export interface CreatedConnection {
  token: Connection;
  secret: string;
  api: string;
  payload: string;
  qr: string;
}

export interface AccountSummary {
  user: AuthUser;
  legacyCount: number;
  saveLinks: SaveLink[];
  activity: ActivityItem[];
}

export interface FileUploadInput {
  path: string;
  size: number;
  mediaType: string;
  condition: { mode: "create" } | { mode: "replace"; contentRevision: string };
  sourceModifiedAt?: string;
}

export interface FileUploadCollection {
  items: FileUploadSession[];
  nextCursor: string | null;
}

export interface UploadProgress {
  loaded: number;
  total: number;
}

export interface CodeResult {
  expiresIn: number;
  devCode?: string;
}

/**
 * When the response is not the envelope: the backend is probably down, or something else listens
 * on the port the /api proxy targets.
 */
function httpError(status: number): ApiError {
  return new ApiError(
    `unexpected response from the API (HTTP ${status}); is the backend running?`,
    status,
  );
}

/**
 * Unwrap the envelope; a bad shape raises a locatable error instead of stringifying to
 * "[object Object]".
 */
function unpack<T>(payload: unknown, status: number): T {
  if (typeof payload !== "object" || payload === null || !("ok" in payload)) {
    throw httpError(status);
  }
  const envelope = payload as Record<string, unknown>;
  if (envelope.ok !== true) {
    const message =
      typeof envelope.error === "string" && envelope.error !== ""
        ? envelope.error
        : `request failed (HTTP ${status})`;
    const metadata: DeletionConflict = {};
    if (typeof envelope.code === "string" && envelope.code) metadata.code = envelope.code;
    // Incomplete deletion metadata must never enable an automatic retry.
    if (status === 409 && metadata.code === "deletion_in_progress"
      && typeof envelope.operationId === "string" && envelope.operationId.trim()
      && typeof envelope.path === "string" && envelope.path.trim()
      && (envelope.deletionStatus === "pending" || envelope.deletionStatus === "running" || envelope.deletionStatus === "failed")) {
      metadata.operationId = envelope.operationId;
      metadata.path = envelope.path;
      metadata.deletionStatus = envelope.deletionStatus;
    }
    throw new ApiError(message, status, metadata);
  }
  return envelope.data as T;
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: {
      ...(init?.body ? { "content-type": "application/json" } : {}),
      ...init?.headers,
    },
  });

  let payload: unknown;
  try {
    payload = await response.json();
  } catch {
    // With the backend down the Vite proxy returns an HTML error page, not an envelope.
    throw httpError(response.status);
  }
  return unpack<T>(payload, response.status);
}

function post<T>(path: string, body?: unknown): Promise<T> {
  return request<T>(path, {
    method: "POST",
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

/** API paths are absolute; route and queue paths may omit the leading slash. */
export function filePath(path: string): string {
  return path === "" ? "/" : path.startsWith("/") ? path : `/${path}`;
}

function fileURL(resource: string, values: Record<string, string | number | undefined> = {}): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined) query.set(key, String(value));
  }
  const encoded = query.toString();
  return `/api/files${resource}${encoded ? `?${encoded}` : ""}`;
}

function filePayload<T>(payload: unknown, status: number): T {
  if (status < 200 || status >= 300) {
    const problem = payload as Partial<FileProblem> | null;
    if (!problem || typeof problem.code !== "string" || typeof problem.detail !== "string" || problem.status !== status) {
      throw httpError(status);
    }
    throw new FileApiError(problem as FileProblem);
  }
  return payload as T;
}

let csrfRequest: Promise<string> | undefined;
async function fileCSRF(): Promise<string> {
  if (!csrfRequest) {
    csrfRequest = fileRequest<FilePrincipal>(fileURL("/me")).then((principal) => {
      if (!principal.csrfToken) throw new ApiError("missing file session CSRF token", 403);
      return principal.csrfToken;
    }).catch((error) => { csrfRequest = undefined; throw error; });
  }
  return csrfRequest;
}

async function fileRequest<T>(url: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.method && !["GET", "HEAD"].includes(init.method)) {
    headers.set("X-CSRF-Token", await fileCSRF());
  }
  const response = await fetch(url, { ...init, headers, credentials: "same-origin" });
  if (response.status === 401 || response.status === 403) csrfRequest = undefined;
  if (response.status === 204) return undefined as T;
  const payload: unknown = await response.json().catch(() => { throw httpError(response.status); });
  return filePayload<T>(payload, response.status);
}

function filePost<T>(resource: string, body?: unknown, headers?: HeadersInit, signal?: AbortSignal): Promise<T> {
  return fileRequest(fileURL(resource), {
    method: "POST", signal,
    headers: { ...(body === undefined ? {} : { "Content-Type": "application/json" }), ...headers },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

function signedPartTarget(grant: FileUploadAuthorization, md5: string): { url: string; headers: Record<string, string> } {
  const prefix = "/api/files/transfer/";
  if (grant?.method !== "PUT" || typeof grant.url !== "string" || !grant.url.startsWith(prefix)
    || /[\\\\#\s\u0000-\u001f\u007f]/.test(grant.url) || /%(?:0[0-9a-f]|1[0-9a-f]|7f|5c)/i.test(grant.url)) throw new Error("Invalid signed upload target.");
  const parsed = new URL(grant.url, "https://upload.invalid");
  if (parsed.origin !== "https://upload.invalid" || !parsed.pathname.startsWith(prefix)
    || parsed.pathname.length === prefix.length || !parsed.search || parsed.hash) throw new Error("Invalid signed upload target.");
  if (!Number.isFinite(Date.parse(grant.expiresAt)) || Date.parse(grant.expiresAt) <= Date.now()) throw new Error("The signed upload authorization has expired.");
  const entries = Object.entries(grant.headers ?? {});
  const headers: Record<string, string> = {};
  for (const [name, value] of entries) {
    const lower = name.toLowerCase();
    if (!["content-md5", "content-type"].includes(lower) || typeof value !== "string" || headers[lower] !== undefined) throw new Error("Invalid signed upload headers.");
    headers[lower] = value;
  }
  if (headers["content-md5"] !== md5 || headers["content-type"] !== "application/octet-stream") throw new Error("Invalid signed upload headers.");
  return { url: grant.url, headers };
}

function sendSignedPart(target: { url: string; headers: Record<string, string> }, blob: Blob, signal?: AbortSignal,
  onProgress?: (progress: UploadProgress) => void): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) { reject(new DOMException("Upload canceled", "AbortError")); return; }
    const xhr = new XMLHttpRequest();
    let settled = false;
    const finish = (error?: unknown) => {
      if (settled) return;
      settled = true;
      signal?.removeEventListener("abort", abort);
      if (error) reject(error); else resolve();
    };
    const abort = () => { xhr.abort(); finish(new DOMException("Upload canceled", "AbortError")); };
    xhr.open("PUT", target.url);
    // Same-origin cookies are still sent by browsers; EdgeOne must strip them
    // before COS. Never attach application Authorization or CSRF to this request.
    xhr.withCredentials = false;
    for (const [name, value] of Object.entries(target.headers)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (event) => {
      if (!settled && !signal?.aborted) onProgress?.({ loaded: event.loaded, total: blob.size });
    };
    // COS returns XML/empty bodies, not application JSON receipts. Its reply is
    // only a transport outcome; the application confirms the native part below.
    xhr.onload = () => finish(xhr.status >= 200 && xhr.status < 300 ? undefined : new ApiError("signed part transfer failed", xhr.status));
    xhr.onerror = () => finish(new ApiError("upload connection interrupted", 0));
    xhr.ontimeout = () => finish(new ApiError("upload request timed out", 0));
    xhr.onabort = () => finish(new DOMException("Upload canceled", "AbortError"));
    signal?.addEventListener("abort", abort, { once: true });
    try { xhr.send(blob); } catch (error) { finish(error); }
  });
}

async function uploadSignedPart(id: string, number: number, blob: Blob, signal?: AbortSignal,
  onProgress?: (progress: UploadProgress) => void): Promise<FileUploadReceipt> {
  if (signal?.aborted) throw new DOMException("Upload canceled", "AbortError");
  const { uploadDigests } = await import("./uploadDigests");
  const digests = await uploadDigests(blob);
  if (signal?.aborted) throw new DOMException("Upload canceled", "AbortError");
  const part = `/uploads/${encodeURIComponent(id)}/parts/${number}`;
  const grant = await filePost<FileUploadAuthorization>(`${part}/authorize`, { size: blob.size, ...digests }, undefined, signal);
  const target = signedPartTarget(grant, digests.md5);
  const confirm = async () => {
    const receipt = await filePost<FileUploadReceipt>(`${part}/confirm`, undefined, undefined, signal);
    if (receipt.number !== number || receipt.size !== blob.size || receipt.sha256 !== digests.sha256
      || receipt.checksumSource !== "client-sha256-cos-md5") throw new Error("The confirmed upload part identity changed.");
    return receipt;
  };
  try { return await confirm(); }
  catch (error) {
    if (!(error instanceof ApiError) || error.status !== 409 || error.code !== "part_pending") throw error;
  }
  try { await sendSignedPart(target, blob, signal, onProgress); }
  catch (error) {
    if (signal?.aborted) throw error;
    // A lost COS reply may hide durable success. Always confirm before allowing
    // a caller to obtain a new grant and replay this immutable part.
  }
  return confirm();
}

export function isFileOperation(result: FileResource | FileOperation | void): result is FileOperation {
  return Boolean(result && "state" in result && "id" in result);
}

/** Unified resources and same-origin byte URLs. No provider locations are consumed. */
export const files = {
  metadata(path: string, signal?: AbortSignal): Promise<FileResource> {
    return fileRequest(fileURL("", { path: filePath(path) }), { signal });
  },
  list(path: string, options: { limit?: number; cursor?: string } = {}): Promise<FileCollection> {
    return fileRequest(fileURL("/list", { ...options, path: filePath(path) }));
  },
  search(options: { q: string; path?: string; kind?: "file" | "folder"; mediaType?: string; limit?: number; cursor?: string }): Promise<FileCollection> {
    return fileRequest(fileURL("/search", { ...options, path: options.path === undefined ? undefined : filePath(options.path) }));
  },
  me(): Promise<FilePrincipal> {
    return fileRequest(fileURL("/me"));
  },
  capabilities(path?: string): Promise<FileCapabilities> {
    return fileRequest(fileURL("/capabilities", { path: path === undefined ? undefined : filePath(path) }));
  },
  operation(id: string, signal?: AbortSignal): Promise<FileOperation> {
    return fileRequest(fileURL(`/operations/${encodeURIComponent(id)}`), { signal });
  },
  cancelOperation(id: string, signal?: AbortSignal): Promise<FileOperation> {
    return filePost(`/operations/${encodeURIComponent(id)}/cancel`, undefined, undefined, signal);
  },
  contentURL(path: string, disposition: "inline" | "attachment" = "attachment"): string {
    return fileURL("/content", { path: filePath(path), disposition });
  },
  thumbnailURL(path: string, width = 320, format = "webp"): string {
    return fileURL("/thumbnail", { path: filePath(path), width, format });
  },
};

async function allFiles(load: (cursor?: string) => Promise<FileCollection>): Promise<FileResource[]> {
  const items = new Map<string, FileResource>();
  let cursor: string | undefined;
  do {
    const page = await load(cursor);
    for (const item of page.items) items.set(item.path, item);
    cursor = page.nextCursor ?? undefined;
  } while (cursor);
  return [...items.values()];
}

export const api = {
  async me(): Promise<AuthUser | null> {
    const data = await request<{ user: AuthUser | null }>("/api/auth/me");
    return data.user;
  },

  async account(): Promise<AccountSummary> {
    return request<AccountSummary>("/api/account");
  },

  connect(): Promise<ConnectData> {
    return request<ConnectData>("/api/connect/tokens");
  },

  createConnection(input: {
    /** Required for a token; an app may start nameless and name itself through PATCH /me. */
    name?: string;
    kind: ConnectionKind;
    prefix: string;
    read: boolean;
    write: boolean;
    events: boolean;
    expiresIn: number;
  }): Promise<CreatedConnection> {
    return post<CreatedConnection>("/api/connect/tokens", input);
  },

  /** Rewrite an existing connection's policy. The secret, name and kind cannot change. */
  updateConnection(
    id: string,
    input: { prefix: string; read: boolean; write: boolean; events: boolean; expiresIn: number },
  ): Promise<{ token: Connection }> {
    return request<{ token: Connection }>(`/api/connect/tokens/${encodeURIComponent(id)}`, {
      method: "PATCH",
      headers: { "content-type": "application/json" },
      body: JSON.stringify(input),
    });
  },

  async revokeConnection(id: string): Promise<void> {
    await request(`/api/connect/tokens/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  createSaveLink(): Promise<{ link: SaveLink }> {
    return post<{ link: SaveLink }>("/api/save-links");
  },

  async revokeSaveLink(id: string): Promise<void> {
    await request(`/api/save-links/${encodeURIComponent(id)}`, { method: "DELETE" });
  },

  sendCode(email: string): Promise<CodeResult> {
    return post<CodeResult>("/api/auth/code", { email });
  },

  async verifyCode(email: string, code: string): Promise<{ user: AuthUser; created: boolean }> {
    return post("/api/auth/verify", { email, code });
  },

  async loginWithPassword(email: string, password: string): Promise<{ user: AuthUser }> {
    return post("/api/auth/login", { email, password });
  },

  async logout(): Promise<void> {
    await post("/api/auth/logout");
    csrfRequest = undefined;
  },

  async setPassword(input: {
    password: string;
    currentPassword?: string;
  }): Promise<{ user: AuthUser }> {
    return post("/api/auth/password", input);
  },

  async removePassword(currentPassword: string): Promise<{ user: AuthUser }> {
    return request<{ user: AuthUser }>("/api/auth/password", {
      method: "DELETE",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ currentPassword }),
    });
  },

  sendCurrentEmailCode(): Promise<CodeResult> {
    return post<CodeResult>("/api/auth/email/current-code");
  },

  sendNewEmailCode(email: string): Promise<CodeResult> {
    return post<CodeResult>("/api/auth/email/code", { email });
  },

  async changeEmail(input: {
    email: string;
    oldCode: string;
    code: string;
  }): Promise<{ user: AuthUser }> {
    return post("/api/auth/email", input);
  },

  metadata: files.metadata,
  fileMe: files.me,
  capabilities: files.capabilities,
  getOperation: files.operation,
  cancelOperation: files.cancelOperation,
  contentURL: files.contentURL,
  thumbnailURL: files.thumbnailURL,

  /** App navigation composed from resources, rather than a legacy server listing DTO. */
  async list(path: string): Promise<ListResult> {
    const absolute = filePath(path);
    let node: FileResource | null;
    try { node = await files.metadata(absolute); }
    catch (error) {
      if (!(error instanceof ApiError) || error.status !== 404) throw error;
      node = null;
    }
    const breadcrumb: Crumb[] = [{ name: "", path: "/" }];
    let current = "";
    for (const name of absolute.split("/").filter(Boolean)) {
      current += `/${name}`;
      breadcrumb.push({ name, path: current });
    }
    return {
      path: absolute, parent: node?.parentPath ?? (absolute.slice(0, absolute.lastIndexOf("/")) || "/"),
      breadcrumb, node,
      items: node?.kind === "folder" ? await allFiles((cursor) => files.list(absolute, { cursor })) : [],
    };
  },

  async search(keyword: string): Promise<SearchResult> {
    return { items: await allFiles((cursor) => files.search({ q: keyword, cursor })) };
  },

  mkdir(path: string, parents = false): Promise<FileResource | FileOperation> {
    return filePost("", { kind: "folder", path: filePath(path), parents });
  },

  move(from: string, to: string, ifRevision: string): Promise<FileResource | FileOperation> {
    return filePost("/move", { from: filePath(from), to: filePath(to), ifRevision }, { Prefer: "respond-async" });
  },

  copy(from: string, to: string, ifSourceRevision: string): Promise<FileResource | FileOperation> {
    return filePost("/copy", { from: filePath(from), to: filePath(to), ifSourceRevision }, { Prefer: "respond-async" });
  },

  deletePath(path: string, recursive = false, ifRevision?: string): Promise<FileOperation | void> {
    return fileRequest(fileURL("", { path: filePath(path), recursive: recursive ? "true" : undefined }), {
      method: "DELETE", headers: { Prefer: "respond-async", ...(ifRevision ? { "If-Revision": ifRevision } : {}) },
    });
  },

  createUpload(input: FileUploadInput, key: string, signal?: AbortSignal): Promise<FileUploadSession> {
    return filePost("/uploads", { ...input, path: filePath(input.path) }, { "Idempotency-Key": key }, signal);
  },

  async uploadPart(id: string, number: number, blob: Blob, signal?: AbortSignal, onProgress?: (progress: UploadProgress) => void,
    transport: FileUploadTransport = "embedded"): Promise<FileUploadReceipt> {
    if (transport === "cos-signed") return uploadSignedPart(id, number, blob, signal, onProgress);
    if (transport !== "embedded") throw new Error("Unsupported upload transport.");
    const token = await fileCSRF();
    if (signal?.aborted) throw new DOMException("Upload canceled", "AbortError");
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      let settled = false;
      const finish = (error?: unknown, receipt?: FileUploadReceipt) => {
        if (settled) return;
        settled = true;
        signal?.removeEventListener("abort", abort);
        if (error) reject(error); else resolve(receipt!);
      };
      const abort = () => { xhr.abort(); finish(new DOMException("Upload canceled", "AbortError")); };
      xhr.open("PUT", fileURL(`/uploads/${encodeURIComponent(id)}/parts/${number}`));
      xhr.withCredentials = true;
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.setRequestHeader("X-CSRF-Token", token);
      xhr.upload.onprogress = (event) => {
        if (!settled && !signal?.aborted) onProgress?.({ loaded: event.loaded, total: blob.size });
      };
      xhr.onload = () => {
        if (xhr.status === 401 || xhr.status === 403) csrfRequest = undefined;
        try { finish(undefined, filePayload<FileUploadReceipt>(JSON.parse(xhr.responseText), xhr.status)); }
        catch (error) { finish(error instanceof ApiError ? error : httpError(xhr.status)); }
      };
      xhr.onerror = () => finish(new ApiError("upload connection interrupted", 0));
      xhr.ontimeout = () => finish(new ApiError("upload request timed out", 0));
      xhr.onabort = () => finish(new DOMException("Upload canceled", "AbortError"));
      signal?.addEventListener("abort", abort, { once: true });
      try { xhr.send(blob); } catch (error) { finish(error); }
    });
  },

  uploadStatus(id: string, signal?: AbortSignal): Promise<FileUploadSession> {
    return fileRequest(fileURL(`/uploads/${encodeURIComponent(id)}`), { signal });
  },

  uploadParts(id: string, cursor?: string, signal?: AbortSignal): Promise<FileUploadReceipts> {
    return fileRequest(fileURL(`/uploads/${encodeURIComponent(id)}/parts`, { cursor }), { signal });
  },

  commitUpload(id: string, signal?: AbortSignal): Promise<FileResource | FileOperation> {
    return filePost(`/uploads/${encodeURIComponent(id)}/commit`, undefined, undefined, signal);
  },

  abortUpload(id: string, signal?: AbortSignal): Promise<void> {
    return fileRequest(fileURL(`/uploads/${encodeURIComponent(id)}`), { method: "DELETE", signal });
  },

  listUploads(cursor?: string, signal?: AbortSignal): Promise<FileUploadCollection> {
    return fileRequest(fileURL("/uploads", { cursor }), { signal });
  },

  /** Timeline: one flat event stream with keyset pagination. */
  events(cursor?: string, limit = 20): Promise<EventPage> {
    const params = new URLSearchParams();
    if (cursor) params.set("cursor", cursor);
    params.set("limit", String(limit));
    return request<EventPage>(`/api/timeline?${params.toString()}`);
  },

  streamStatus(): Promise<StreamStatus> {
    return request<StreamStatus>("/api/timeline/status");
  },

  /** Same-origin attachment; provider locations never reach the browser. */
  downloadHref(path: string): string {
    return files.contentURL(path, "attachment");
  },
};
