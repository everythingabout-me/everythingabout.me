import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { HttpError } from "@/lib/errors";
import { storage } from "@/lib/cos";
import { ensureParents, getLiveNode, upsertFile } from "@/lib/nodes";
import { keyFor, normalizePath } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    const body = (await request.json()) as {
      path?: string;
      mime?: string;
      sha256?: string;
    };
    const path = normalizePath(body.path);
    if (!path) throw new HttpError(400, "路径不能为空");

    const existing = await getLiveNode(path);
    if (existing?.is_dir) throw new HttpError(409, "同名目录已存在");

    const info = await storage().head(keyFor(path));
    if (!info) throw new HttpError(400, "对象不存在，上传可能未完成");

    await ensureParents(path);
    const meta: Record<string, unknown> = {};
    if (body.sha256) meta.sha256 = body.sha256;
    if (body.mime) meta.mime = body.mime;

    const node = await upsertFile({
      path,
      size: info.size,
      mime: body.mime ?? null,
      etag: info.etag ?? null,
      storageClass: info.storageClass ?? null,
      meta,
    });

    return { node: { ...node, size: Number(node.size) } };
  });
}
