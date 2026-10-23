import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { requireUser } from "@/lib/auth";
import { query } from "@/lib/db";
import type { NodeRow } from "@/lib/nodes";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

function escapeLike(value: string): string {
  return value.replace(/[\\%_]/g, (char) => "\\" + char);
}

export async function GET(request: NextRequest) {
  return handle(async () => {
    const user = await requireUser();
    const keyword = (request.nextUrl.searchParams.get("q") ?? "").trim();
    if (!keyword) return { items: [] };

    const pattern = `%${escapeLike(keyword)}%`;
    const { rows } = await query<NodeRow>(
      `select id, path, parent, name, is_dir, size, mime, updated_at, meta
         from nodes
        where owner_id = $1
          and deleted_at is null
          and (name ilike $2 or path ilike $2)
        order by is_dir desc, lower(name) asc
        limit 200`,
      [user.id, pattern],
    );

    return { items: rows.map((row) => ({ ...row, size: Number(row.size) })) };
  });
}
