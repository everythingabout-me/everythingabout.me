import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { HttpError } from "@/lib/errors";
import { storage } from "@/lib/cos";
import { query } from "@/lib/db";
import { ensureParents, getLiveNode } from "@/lib/nodes";
import { dirKeyFor, nameOf, normalizePath, parentOf } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    const body = (await request.json()) as { path?: string };
    const path = normalizePath(body.path);
    if (!path) throw new HttpError(400, "目录路径不能为空");

    const existing = await getLiveNode(path);
    if (existing) throw new HttpError(409, "同名文件或目录已存在");

    await ensureParents(path);
    await storage().putEmpty(dirKeyFor(path));

    const { rows } = await query(
      `insert into nodes (key, path, parent, name, is_dir)
       values ($1, $2, $3, $4, true)
       on conflict (path) where deleted_at is null do nothing
       returning *`,
      [dirKeyFor(path), path, parentOf(path), nameOf(path)],
    );

    return { node: rows[0] ?? null };
  });
}
