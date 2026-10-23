import { handle } from "@/lib/api";
import { getSessionUser } from "@/lib/auth";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET() {
  return handle(async () => ({ user: await getSessionUser() }));
}
