import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { hashPassword, requireUser, verifyPassword } from "@/lib/auth";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin, readJson } from "@/lib/http";
import { enforceRateLimit } from "@/lib/rate-limit";
import { findUserById, setUserPassword, toAuthUser } from "@/lib/users";
import { validatePassword } from "@/lib/validation";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/** 设置或修改密码；账号已有密码时必须先验证当前密码 */
export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const current = await requireUser();
    enforceRateLimit(`password:${current.id}`, 10, 60 * 60 * 1000);

    const body = await readJson<{ password?: unknown; currentPassword?: unknown }>(request);
    const password = validatePassword(body.password);

    const row = await findUserById(current.id);
    if (!row) throw new HttpError(401, "请先登录");

    if (row.password_hash) {
      const currentPassword =
        typeof body.currentPassword === "string" ? body.currentPassword : "";
      if (!currentPassword) throw new HttpError(400, "请输入当前密码");
      if (currentPassword.length > 128) throw new HttpError(401, "当前密码不正确");
      const valid = await verifyPassword(currentPassword, row.password_hash);
      if (!valid) throw new HttpError(401, "当前密码不正确");
    }

    const updated = await setUserPassword(row.id, await hashPassword(password));
    return { user: toAuthUser(updated) };
  });
}

/** 移除密码，之后只能靠邮箱验证码登录；需要当前密码 */
export async function DELETE(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const current = await requireUser();
    enforceRateLimit(`password-remove:${current.id}`, 10, 60 * 60 * 1000);

    const row = await findUserById(current.id);
    if (!row) throw new HttpError(401, "请先登录");
    if (!row.password_hash) throw new HttpError(400, "这个账号还没有设置密码");

    const body = await readJson<{ currentPassword?: unknown }>(request);
    const currentPassword =
      typeof body.currentPassword === "string" ? body.currentPassword : "";
    if (!currentPassword) throw new HttpError(400, "请输入当前密码");
    if (currentPassword.length > 128) throw new HttpError(401, "当前密码不正确");

    const valid = await verifyPassword(currentPassword, row.password_hash);
    if (!valid) throw new HttpError(401, "当前密码不正确");

    const updated = await setUserPassword(row.id, null);
    return { user: toAuthUser(updated) };
  });
}
