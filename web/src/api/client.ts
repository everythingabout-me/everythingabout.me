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

/**
 * The index still lists the file, but storage has no object at its path: someone moved or
 * deleted it outside the app. Read grants refuse this instead of handing out a URL that
 * can only answer 404, so callers can offer a sync instead of a broken preview.
 */
export function isNativeAbsent(error: unknown): boolean {
  // A failed download item arrives as a Problem inside a 200 batch, so the grant helper
  // rethrows it as the base ApiError; a direct file call raises FileApiError instead.
  return error instanceof ApiError && error.status === 404 && error.code === "native_absent";
}

/** Service identity and native location select the same resource. A string retains legacy path convenience. */
export type EntrySelector = { id: string; path?: never } | { path: string; id?: never };
export type FileSelector = string | EntrySelector;
export type Destination =
  | { path: string; parent?: never; name?: never }
  | { parent: EntrySelector; name: string; path?: never };
export type FileDestination = Destination;

export type FileSearchOptions = {
  q: string;
  kind?: "file" | "folder";
  mediaType?: string;
  limit?: number;
  cursor?: string;
} & ({ path?: string; id?: never } | { id: string; path?: never });

/** Indexed metadata; browsing never requires a live storage lookup. */
export interface FileResource {
  id: string;
  parentId: string | null;
  path: string;
  parentPath: string | null;
  name: string;
  kind: "file" | "folder";
  /** Revision of the indexed metadata and direct folder membership. */
  revision: string;
  /** Saved content generation; content and replacement requests revalidate native evidence. */
  contentRevision: string | null;
  size: number | null;
  mediaType: string | null;
  sourceModifiedAt: string | null;
  createdAt: string;
  updatedAt: string;
  /** Confirmed observed-index change state; folder values include visible descendants. */
  lastChangedAt?: string;
  /** Opaque equality marker, not a content validator or feed cursor. */
  changeToken?: string;
  checksum: { algorithm: string; value: string } | null;
}

export type FileChangeFilter =
  | { target: EntrySelector; kind: "file"; recursive: false }
  | { target: EntrySelector; kind: "folder"; recursive: boolean };

export interface FileChangeSnapshot {
  id: string;
  state: "ready";
  filter: FileChangeFilter;
  totalEntries: number;
  entriesUrl: string;
  changesCursor: string;
  createdAt: string;
  expiresAt: string;
}

export interface FileChangeSnapshotPage extends FileChangeSnapshot {
  items: FileResource[];
  nextCursor: string | null;
}

export interface FileChange {
  id: string;
  type: "upsert" | "remove" | "moved" | "invalidated" | "watch_ended";
  coverage: "entry" | "subtree";
  entryId?: string;
  before?: FileResource;
  after?: FileResource;
  path?: string;
  impactPaths?: string[];
  affectedFolders?: FileResource[];
  origin: string;
  observedAt: string;
  reason?: "deleted" | "access_lost";
}

export interface FileChangesPage {
  items: FileChange[];
  /** Always present, including empty pages; save with the applied changes. */
  nextCursor: string;
  hasMore: boolean;
}

export type FileSubscriptionInput = FileChangeFilter & { delivery: { type: "sse" } };

export interface FileSubscription {
  id: string;
  filter: FileChangeFilter;
  state: "active" | "ended";
  expiresAt: string;
  changesCursor: string;
  eventsUrl: string;
}

export interface FileSubscriptionNotification {
  subscriptionId: string;
  type: "changes_available";
  /** SSE id identifies a wakeup, never a consumer change checkpoint. */
  notificationId: string;
}

export interface FileAuditActor {
  kind: string;
  principalId?: string;
  appId?: string;
  grantId?: string;
  label?: string;
  scope?: string;
  canRead?: boolean;
  canWrite?: boolean;
  canChanges?: boolean;
  canSubscribe?: boolean;
  canEvents?: boolean;
  policyGeneration?: string;
}

