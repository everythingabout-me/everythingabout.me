/** The only backend entry point. Legacy account/event calls use envelopes; files use resources/Problems. */

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
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
    super(problem.detail, problem.status);
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

export interface FileCapabilities {
  path: string;
  read: boolean;
  write: boolean;
  range: boolean;
  pinnedRead: boolean;
  strongContentValidators: boolean;
  thumbnails: boolean;
  uploads: boolean;
  checksums: string[];
  maxFileBytes: number | null;
  maxSinglePutBytes: number;
  maxPartBytes: number | null;
  pagination: { defaultLimit: number; maxLimit: number };
  names: { caseSensitive: boolean; maxSegmentBytes: number; maxPathBytes: number; maxDepth: number };
  sessionExpirySeconds: number | null;
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

export interface Item {
  id: string;
  path: string;
  parent: string;
  name: string;
  is_dir: boolean;
  size: number;
  mime: string | null;
  created_at: string;
  updated_at: string;
  meta: Record<string, unknown>;
}

export interface Crumb {
  name: string;
  path: string;
}

export interface ListResult {
  path: string;
  parent: string;
  breadcrumb: Crumb[];
  truncated: boolean;
  /** The current path itself: a folder, a file (is_dir false), or null at the root / when missing. */
  node: Item | null;
  items: Item[];
}

export interface SearchResult {
  items: Item[];
}

/**
 * A background move/copy of a directory. The web API only surfaces it for a directory rename,
 * but the shape is the shared one: the same row backs `/move/{id}` and `/copy/{id}`.
 */
export interface Job {
  id: string;
  op: "move" | "copy";
  from: string;
  to: string;
  status: "pending" | "running" | "done" | "failed" | "cancelled";
  total: number;
  done: number;
  error?: string;
  cancelRequested: boolean;
  createdAt: string;
  startedAt: string | null;
  finishedAt: string | null;
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

export interface UploadSignResult {
  uploadId: string;
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
  const envelope = payload as { ok?: unknown; data?: unknown; error?: unknown };
  if (envelope.ok !== true) {
    const message =
      typeof envelope.error === "string" && envelope.error !== ""
        ? envelope.error
        : `request failed (HTTP ${status})`;
    throw new ApiError(message, status);
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

function fileURL(resource: string, values: Record<string, string | number | undefined> = {}): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value !== undefined) query.set(key, String(value));
  }
  const encoded = query.toString();
  return `/api/files${resource}${encoded ? `?${encoded}` : ""}`;
}

async function fileRequest<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, { ...init, credentials: "same-origin" });
  if (response.status === 204) return undefined as T;
  const payload: unknown = await response.json().catch(() => { throw httpError(response.status); });
  if (!response.ok) {
    const p = payload as Partial<FileProblem> | null;
    if (!p || typeof p.code !== "string" || typeof p.detail !== "string" || p.status !== response.status) {
      throw httpError(response.status);
    }
    throw new FileApiError(p as FileProblem);
  }
  return payload as T;
}

