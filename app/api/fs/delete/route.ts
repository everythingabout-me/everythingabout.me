import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { HttpError } from "@/lib/errors";
import { assertNotRoot, softDelete } from "@/lib/nodes";
import { normalizePath } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    const body = (await request.json()) as { path?: string };
    const path = normalizePath(body.path);
    assertNotRoot(path);

    // 软删除：只打标记，对象留在原地；purge 任务负责真正删对象
    const count = await softDelete(path);
    if (count === 0) throw new HttpError(404, "没有可删除的文件");
    return { deleted: count };
  });
}