export interface FileAudit {
  id: string;
  action: string;
  outcome: string;
  allowed: boolean | null;
  status: number;
  bytesWritten?: number;
  reasonCode?: string;
  entryId?: string;
  path?: string;
  actor: FileAuditActor;
  operationId?: string;
  requestId?: string;
  origin: string;
  observedAt: string;
}

export interface FileSyncReport {
  dryRun: boolean;
  scanned: number;
  inserted: number;
  updated: number;
  unchanged: number;
  removed: number;
  removedPaths?: string[];
}

export interface FileAuditPage {
  items: FileAudit[];
  nextCursor: string | null;
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
  changes: boolean;
  subscribe: boolean;
  usage: { bytes: number; files: number; indexed: boolean };
  quota: FileQuota | null;
  reservations: FileReservations | null;
  csrfToken?: string;
}

export type FileTransferClass = "A" | "B" | "C";
export type FileTransferKind = "presigned-parts" | "resumable-session" | "relay";

export interface FileUploadPartGrant {
  number: number;
  size: number;
  method: "PUT";
  url: string;
  headers: Record<string, string>;
  expiresAt: string;
}

export type FileUploadTransport =
  | { kind: "presigned-parts"; parts: FileUploadPartGrant[]; signUrl: string; expiresAt: string }
  | { kind: "resumable-session"; url: string; chunkSize: number; credential?: { scheme: "bearer"; value: string; expiresAt: string } }
  | { kind: "relay"; url: string };

export interface FileUploadSession {
  id: string;
  class: FileTransferClass;
  transport?: FileUploadTransport;
  path: string;
  state: "open" | "committing" | "committed" | "outcome_unknown" | "failed" | "aborted" | "expired";
  size: number;
  partSize: number;
  partCount: number;
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
}

export interface FileProviderPart {
  number: number;
  size: number;
  etag: string;
}

export interface FileUploadPartsPage {
  items: (FileUploadReceipt | FileProviderPart)[];
  nextCursor: string | null;
  watermark?: number;
}

export interface FileDownloadGrant {
  id?: string;
  kind: "url" | "bearer";
  url?: string;
  headers?: Record<string, string>;
  expiresAt?: string;
  size?: number;
  contentType?: string;
  acceptRanges?: boolean;
  etag?: string;
  conditions?: string[];
  code?: string;
  detail?: string;
  status?: number;
}

export interface FileDownloadGrantCollection {
  items: FileDownloadGrant[];
}

export interface FileChangeTrackingCapabilities {
  available: boolean;
  read: boolean;
  subscribe: boolean;
  delivery: "sse"[];
  retentionSeconds: number;
  subscriptionLifetimeSeconds: number;
  snapshotLifetimeSeconds: number;
  snapshotMaxEntries: number;
  snapshotMaxBytes: number;
  observation: "indexed";
  externalChanges: "explicit-reconcile";
}

export interface FileCapabilities {
  identity: { id: boolean; path: boolean; uniquePaths: boolean };
  changeTracking: FileChangeTrackingCapabilities;
  path: string;
  read: boolean;
  write: boolean;
  range: boolean;
  pinnedRead: boolean;
  strongContentValidators: boolean;
  thumbnails: boolean;
  uploads: boolean;
  transferClass: FileTransferClass;
  transferKind: FileTransferKind;
  transferClasses: string[];
  atomicCreate: boolean;
  downloads: boolean;
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
  changes: boolean;
  subscribe: boolean;
  policyVersion: number;
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

export type FileUploadCondition = { mode: "create" } | { mode: "replace"; contentRevision: string };

export type FileUploadInput = {
  size: number;
  mediaType: string;
  sourceModifiedAt?: string;
} & (
  | { path: string; condition: FileUploadCondition; destination?: never; target?: never }
  | { destination: Destination; condition?: { mode: "create" }; path?: never; target?: never }
  | { target: EntrySelector; condition: { mode: "replace"; contentRevision: string }; path?: never; destination?: never }
);

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

function invalidSelector(): never {
  throw new ApiError("Select exactly one file ID or path, or one creation destination.", 400, { code: "invalid_selector" });
}

function hasOwn(value: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(value, key);
}

function entrySelector(value: FileSelector): EntrySelector {
  // Empty route/queue strings historically select root; explicit JSON selectors must be nonempty.
  if (typeof value === "string") return { path: filePath(value) };
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).length !== 1) return invalidSelector();
  if (hasOwn(value, "id") && typeof value.id === "string" && value.id.trim() !== "") return { id: value.id };
  if (hasOwn(value, "path") && typeof value.path === "string" && value.path !== "") return { path: filePath(value.path) };
  return invalidSelector();
}

