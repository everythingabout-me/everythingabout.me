import { HttpError } from "./errors";

/**
 * 只做"像不像邮箱"的宽松校验：本地部分允许任意非空白非 @ 字符。
 * 真正能不能收信，由验证码流程确认。
 */
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]{2,}$/;

export function normalizeEmail(value: unknown): string {
  if (typeof value !== "string") throw new HttpError(400, "请输入邮箱");
  const email = value.trim().toLowerCase();
  if (!email) throw new HttpError(400, "请输入邮箱");
  if (email.length > 254) throw new HttpError(400, "邮箱过长");
  if (!EMAIL_RE.test(email)) throw new HttpError(400, "邮箱格式不正确");
  return email;
}

export function validatePassword(value: unknown): string {
  if (typeof value !== "string" || !value) throw new HttpError(400, "请输入密码");
  if (value.length < 8) throw new HttpError(400, "密码至少 8 位");
  if (value.length > 128) throw new HttpError(400, "密码最长 128 位");
  return value;
}
