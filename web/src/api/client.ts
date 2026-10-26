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


export interface StreamThumb {
  path: string;
  url?: string;
  name?: string;
  video?: boolean;
  duration_ms?: number;
}

export interface StreamSeries {
  metric: string;
  label: string;
  unit?: string;
  days: number[];
  values: number[];
  latest: number;
  min: number;
  max: number;
}

export interface StreamItem {
  type: "moment" | "gap" | "ambient";
  id?: string;
  layout?: "single" | "mosaic" | "hero" | "series";
  title?: string;
  subtitle?: string;
  folder?: string;
  starts_at?: string;
  ends_at?: string;
  item_count?: number;
  more?: number;
  cover?: StreamThumb;
  grid?: StreamThumb[];
  score?: number;
  label?: string;
  from?: string;
  to?: string;
  series?: StreamSeries[];
}

export interface StreamPeriod {
  month: string;
  file_count: number;
  moment_count: number;
  coverage: "ready" | "indexing" | "empty";
  generated_at?: string;
  items: StreamItem[];
}

export interface StreamStatus {
  enabled: boolean;
  pending: number;
  indexed_files: number;
  earliest_month?: string;
  latest_month?: string;
  last_generated?: string;
}

export interface StreamFeed {
  periods: StreamPeriod[];
  next?: string;
  status: StreamStatus;
}

export interface AccountSummary {
  user: AuthUser;
  legacyCount: number;
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

type Envelope<T> = { ok: true; data: T } | { ok: false; error: string };

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: {
      ...(init?.body ? { "content-type": "application/json" } : {}),
      ...init?.headers,
    },
  });

  let payload: Envelope<T>;
  try {
    payload = (await response.json()) as Envelope<T>;
  } catch {
    throw new ApiError(`请求失败（HTTP ${response.status}）`, response.status);
  }
  if (!payload.ok) {
    throw new ApiError(payload.error || `请求失败（HTTP ${response.status}）`, response.status);
  }
  return payload.data;
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

  async confirmUpload(path: string, mime?: string, sha256?: string, modifiedAt?: number): Promise<void> {
    await post("/api/fs/upload/confirm", { path, mime, sha256, modifiedAt });
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

  stream(before?: string, months = 2): Promise<StreamFeed> {
    const params = new URLSearchParams();
    if (before) params.set("before", before);
    params.set("months", String(months));
    return request<StreamFeed>(`/api/timeline?${params.toString()}`);
  },

  streamStatus(): Promise<StreamStatus> {
    return request<StreamStatus>("/api/timeline/status");
  },

  /** 302 到 COS 预签名地址；直接当 <a href> 用。 */
  downloadHref(path: string): string {
    return `/api/fs/download?path=${encodeURIComponent(path)}`;
  },
};