function fileDestination(value: Destination): Destination {
  if (!value || typeof value !== "object" || Array.isArray(value)) return invalidSelector();
  const keys = Object.keys(value);
  if (keys.length === 1 && hasOwn(value, "path")) return entrySelector({ path: value.path! }) as { path: string };
  if (keys.length !== 2 || !hasOwn(value, "parent") || !hasOwn(value, "name")
    || typeof value.name !== "string" || value.name === "" || typeof value.parent !== "object" || value.parent === null) return invalidSelector();
  if (value.name === "." || value.name === ".." || /[/\\\u0000-\u001f\u007f]/.test(value.name)) {
    throw new ApiError("A destination name must be one valid filename segment.", 400, { code: "invalid_path" });
  }
  return { parent: entrySelector(value.parent), name: value.name };
}

function uploadInput(input: FileUploadInput): Record<string, unknown> {
  if (!input || typeof input !== "object" || ["path", "target", "destination"].filter((key) => hasOwn(input, key)).length !== 1) return invalidSelector();
  if (hasOwn(input, "condition") && (!input.condition || typeof input.condition !== "object" || !hasOwn(input.condition, "mode"))) return invalidSelector();
  const mode = input.condition?.mode ?? "create";
  if (mode !== "create" && mode !== "replace") return invalidSelector();
  if (mode === "replace" && (input.condition?.mode !== "replace" || typeof input.condition.contentRevision !== "string" || input.condition.contentRevision === "")) return invalidSelector();
  if (hasOwn(input, "path")) {
    if (typeof input.path !== "string" || input.path === "") return invalidSelector();
    return { ...input, path: filePath(input.path) };
  }
  if (hasOwn(input, "target")) {
    if (mode !== "replace" || !input.target || typeof input.target !== "object") return invalidSelector();
    return { ...input, target: entrySelector(input.target) };
  }
  if (mode !== "create") return invalidSelector();
  return { ...input, destination: fileDestination(input.destination!) };
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
  if (response.status === 401 || response.status === 403) forgetSession();
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

function presignedPartTarget(grant: FileUploadPartGrant): { url: string; headers: Record<string, string> } {
  if (grant?.method !== "PUT" || typeof grant.url !== "string" || typeof grant.size !== "number"
    || !Number.isSafeInteger(grant.number) || grant.number < 1 || !Number.isSafeInteger(grant.size) || grant.size < 0) {
    throw new Error("Invalid presigned part grant.");
  }
  const relative = grant.url.startsWith("/");
  if (!relative && !/^https?:\/\//i.test(grant.url)) throw new Error("Invalid presigned part target.");
  const parsed = new URL(grant.url, window.location.origin);
  if (parsed.username || parsed.password || parsed.hash) throw new Error("Invalid presigned part target.");
  if (relative && !parsed.pathname.startsWith("/api/files/transfer/")) throw new Error("Invalid presigned part target.");
  if (relative && parsed.origin !== window.location.origin) throw new Error("Invalid presigned part target.");
  if (!parsed.search) throw new Error("Invalid presigned part target.");
  if (!Number.isFinite(Date.parse(grant.expiresAt)) || Date.parse(grant.expiresAt) <= Date.now()) throw new Error("The presigned part has expired.");
  const headers: Record<string, string> = {};
  for (const [name, value] of Object.entries(grant.headers ?? {})) {
    const lower = name.toLowerCase();
    // The browser sets Content-Length from the Blob itself; setting it manually
    // is forbidden. Only Content-Type is an application-set header.
    if (lower === "content-length") continue;
    if (lower !== "content-type" || typeof value !== "string") throw new Error("Invalid presigned part headers.");
    headers[lower] = value;
  }
  return { url: parsed.toString(), headers };
}

function sendPresignedPart(target: { url: string; headers: Record<string, string> }, blob: Blob, signal?: AbortSignal,
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
    // A provider grant never receives application cookies or credentials.
    xhr.withCredentials = false;
    for (const [name, value] of Object.entries(target.headers)) xhr.setRequestHeader(name, value);
    xhr.upload.onprogress = (event) => {
      if (!settled && !signal?.aborted) onProgress?.({ loaded: event.loaded, total: blob.size });
    };
    xhr.onload = () => finish(xhr.status >= 200 && xhr.status < 300 ? undefined : new ApiError("presigned part transfer failed", xhr.status));
    xhr.onerror = () => finish(new ApiError("upload connection interrupted", 0));
    xhr.ontimeout = () => finish(new ApiError("upload request timed out", 0));
    xhr.onabort = () => finish(new DOMException("Upload canceled", "AbortError"));
    signal?.addEventListener("abort", abort, { once: true });
    try { xhr.send(blob); } catch (error) { finish(error); }
  });
}

