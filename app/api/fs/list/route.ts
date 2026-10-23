import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { query } from "@/lib/db";
import { normalizePath, parentOf } from "@/lib/paths";
import type { NodeRow } from "@/lib/nodes";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const PAGE_LIMIT = 5000;

export async function GET(request: NextRequest) {
  return handle(async () => {
    const path = normalizePath(request.nextUrl.searchParams.get("path"));
    const { rows } = await query<NodeRow>(
      `select id, path, parent, name, is_dir, size, mime, updated_at, meta
         from nodes
        where deleted_at is null and parent = $1
        order by is_dir desc, lower(name) asc
        limit $2`,
      [path, PAGE_LIMIT],
    );

    const segments = path ? path.split("/") : [];
    const breadcrumb = [{ name: "全部文件", path: "" }];
    let cursor = "";
    for (const segment of segments) {
      cursor = cursor ? `${cursor}/${segment}` : segment;
      breadcrumb.push({ name: segment, path: cursor });
    }

    return {
      path,
      parent: parentOf(path),
      breadcrumb,
      truncated: rows.length >= PAGE_LIMIT,
      items: rows.map((row) => ({ ...row, size: Number(row.size) })),
    };
  });
}
