import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { requireUser } from "@/lib/auth";
import { HttpError } from "@/lib/errors";
import { env } from "@/lib/env";
import { storage } from "@/lib/cos";
import { evictTrashedAt, getLiveNode } from "@/lib/nodes";
import { assertSameOrigin } from "@/lib/http";
import { keyFor, normalizePath } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

const MAX_FILE_SIZE = 48 * 1024 ** 3; // COS 单对象上限 48.82TB，这里给个保守值

export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const user = await requireUser();
    const body = (await request.json()) as { path?: string; size?: number };
    const path = normalizePath(body.path);
    const size = Number(body.size ?? 0);

    if (!path) throw new HttpError(400, "路径不能为空");
    if (!Number.isFinite(size) || size <= 0) throw new HttpError(400, "文件大小不合法");
    if (size > MAX_FILE_SIZE) throw new HttpError(413, "单个文件超过 48TB");

    const existing = await getLiveNode(path, user.id);
    if (existing?.is_dir) throw new HttpError(409, "同名目录已存在");

    await evictTrashedAt(path, user.id);
    const key = keyFor(path, user.id);
    const credentials = await storage().temporaryCredentials(key, 3600);

    return {
      bucket: env.cos.bucket,
      region: env.cos.region,
      key,
      credentials,
    };
  });
}