async function uploadPresignedPart(grant: FileUploadPartGrant, blob: Blob, signal?: AbortSignal,
  onProgress?: (progress: UploadProgress) => void): Promise<void> {
  if (grant.size !== blob.size) throw new Error("The presigned part length changed.");
  const target = presignedPartTarget(grant);
  await sendPresignedPart(target, blob, signal, onProgress);
}

async function uploadRelayPart(id: string, number: number, blob: Blob, signal?: AbortSignal,
  onProgress?: (progress: UploadProgress) => void): Promise<FileUploadReceipt> {
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
      if (xhr.status === 401 || xhr.status === 403) forgetSession();
      try { finish(undefined, filePayload<FileUploadReceipt>(JSON.parse(xhr.responseText), xhr.status)); }
      catch (error) { finish(error instanceof ApiError ? error : httpError(xhr.status)); }
    };
    xhr.onerror = () => finish(new ApiError("upload connection interrupted", 0));
    xhr.ontimeout = () => finish(new ApiError("upload request timed out", 0));
    xhr.onabort = () => finish(new DOMException("Upload canceled", "AbortError"));
    signal?.addEventListener("abort", abort, { once: true });
    try { xhr.send(blob); } catch (error) { finish(error); }
  });
}

async function fetchDownloadGrant(selector: FileSelector, disposition: "inline" | "attachment", signal?: AbortSignal): Promise<FileDownloadGrant> {
  const result = await filePost<FileDownloadGrantCollection>("/downloads", { items: [typeof selector === "string" ? filePath(selector) : entrySelector(selector)], disposition }, undefined, signal);
  const grant = result.items[0];
  if (!grant || !grant.kind || grant.code) {
    throw new ApiError(grant?.detail ?? "The download grant was refused.", grant?.status ?? 502, grant ?? {});
  }
  return grant;
}

export function isFileOperation(result: FileResource | FileOperation | void): result is FileOperation {
  return Boolean(result && "state" in result && "id" in result);
}

