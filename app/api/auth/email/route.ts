import type { NextRequest } from "next/server";
import { handle } from "@/lib/api";
import { requireUser } from "@/lib/auth";
import { isUniqueViolation } from "@/lib/db";
import { consumeEmailCodeById, verifyEmailCode } from "@/lib/email-codes";
import { HttpError } from "@/lib/errors";
import { assertSameOrigin, readJson } from "@/lib/http";
import { enforceRateLimit } from "@/lib/rate-limit";
import { findUserByEmail, toAuthUser, updateUserEmail } from "@/lib/users";
import { normalizeEmail } from "@/lib/validation";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

async function verifyOrLabel(
  email: string,
  purpose: string,
  code: string,
  label: string,
): Promise<string> {
  try {
    return await verifyEmailCode(email, purpose, code);
  } catch (error) {
    if (error instanceof HttpError && error.message === "验证码不正确") {
      throw new HttpError(400, `${label}验证码不正确`);
    }
    throw error;
  }
}

/** 修改邮箱：当前邮箱和新邮箱的两个验证码都必须正确 */
export async function POST(request: NextRequest) {
  return handle(async () => {
    assertSameOrigin(request);
    const current = await requireUser();
    enforceRateLimit(`email-change:${current.id}`, 10, 60 * 60 * 1000);

    const body = await readJson<{
      email?: unknown;
      oldCode?: unknown;
      code?: unknown;
    }>(request);
    const email = normalizeEmail(body.email);
    const oldCode = typeof body.oldCode === "string" ? body.oldCode.trim() : "";
    const code = typeof body.code === "string" ? body.code.trim() : "";
    if (!/^\d{6}$/.test(oldCode)) throw new HttpError(400, "请输入当前邮箱验证码");
    if (!/^\d{6}$/.test(code)) throw new HttpError(400, "请输入新邮箱验证码");
    if (email === current.email) throw new HttpError(400, "新邮箱和当前邮箱相同");

    if (await findUserByEmail(email)) {
      throw new HttpError(409, "这个邮箱已被其他账户使用");
    }

    // 先都验证通过，再一起消费：不会因为其中一个填错而把另一个作废
    const oldId = await verifyOrLabel(
      current.email,
      "change_email_old",
      oldCode,
      "当前邮箱",
    );
    const newId = await verifyOrLabel(email, "change_email", code, "新邮箱");
    await consumeEmailCodeById(oldId);
    await consumeEmailCodeById(newId);

    try {
      return { user: toAuthUser(await updateUserEmail(current.id, email)) };
    } catch (error) {
      if (isUniqueViolation(error)) throw new HttpError(409, "这个邮箱已被其他账户使用");
      throw error;
    }
  });
}
