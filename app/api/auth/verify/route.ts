import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { createSession, setSessionCookie } from "@/lib/auth";
import { isUniqueViolation } from "@/lib/db";
import { consumeEmailCode } from "@/lib/email-codes";
import { env } from "@/lib/env";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin, readJson } from "@/lib/http";
import { clientIp, enforceRateLimit } from "@/lib/rate-limit";
import {
  createUser,
  findUserByEmail,
  toAuthUser,
  touchLastLogin,
} from "@/lib/users";
import { normalizeEmail } from "@/lib/validation";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/** 验证码登录；邮箱不存在且注册开放时，自动创建一个无密码账号 */
export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const ip = clientIp(request);
    enforceRateLimit(`verify:ip:${ip}`, 30, 15 * 60 * 1000);

    const body = await readJson<{ email?: unknown; code?: unknown }>(request);
    const email = normalizeEmail(body.email);
    const code = typeof body.code === "string" ? body.code.trim() : "";
    if (!/^\d{6}$/.test(code)) throw new HttpError(400, "请输入 6 位验证码");

    enforceRateLimit(`verify:email:${email}`, 10, 15 * 60 * 1000);

    let user = await findUserByEmail(email);
    const existing = Boolean(user);
    if (!user && !env.registrationOpen) {
      throw new HttpError(403, "注册已关闭");
    }

    // 兼容旧流程发出去的 register 验证码
    await consumeEmailCode(email, ["signin", "register"], code);

    if (!user) {
      try {
        user = await createUser(email, null);
      } catch (error) {
        if (!isUniqueViolation(error)) throw error;
        user = await findUserByEmail(email);
      }
    }
    if (!user) throw new HttpError(500, "创建账号失败，请重试");

    await touchLastLogin(user.id);
    const { token, expiresAt } = await createSession(user.id, {
      userAgent: request.headers.get("user-agent"),
      ip,
    });
    await setSessionCookie(token, expiresAt);

    return { user: toAuthUser(user), created: !existing };
  });
}
