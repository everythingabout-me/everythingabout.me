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
import { assertSameOrigin } from "@/lib/http";
import { clientIp, enforceRateLimit } from "@/lib/rate-limit";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

/** 给「当前邮箱」发验证码，用于改邮箱前的身份确认 */
export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const current = await requireUser();
    if (process.env.NODE_ENV === "production" && !emailDeliveryConfigured()) {
      throw new HttpError(503, "邮件服务未配置，暂时无法发送验证码");
    }

    const ip = clientIp(request);
    enforceRateLimit(`email-change-old:ip:${ip}`, 30, 60 * 60 * 1000);
    enforceRateLimit(`email-change-old:user:${current.id}`, 10, 60 * 60 * 1000);
    enforceRateLimit(`email-change-old:email:${current.email}`, 5, 60 * 60 * 1000);
    enforceRateLimit(`email-change-old:cooldown:${current.email}`, 1, 60 * 1000);

    const code = await issueEmailCode(current.email, "change_email_old", ip);
    try {
      const result = await sendVerificationCode(current.email, code);
      return { expiresIn: EMAIL_CODE_TTL_SECONDS, devCode: result.devCode };
    } catch (error) {
      await revokeEmailCodes(current.email, "change_email_old");
      throw error;
    }
  });
}