/** Unified resources; byte access is through read grants or same-origin class C streaming. */
export const files = {
  metadata(selector: FileSelector, signal?: AbortSignal): Promise<FileResource> {
    return fileRequest(fileURL("", entrySelector(selector)), { signal });
  },
  list(selector: FileSelector, options: { limit?: number; cursor?: string } = {}): Promise<FileCollection> {
    return fileRequest(fileURL("/list", { limit: options.limit, cursor: options.cursor, ...entrySelector(selector) }));
  },
  search(options: FileSearchOptions): Promise<FileCollection> {
    const selected = Object.fromEntries(["id", "path"].filter((key) => hasOwn(options, key)).map((key) => [key, options[key as "id" | "path"]]));
    return fileRequest(fileURL("/search", { ...options, ...(Object.keys(selected).length ? entrySelector(selected as EntrySelector) : {}) }));
  },
  me(): Promise<FilePrincipal> {
    return fileRequest(fileURL("/me"));
  },
  capabilities(selector?: FileSelector): Promise<FileCapabilities> {
    return fileRequest(fileURL("/capabilities", selector === undefined ? {} : entrySelector(selector)));
  },
  /** Make the index equal the native tree; dryRun lists what would be removed. */
  sync(dryRun: boolean, signal?: AbortSignal): Promise<FileSyncReport> {
    return filePost("/sync", { dryRun }, undefined, signal);
  },
  createChangeSnapshot(filter: FileChangeFilter, signal?: AbortSignal): Promise<FileChangeSnapshot> {
    return filePost("/change-snapshots", { ...filter, target: entrySelector(filter.target) }, undefined, signal);
  },
  changeSnapshot(id: string, options: { cursor?: string; limit?: number } = {}, signal?: AbortSignal): Promise<FileChangeSnapshotPage> {
    return fileRequest(fileURL(`/change-snapshots/${encodeURIComponent(id)}`, options), { signal });
  },
  deleteChangeSnapshot(id: string, signal?: AbortSignal): Promise<void> {
    return fileRequest(fileURL(`/change-snapshots/${encodeURIComponent(id)}`), { method: "DELETE", signal });
  },
  changes(cursor: string, options: { limit?: number } = {}, signal?: AbortSignal): Promise<FileChangesPage> {
    return fileRequest(fileURL("/changes", { cursor, limit: options.limit }), { signal });
  },
  createSubscription(input: FileSubscriptionInput, signal?: AbortSignal): Promise<FileSubscription> {
    return filePost("/subscriptions", { ...input, target: entrySelector(input.target) }, undefined, signal);
  },
  subscriptions(signal?: AbortSignal): Promise<{ items: FileSubscription[] }> {
    return fileRequest(fileURL("/subscriptions"), { signal });
  },
  subscription(id: string, signal?: AbortSignal): Promise<FileSubscription> {
    return fileRequest(fileURL(`/subscriptions/${encodeURIComponent(id)}`), { signal });
  },
  renewSubscription(id: string, signal?: AbortSignal): Promise<FileSubscription> {
    return filePost(`/subscriptions/${encodeURIComponent(id)}/renew`, undefined, undefined, signal);
  },
  deleteSubscription(id: string, signal?: AbortSignal): Promise<void> {
    return fileRequest(fileURL(`/subscriptions/${encodeURIComponent(id)}`), { method: "DELETE", signal });
  },
  /** Connect EventSource with the session cookie; notification IDs never advance pull progress. */
  subscriptionEventsURL(id: string): string {
    return fileURL(`/subscriptions/${encodeURIComponent(id)}/events`);
  },
  operation(id: string, signal?: AbortSignal): Promise<FileOperation> {
    return fileRequest(fileURL(`/operations/${encodeURIComponent(id)}`), { signal });
  },
  cancelOperation(id: string, signal?: AbortSignal): Promise<FileOperation> {
    return filePost(`/operations/${encodeURIComponent(id)}/cancel`, undefined, undefined, signal);
  },
  contentURL(selector: FileSelector, disposition: "inline" | "attachment" = "attachment"): string {
    return fileURL("/content", { ...entrySelector(selector), disposition });
  },
  thumbnailURL(selector: FileSelector, width = 320, format = "webp"): string {
    return fileURL("/thumbnail", { ...entrySelector(selector), width, format });
  },
};

