import { NextResponse, type NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { HttpError } from "@/lib/errors";
import { storage } from "@/lib/cos";
import { getLiveNode } from "@/lib/nodes";
import { keyFor, normalizePath } from "@/lib/paths";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function GET(request: NextRequest) {
  const params = request.nextUrl.searchParams;
  const asJson = params.get("json") === "1";

  if (asJson) {
    return handle(async () => {
      const path = normalizePath(params.get("path"));
      const node = await getLiveNode(path);
      if (!node || node.is_dir) throw new HttpError(404, "文件不存在");
      const url = await storage().getUrl(keyFor(node.path), {
        downloadName: node.name,
        contentType: node.mime ?? undefined,
      });
      return { url, name: node.name, size: Number(node.size) };
    });
  }

  try {
    const path = normalizePath(params.get("path"));
    const node = await getLiveNode(path);
    if (!node || node.is_dir) {
      return NextResponse.json({ ok: false, error: "文件不存在" }, { status: 404 });
    }
    const url = await storage().getUrl(keyFor(node.path), {
      downloadName: node.name,
      contentType: node.mime ?? undefined,
    });
    return NextResponse.redirect(url, 302);
  } catch (error) {
    return NextResponse.json(
      { ok: false, error: error instanceof Error ? error.message : "未知错误" },
      { status: 500 },
    );
  }
}
