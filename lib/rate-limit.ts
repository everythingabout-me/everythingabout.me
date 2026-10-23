import type { NextRequest } from "next/server";
import { HttpError } from "./errors";

interface Bucket {
  count: number;
  resetAt: number;
}

const buckets = new Map<string, Bucket>();
const MAX_KEYS = 10_000;

/**
 * 单进程内存限流，够挡住暴力猜密码和批量注册。
 * 多实例部署时每个实例各算各的，真要严格限流得换 Redis。
 */
export function rateLimit(
  key: string,
  limit: number,
  windowMs: number,
): { ok: boolean; retryAfter: number } {
  const now = Date.now();
  const bucket = buckets.get(key);

  if (!bucket || bucket.resetAt <= now) {
    if (buckets.size >= MAX_KEYS) {
      for (const [candidate, value] of buckets) {
        if (value.resetAt <= now) buckets.delete(candidate);
      }
      if (buckets.size >= MAX_KEYS) buckets.clear();
    }
    buckets.set(key, { count: 1, resetAt: now + windowMs });
    return { ok: true, retryAfter: 0 };
  }

  if (bucket.count >= limit) {
    return { ok: false, retryAfter: Math.max(1, Math.ceil((bucket.resetAt - now) / 1000)) };
  }

  bucket.count += 1;
  return { ok: true, retryAfter: 0 };
}

export function enforceRateLimit(key: string, limit: number, windowMs: number): void {
  const result = rateLimit(key, limit, windowMs);
  if (!result.ok) {
    throw new HttpError(429, `操作太频繁，请 ${result.retryAfter} 秒后再试`);
  }
}

export function clientIp(request: NextRequest): string {
  const forwarded = request.headers.get("x-forwarded-for");
  if (forwarded) return forwarded.split(",")[0]!.trim();
  return request.headers.get("x-real-ip")?.trim() || "local";
}