async function allFiles(
  load: (cursor?: string) => Promise<FileCollection>,
  firstPage?: Promise<FileCollection>,
): Promise<FileResource[]> {
  const items = new Map<string, FileResource>();
  let page = firstPage ? await firstPage : await load();
  for (;;) {
    for (const item of page.items) items.set(item.id, item);
    const cursor = page.nextCursor ?? undefined;
    if (!cursor) break;
    page = await load(cursor);
  }
  return [...items.values()];
}

function selectorKey(selected: EntrySelector): string {
  return selected.id !== undefined ? `id:${selected.id}` : `path:${selected.path}`;
}

function missingPathAsNull(error: unknown, selected: EntrySelector): null {
  // A path that does not exist is an empty listing the page renders as "not found"; an ID
  // selector keeps failing because a vanished ID is a real error for the caller.
  if (error instanceof ApiError && error.status === 404 && selected.id === undefined) return null;
  throw error;
}

/**
 * One directory read for the page: the parent metadata and the first page are independent
 * queries, so both start together instead of paying two round trips in sequence.
 */
async function loadListing(selected: EntrySelector): Promise<ListResult> {
  const nodeRequest = files.metadata(selected).catch((error: unknown) => missingPathAsNull(error, selected));
  const firstPage = files.list(selected);
  // A file selector only needs its metadata; the page request answers 409. Keep that rejection
  // from surfacing as unhandled even though the branch below never awaits it.
  firstPage.catch(() => undefined);

  const node = await nodeRequest;
  const absolute = node?.path ?? selected.path!;
  const breadcrumb: Crumb[] = [{ name: "", path: "/" }];
  let current = "";
  for (const name of absolute.split("/").filter(Boolean)) {
    current += `/${name}`;
    breadcrumb.push({ name, path: current });
  }
  return {
    path: absolute, parent: node?.parentPath ?? (absolute.slice(0, absolute.lastIndexOf("/")) || "/"),
    breadcrumb, node,
    items: node?.kind === "folder" ? await allFiles((cursor) => files.list(selected, { cursor }), firstPage) : [],
  };
}

/**
 * Listings warmed by hovering a row, so opening it does not wait for a round trip. An entry is
 * single use and expires quickly: a listing is a snapshot, and a mutation must not be followed
 * by a stale page.
 */
const listingPrefetches = new Map<string, { listing: Promise<ListResult>; expiresAt: number }>();
const listingPrefetchTTL = 10_000;
const listingPrefetchLimit = 24;

/** Session-bound state that must not survive a sign-out or an expired credential. */
function forgetSession(): void {
  csrfRequest = undefined;
  listingPrefetches.clear();
}

/** Warm one listing before the click that opens it; safe to call repeatedly. */
export function prefetchListing(selector: FileSelector): void {
  let selected: EntrySelector;
  try {
    selected = entrySelector(selector);
  } catch {
    return;
  }
  const key = selectorKey(selected);
  const known = listingPrefetches.get(key);
  if (known && known.expiresAt > Date.now()) {
    // Still useful: keep it, but move it to the end so the size cap drops colder entries.
    listingPrefetches.delete(key);
    listingPrefetches.set(key, known);
    return;
  }
  listingPrefetches.delete(key);
  const entry: { listing: Promise<ListResult>; expiresAt: number } = {
    listing: loadListing(selected), expiresAt: Number.POSITIVE_INFINITY,
  };
  entry.listing.then(
    () => { entry.expiresAt = Date.now() + listingPrefetchTTL; },
    () => { if (listingPrefetches.get(key) === entry) listingPrefetches.delete(key); },
  );
  listingPrefetches.set(key, entry);
  while (listingPrefetches.size > listingPrefetchLimit) {
    const oldest = listingPrefetches.keys().next().value;
    if (oldest === undefined) break;
    listingPrefetches.delete(oldest);
  }
}

