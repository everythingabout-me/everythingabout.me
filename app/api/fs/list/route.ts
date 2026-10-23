import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { requireUser } from "@/lib/auth";
import { query } from "@/lib/db";
import { normalizePath, parentOf } from "@/lib/paths";
import type { NodeRow } from "@/lib/nodes";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const PAGE_LIMIT = 5000;

export async function GET(request: NextRequest) {
  return handle(async () => {
    const user = await requireUser();
    const path = normalizePath(request.nextUrl.searchParams.get("path"));
    const { rows } = await query<NodeRow>(
      `select id, path, parent, name, is_dir, size, mime, updated_at, meta
         from nodes
        where owner_id = $1
          and deleted_at is null
          and parent = $2
        order by is_dir desc, lower(name) asc
        limit $3`,
      [user.id, path, PAGE_LIMIT],
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