/** Shared file reads. Existing pages migrate to this contract in P6. */
export const files = {
  metadata(path: string): Promise<FileResource> {
    return fileRequest(fileURL("", { path }));
  },
  list(path: string, options: { limit?: number; cursor?: string } = {}): Promise<FileCollection> {
    return fileRequest(fileURL("/list", { path, ...options }));
  },
  search(options: { q: string; path?: string; kind?: "file" | "folder"; mediaType?: string; limit?: number; cursor?: string }): Promise<FileCollection> {
    return fileRequest(fileURL("/search", options));
  },
  me(): Promise<FilePrincipal> {
    return fileRequest(fileURL("/me"));
  },
  capabilities(path?: string): Promise<FileCapabilities> {
    return fileRequest(fileURL("/capabilities", { path }));
  },
  operation(id: string): Promise<FileOperation> {
    return fileRequest(fileURL(`/operations/${encodeURIComponent(id)}`));
  },
  cancelOperation(id: string, csrfToken: string): Promise<FileOperation> {
    return fileRequest(fileURL(`/operations/${encodeURIComponent(id)}/cancel`), {
      method: "POST",
      headers: { "X-CSRF-Token": csrfToken },
    });
  },
  contentURL(path: string, disposition?: "inline" | "attachment"): string {
    return fileURL("/content", { path, disposition });
  },
};

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

  list(path: string): Promise<ListResult> {
    return request<ListResult>(`/api/fs/list?path=${encodeURIComponent(path)}`);
  },

  search(keyword: string): Promise<SearchResult> {
    return request<SearchResult>(`/api/fs/search?q=${encodeURIComponent(keyword)}`);
  },

  /** Allocate a server-owned upload receipt before sending bytes. */
  signUpload(
    path: string,
    size: number,
    overwrite = false,
    mime?: string,
    modifiedAt?: number,
  ): Promise<UploadSignResult> {
    return post<UploadSignResult>("/api/fs/upload/sign", { path, size, overwrite, mime, modifiedAt });
  },

  /** Retry finalization/result retrieval without sending the file body again. */
  async confirmUpload(uploadId: string): Promise<Item> {
    const data = await post<{ node: Item }>("/api/fs/upload/confirm", { uploadId });
    return data.node;
  },

  /** Stream a File to the configured API origin; the backend publishes and indexes it. */
  uploadFile(
    uploadId: string,
    file: File,
    onProgress: (progress: UploadProgress) => void,
    onSent: () => void,
    signal: AbortSignal,
  ): Promise<Item> {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      let settled = false;
      const finish = (error?: unknown, item?: Item) => {
        if (settled) return;
        settled = true;
        signal.removeEventListener("abort", abort);
        if (error) reject(error);
        else resolve(item!);
      };
      const abort = () => {
        xhr.abort();
        finish(new DOMException("Upload canceled", "AbortError"));
      };
      if (signal.aborted) {
        finish(new DOMException("Upload canceled", "AbortError"));
        return;
      }
      const query = new URLSearchParams({ uploadId });
      xhr.open("POST", `/api/fs/upload/confirm?${query}`);
      xhr.withCredentials = true;
      xhr.setRequestHeader("Content-Type", "application/octet-stream");
      xhr.upload.onprogress = (event) => {
        if (!settled && !signal.aborted) onProgress({ loaded: event.loaded, total: file.size });
      };
      xhr.upload.onload = () => {
        if (!settled && !signal.aborted) onSent();
      };
      xhr.onload = () => {
        try {
          const data = unpack<{ node: Item }>(JSON.parse(xhr.responseText), xhr.status);
          if (xhr.status < 200 || xhr.status >= 300) throw httpError(xhr.status);
          finish(undefined, data.node);
        } catch (error) {
          finish(error instanceof ApiError ? error : httpError(xhr.status));
        }
      };
      xhr.onerror = () => finish(new ApiError("upload connection interrupted", 0));
      xhr.ontimeout = () => finish(new ApiError("upload request timed out", 0));
      xhr.onabort = () => finish(new DOMException("Upload canceled", "AbortError"));
      signal.addEventListener("abort", abort, { once: true });
      try {
        xhr.send(file);
      } catch (error) {
        finish(error);
      }
    });
  },

  async mkdir(path: string): Promise<void> {
    await post("/api/fs/mkdir", { path });
  },

  /**
   * A file is renamed in one request; a directory is a subtree, so it answers with a job whose
   * progress is polled at `renameStatus`. `job` is null when the work already finished.
   * `respond-async` keeps the request from blocking while a big directory is being copied.
   */
  async rename(from: string, to: string): Promise<{ job: Job | null }> {
    const data = await request<{ job?: Job }>("/api/fs/rename", {
      method: "POST",
      body: JSON.stringify({ from, to }),
      headers: { Prefer: "respond-async" },
    });
    return { job: data.job ?? null };
  },

  async renameStatus(id: string): Promise<{ job: Job }> {
    return request<{ job: Job }>(`/api/fs/rename/${encodeURIComponent(id)}`);
  },

  async deletePath(path: string, recursive = false): Promise<void> {
    await post(`/api/fs/delete${recursive ? "?recursive=1" : ""}`, { path });
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

  /** 302s to a presigned COS URL; use it directly as an <a href>. */
  downloadHref(path: string): string {
    return `/api/fs/download?path=${encodeURIComponent(path)}`;
  },
};
