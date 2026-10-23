import type { NextRequest } from "next/server";
import { HttpError } from "./errors";

/**
 * SameSite=Lax 已经挡住绝大多数跨站 POST；这里再比一次 Origin/Host，
 * 给 cookie 同站的子域也加一道闸。没有 Origin 的请求（curl、脚本）放行。
 */
export function assertSameOrigin(request: NextRequest): void {
  const origin = request.headers.get("origin");
  if (!origin) return;

  let host: string;
  try {
    host = new URL(origin).host;
  } catch {
    throw new HttpError(403, "请求来源不合法");
  }

  const requestHost =
    request.headers.get("x-forwarded-host")?.split(",")[0]?.trim() ||
    request.headers.get("host");
  if (requestHost && host !== requestHost) {
    throw new HttpError(403, "跨站请求被拒绝");
  }
}

export async function readJson<T>(request: NextRequest): Promise<T> {
  try {
    return (await request.json()) as T;
  } catch {
    throw new HttpError(400, "请求体不是合法 JSON");
  }
}