function takeListingPrefetch(selected: EntrySelector): Promise<ListResult> | null {
  const key = selectorKey(selected);
  const entry = listingPrefetches.get(key);
  if (!entry) return null;
  listingPrefetches.delete(key);
  return entry.expiresAt > Date.now() ? entry.listing : null;
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
    changes: boolean;
    subscribe: boolean;
    expiresIn: number;
  }): Promise<CreatedConnection> {
    return post<CreatedConnection>("/api/connect/tokens", input);
  },

  /** Rewrite an existing connection's policy. The secret, name and kind cannot change. */
  updateConnection(
    id: string,
    input: { prefix: string; read: boolean; write: boolean; events: boolean; changes: boolean; subscribe: boolean; expiresIn: number },
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
    forgetSession();
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

  /** Owner cookie management view; this resource uses plain JSON and Problems. */
  audit(options: { cursor?: string; limit?: number } = {}, signal?: AbortSignal): Promise<FileAuditPage> {
    const query = new URLSearchParams();
    if (options.cursor !== undefined) query.set("cursor", options.cursor);
    if (options.limit !== undefined) query.set("limit", String(options.limit));
    const suffix = query.toString();
    return fileRequest(`/api/audit${suffix ? `?${suffix}` : ""}`, { signal });
  },

  syncFiles: files.sync,
  createChangeSnapshot: files.createChangeSnapshot,
  changeSnapshot: files.changeSnapshot,
  deleteChangeSnapshot: files.deleteChangeSnapshot,
  changes: files.changes,
  createSubscription: files.createSubscription,
  subscriptions: files.subscriptions,
  subscription: files.subscription,
  renewSubscription: files.renewSubscription,
  deleteSubscription: files.deleteSubscription,
  subscriptionEventsURL: files.subscriptionEventsURL,

  metadata: files.metadata,
  fileMe: files.me,
  capabilities: files.capabilities,
  getOperation: files.operation,
  cancelOperation: files.cancelOperation,
  contentURL: files.contentURL,
  thumbnailURL: files.thumbnailURL,

  /** App navigation composed from resources, rather than a legacy server listing DTO. */
  async list(selector: FileSelector): Promise<ListResult> {
    const selected = entrySelector(selector);
    return takeListingPrefetch(selected) ?? loadListing(selected);
  },

  async search(keyword: string, selector?: FileSelector): Promise<SearchResult> {
    const selected = selector === undefined ? {} : entrySelector(selector);
    return { items: await allFiles((cursor) => files.search({ q: keyword, cursor, ...selected })) };
  },

  mkdir(destination: string | Destination, parents = false): Promise<FileResource | FileOperation> {
    if (typeof destination === "string") return filePost("", { kind: "folder", path: filePath(destination), parents });
    const selected = fileDestination(destination);
    if (parents && hasOwn(selected, "parent")) return invalidSelector();
    return filePost("", { kind: "folder", destination: selected, parents });
  },

  move(from: FileSelector, to: string | Destination, ifRevision: string): Promise<FileResource | FileOperation> {
    const source = typeof from === "string" ? filePath(from) : entrySelector(from);
    const destination = typeof to === "string" ? filePath(to) : fileDestination(to);
    return filePost("/move", { from: source, to: destination, ifRevision }, { Prefer: "respond-async" });
  },

  copy(from: FileSelector, to: string | Destination, ifSourceRevision: string): Promise<FileResource | FileOperation> {
    const source = typeof from === "string" ? filePath(from) : entrySelector(from);
    const destination = typeof to === "string" ? filePath(to) : fileDestination(to);
    return filePost("/copy", { from: source, to: destination, ifSourceRevision }, { Prefer: "respond-async" });
  },

  deletePath(selector: FileSelector, recursive = false, ifRevision?: string): Promise<FileOperation | void> {
    return fileRequest(fileURL("", { ...entrySelector(selector), recursive: recursive ? "true" : undefined }), {
      method: "DELETE", headers: { Prefer: "respond-async", ...(ifRevision ? { "If-Revision": ifRevision } : {}) },
    });
  },

  createUpload(input: FileUploadInput, key: string, signal?: AbortSignal): Promise<FileUploadSession> {
    return filePost("/uploads", uploadInput(input), { "Idempotency-Key": key }, signal);
  },

  /** Class C only: one immutable part through the application, with a durable receipt. */
  uploadPart(id: string, number: number, blob: Blob, signal?: AbortSignal, onProgress?: (progress: UploadProgress) => void): Promise<FileUploadReceipt> {
    return uploadRelayPart(id, number, blob, signal, onProgress);
  },

  /** Class A: one signed provider part. There is no application acknowledgement. */
  uploadPresignedPart(grant: FileUploadPartGrant, blob: Blob, signal?: AbortSignal, onProgress?: (progress: UploadProgress) => void): Promise<void> {
    return uploadPresignedPart(grant, blob, signal, onProgress);
  },

  async signUploadParts(id: string, numbers: number[], signal?: AbortSignal): Promise<FileUploadPartGrant[]> {
    const result = await filePost<{ parts: FileUploadPartGrant[] }>(`/uploads/${encodeURIComponent(id)}/parts/sign`, { numbers }, undefined, signal);
    if (!Array.isArray(result.parts)) throw new Error("The signed part list is invalid.");
    return result.parts;
  },

  uploadStatus(id: string, signal?: AbortSignal): Promise<FileUploadSession> {
    return fileRequest(fileURL(`/uploads/${encodeURIComponent(id)}`), { signal });
  },

  uploadParts(id: string, cursor?: string, signal?: AbortSignal): Promise<FileUploadPartsPage> {
    return fileRequest(fileURL(`/uploads/${encodeURIComponent(id)}/parts`, { cursor }), { signal });
  },

  downloadGrants(items: FileSelector[], disposition: "inline" | "attachment" = "attachment", signal?: AbortSignal): Promise<FileDownloadGrantCollection> {
    return filePost("/downloads", { items: items.map((item) => typeof item === "string" ? filePath(item) : entrySelector(item)), disposition }, undefined, signal);
  },

  downloadGrant(selector: FileSelector, disposition: "inline" | "attachment" = "attachment", signal?: AbortSignal): Promise<FileDownloadGrant> {
    return fetchDownloadGrant(selector, disposition, signal);
  },

  /** Resolve one read grant into a URL a browser media element can use. */
  async downloadURL(selector: FileSelector, disposition: "inline" | "attachment" = "inline", signal?: AbortSignal): Promise<string> {
    const grant = await fetchDownloadGrant(selector, disposition, signal);
    if (grant.kind === "url" && grant.url) return grant.url;
    if (grant.kind === "bearer" && grant.url) {
      const response = await fetch(grant.url, { headers: grant.headers ?? {}, credentials: "omit", signal });
      if (!response.ok) throw httpError(response.status);
      return URL.createObjectURL(await response.blob());
    }
    throw new Error("The read grant has no usable URL.");
  },

  /** Fetch a download grant, then hand the representation to the browser. */
  async download(selector: FileSelector, signal?: AbortSignal): Promise<void> {
    const grant = await fetchDownloadGrant(selector, "attachment", signal);
    if (grant.kind === "url" && grant.url) { window.location.assign(grant.url); return; }
    if (grant.kind === "bearer" && grant.url) {
      const response = await fetch(grant.url, { headers: grant.headers ?? {}, credentials: "omit", signal });
      if (!response.ok) throw httpError(response.status);
      const url = URL.createObjectURL(await response.blob());
      const link = document.createElement("a");
      link.href = url;
      link.download = "";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
      return;
    }
    throw new Error("The read grant has no usable URL.");
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
  downloadHref(selector: FileSelector): string {
    return files.contentURL(selector, "attachment");
  },
};
