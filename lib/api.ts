import { NextResponse } from "next/server";
import { PathError } from "./paths";
import { HttpError } from "./errors";

export { HttpError };

/** COS / STS 抛的是普通对象，字段大小写还不统一，这里统一抹平 */
function errorText(error: unknown): string {
  if (error instanceof Error) return error.message;
  if (error && typeof error === "object") {
    const value = error as Record<string, unknown>;
    for (const key of ["message", "Message", "error", "Error", "code", "Code"]) {
      const hit = value[key];
      if (typeof hit === "string" && hit) return hit;
    }
  }
  return "未知错误";
}

export async function handle<T>(fn: () => Promise<T>): Promise<NextResponse> {
  try {
    return NextResponse.json({ ok: true, data: await fn() });
  } catch (error) {
    if (error instanceof PathError) {
      return NextResponse.json({ ok: false, error: error.message }, { status: 400 });
    }
    if (error instanceof HttpError) {
      return NextResponse.json({ ok: false, error: error.message }, { status: error.status });
    }
    console.error("[api]", error);
    return NextResponse.json({ ok: false, error: errorText(error) }, { status: 500 });
  }
}
