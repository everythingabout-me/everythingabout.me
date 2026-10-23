import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { requireUser } from "@/lib/auth";
import { emailDeliveryConfigured, sendVerificationCode } from "@/lib/email";
import {
  EMAIL_CODE_TTL_SECONDS,
  issueEmailCode,
  revokeEmailCodes,
} from "@/lib/email-codes";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin, readJson } from "@/lib/http";
import { clientIp, enforceRateLimit } from "@/lib/rate-limit";
import { findUserByEmail } from "@/lib/users";
import { normalizeEmail } from "@/lib/validation";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/** 给「新邮箱」发验证码；要求已登录 */
export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const current = await requireUser();
    if (process.env.NODE_ENV === "production" && !emailDeliveryConfigured()) {
      throw new HttpError(503, "邮件服务未配置，暂时无法发送验证码");
    }

    const ip = clientIp(request);
    enforceRateLimit(`email-change-code:ip:${ip}`, 30, 60 * 60 * 1000);
    enforceRateLimit(`email-change-code:user:${current.id}`, 10, 60 * 60 * 1000);

    const body = await readJson<{ email?: unknown }>(request);
    const email = normalizeEmail(body.email);
    if (email === current.email) throw new HttpError(400, "新邮箱和当前邮箱相同");

    if (await findUserByEmail(email)) {
      throw new HttpError(409, "这个邮箱已被其他账户使用");
    }

    enforceRateLimit(`email-change-code:email:${email}`, 5, 60 * 60 * 1000);
    enforceRateLimit(`email-change-code:cooldown:${email}`, 1, 60 * 1000);

    const code = await issueEmailCode(email, "change_email", ip);
    try {
      const result = await sendVerificationCode(email, code);
      return { expiresIn: EMAIL_CODE_TTL_SECONDS, devCode: result.devCode };
    } catch (error) {
      await revokeEmailCodes(email, "change_email");
      throw error;
    }
  });
}
