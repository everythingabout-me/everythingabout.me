import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import {
  burnPasswordCheck,
  createSession,
  setSessionCookie,
  verifyPassword,
} from "@/lib/auth";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin, readJson } from "@/lib/http";
import { clientIp, enforceRateLimit } from "@/lib/rate-limit";
import { findUserByEmail, toAuthUser, touchLastLogin } from "@/lib/users";
import { normalizeEmail } from "@/lib/validation";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const ip = clientIp(request);
    enforceRateLimit(`login:${ip}`, 30, 15 * 60 * 1000);

    const body = await readJson<{ email?: unknown; password?: unknown }>(request);
    const email = normalizeEmail(body.email);
    const password = typeof body.password === "string" ? body.password : "";
    if (!password) throw new HttpError(400, "请输入密码");
    if (password.length > 128) throw new HttpError(401, "邮箱或密码不正确");

    enforceRateLimit(`login:${ip}:${email}`, 10, 15 * 60 * 1000);

    const user = await findUserByEmail(email);
    if (!user) {
      await burnPasswordCheck(password);
      throw new HttpError(401, "邮箱或密码不正确");
    }
    if (!user.password_hash) {
      throw new HttpError(401, "这个账号还没有设置密码，请用邮箱验证码登录");
    }

    const valid = await verifyPassword(password, user.password_hash);
    if (!valid) throw new HttpError(401, "邮箱或密码不正确");

    await touchLastLogin(user.id);
    const { token, expiresAt } = await createSession(user.id, {
      userAgent: request.headers.get("user-agent"),
      ip,
    });
    await setSessionCookie(token, expiresAt);

    return { user: toAuthUser(user) };
  });
}
