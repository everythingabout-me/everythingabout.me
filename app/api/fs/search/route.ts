import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { query } from "@/lib/db";
import type { NodeRow } from "@/lib/nodes";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

function escapeLike(value: string): string {
  return value.replace(/[\\%_]/g, (char) => "\\" + char);
}

export async function GET(request: NextRequest) {
  return handle(async () => {
    const keyword = (request.nextUrl.searchParams.get("q") ?? "").trim();
    if (!keyword) return { items: [] };

    const pattern = `%${escapeLike(keyword)}%`;
    const { rows } = await query<NodeRow>(
      `select id, path, parent, name, is_dir, size, mime, updated_at, meta
         from nodes
        where deleted_at is null
          and (name ilike $1 or path ilike $1)
        order by is_dir desc, lower(name) asc
        limit 200`,
      [pattern],
    );

    return { items: rows.map((row) => ({ ...row, size: Number(row.size) })) };
  });
}
