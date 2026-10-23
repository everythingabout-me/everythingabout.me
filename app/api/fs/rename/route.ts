import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { HttpError } from "@/lib/errors";
import { storage } from "@/lib/cos";
import { query } from "@/lib/db";
import { ensureParents, evictTrashedAt, getLiveNode } from "@/lib/nodes";
import { assertNotRoot } from "@/lib/nodes";
import { keyFor, nameOf, normalizePath, parentOf } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    const body = (await request.json()) as { from?: string; to?: string };
    const from = normalizePath(body.from);
    const to = normalizePath(body.to);
    assertNotRoot(from);
    assertNotRoot(to);
    if (from === to) throw new HttpError(400, "路径没有变化");

    const source = await getLiveNode(from);
    if (!source) throw new HttpError(404, "源文件不存在");
    if (source.is_dir) {
      throw new HttpError(
        400,
        "目录重命名需要对每个对象做服务端复制，TB 级目录会很慢。请用 coscli 改名后再跑 reconcile。",
      );
    }

    const collision = await getLiveNode(to);
    if (collision) throw new HttpError(409, "目标路径已存在");

    await ensureParents(to);
    await evictTrashedAt(to);
    await storage().copy(keyFor(from), keyFor(to));
    await storage().remove(keyFor(from));

    const { rows } = await query(
      `update nodes
          set key = $1, path = $2, parent = $3, name = $4, updated_at = now()
        where id = $5
        returning *`,
      [keyFor(to), to, parentOf(to), nameOf(to), source.id],
    );

    return { node: rows[0] };
  });
}
