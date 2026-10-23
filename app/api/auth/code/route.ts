import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { emailDeliveryConfigured, sendVerificationCode } from "@/lib/email";
import {
  EMAIL_CODE_TTL_SECONDS,
  issueEmailCode,
  revokeEmailCodes,
} from "@/lib/email-codes";
import { env } from "@/lib/env";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin, readJson } from "@/lib/http";
import { clientIp, enforceRateLimit } from "@/lib/rate-limit";
import { findUserByEmail } from "@/lib/users";
import { normalizeEmail } from "@/lib/validation";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/**
 * 登录 / 注册共用的验证码发送接口。
 * 邮箱不存在时，只要注册开放，也会发码；验证通过后自动创建无密码账号。
 */
export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    if (process.env.NODE_ENV === "production" && !emailDeliveryConfigured()) {
      throw new HttpError(503, "邮件服务未配置，暂时无法发送验证码");
    }

    const ip = clientIp(request);
    enforceRateLimit(`signin-code:ip:${ip}`, 30, 60 * 60 * 1000);

    const body = await readJson<{ email?: unknown }>(request);
    const email = normalizeEmail(body.email);

    const existing = await findUserByEmail(email);
    if (!existing && !env.registrationOpen) {
      throw new HttpError(403, "注册已关闭");
    }

    enforceRateLimit(`signin-code:email:${email}`, 5, 60 * 60 * 1000);
    // 同一邮箱 60 秒内只能发一次；界面上也会倒计时
    enforceRateLimit(`signin-code:cooldown:${email}`, 1, 60 * 1000);

    const code = await issueEmailCode(email, "signin", ip);
    try {
      const result = await sendVerificationCode(email, code);
      return { expiresIn: EMAIL_CODE_TTL_SECONDS, devCode: result.devCode };
    } catch (error) {
      // 没发出去就别留一个用户永远收不到的可用验证码
      await revokeEmailCodes(email, "signin");
      throw error;
    }
  });
}
