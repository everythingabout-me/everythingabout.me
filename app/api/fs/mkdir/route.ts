import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { requireUser } from "@/lib/auth";
import { storage } from "@/lib/cos";
import { query } from "@/lib/db";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin } from "@/lib/http";
import { ensureParents, evictTrashedAt, getLiveNode } from "@/lib/nodes";
import { dirKeyFor, nameOf, normalizePath, parentOf } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const user = await requireUser();
    const body = (await request.json()) as { path?: string };
    const path = normalizePath(body.path);
    if (!path) throw new HttpError(400, "目录路径不能为空");

    const existing = await getLiveNode(path, user.id);
    if (existing) throw new HttpError(409, "同名文件或目录已存在");

    await ensureParents(path, user.id);
    await evictTrashedAt(path, user.id, true);
    const key = dirKeyFor(path, user.id);
    await storage().putEmpty(key);

    const { rows } = await query(
      `insert into nodes (owner_id, key, path, parent, name, is_dir)
       values ($1, $2, $3, $4, $5, true)
       on conflict (owner_id, path) where deleted_at is null do nothing
       returning *`,
      [user.id, key, path, parentOf(path), nameOf(path)],
    );

    return { node: rows[0] ?? null };
  });
}
