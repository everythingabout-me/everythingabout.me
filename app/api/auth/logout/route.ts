import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { logout } from "@/lib/auth";
import { assertSameOrigin } from "@/lib/http";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    await logout();
    return { ok: true };
  });
}
