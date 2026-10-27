/** 和 Go 后端通信的唯一入口。接口统一返回 {ok, data|error} 信封。 */

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
    this.name = "ApiError";
  }
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

/** 卡片内容。type 决定哪些字段有值，见 docs/design/events.md §3。 */
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

/** 时间线上的一条事件。 */
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

export interface AccountSummary {
  user: AuthUser;
  legacyCount: number;
  saveLinks: SaveLink[];
  activity: ActivityItem[];
}

export interface UploadSignResult {
  bucket: string;
  region: string;
  key: string;
  credentials: {
    tmpSecretId: string;
    tmpSecretKey: string;
    sessionToken: string;
    startTime: number;
    expiredTime: number;
  };
}

export interface CodeResult {
  expiresIn: number;
  devCode?: string;
}

/** 响应不是后端的信封时：多半是后端没起，或 /api 代理指到的端口上跑着别的服务。 */
function httpError(status: number): ApiError {
  return new ApiError(
    `unexpected response from the API (HTTP ${status}); is the backend running?`,
    status,
  );
}

/** 解包信封；形状不对时给出能定位问题的错误，而不是把对象拼成 "[object Object]"。 */
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
    // 后端没起时 Vite 代理会返回 HTML 错误页，这里不能当成信封去读。
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

export const api = {
  async me(): Promise<AuthUser | null> {
    const data = await request<{ user: AuthUser | null }>("/api/auth/me");
    return data.user;
  },

  async account(): Promise<AccountSummary> {
    return request<AccountSummary>("/api/account");
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

  signUpload(path: string, size: number, overwrite = false): Promise<UploadSignResult> {
    return post<UploadSignResult>("/api/fs/upload/sign", { path, size, overwrite });
  },

  /** 传完字节后让服务端校验对象并落索引；返回新文件节点，可直接插进列表。 */
  async confirmUpload(
    path: string,
    mime?: string,
    sha256?: string,
    modifiedAt?: number,
  ): Promise<Item> {
    const data = await post<{ node: Item }>("/api/fs/upload/confirm", {
      path,
      mime,
      sha256,
      modifiedAt,
    });
    return data.node;
  },

  async mkdir(path: string): Promise<void> {
    await post("/api/fs/mkdir", { path });
  },

  async rename(from: string, to: string): Promise<void> {
    await post("/api/fs/rename", { from, to });
  },

  async deletePath(path: string): Promise<void> {
    await post("/api/fs/delete", { path });
  },

  /** 时间线：一层扁平的事件流，keyset 分页。 */
  events(cursor?: string, limit = 20): Promise<EventPage> {
    const params = new URLSearchParams();
    if (cursor) params.set("cursor", cursor);
    params.set("limit", String(limit));
    return request<EventPage>(`/api/timeline?${params.toString()}`);
  },

  streamStatus(): Promise<StreamStatus> {
    return request<StreamStatus>("/api/timeline/status");
  },

  /** 302 到 COS 预签名地址；直接当 <a href> 用。 */
  downloadHref(path: string): string {
    return `/api/fs/download?path=${encodeURIComponent(path)}`;
  },
};
