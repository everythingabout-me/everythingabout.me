import { randomInt } from "node:crypto";
import { hashPassword, verifyPassword } from "./auth";
import { query } from "./db";
import { HttpError } from "./errors";

export const EMAIL_CODE_TTL_SECONDS = 10 * 60;
const MAX_ATTEMPTS = 5;

function generateCode(): string {
  // randomInt 是密码学安全的；补零保证永远是 6 位
  return String(randomInt(0, 1_000_000)).padStart(6, "0");
}

/** 发一个新验证码；同一邮箱同一用途的旧码立即作废 */
export async function issueEmailCode(
  email: string,
  purpose: string,
  ip: string | null,
): Promise<string> {
  const code = generateCode();
  const codeHash = await hashPassword(code);
  const expiresAt = new Date(Date.now() + EMAIL_CODE_TTL_SECONDS * 1000);

  await query(
    `update email_codes set consumed_at = now()
      where email = $1 and purpose = $2 and consumed_at is null`,
    [email, purpose],
  );
  await query(
    `insert into email_codes (email, purpose, code_hash, expires_at, ip)
     values ($1, $2, $3, $4, $5)`,
    [email, purpose, codeHash, expiresAt, ip],
  );
  // 顺手清理一天前的过期记录
  await query(`delete from email_codes where expires_at < now() - interval '1 day'`);

  return code;
}

/** 邮件发送失败时把刚生成的码作废，避免留下一个用户永远收不到的可用码 */
export async function revokeEmailCodes(email: string, purpose: string): Promise<void> {
  await query(
    `update email_codes set consumed_at = now()
      where email = $1 and purpose = $2 and consumed_at is null`,
    [email, purpose],
  );
}

/**
 * 校验验证码但不消费。
 * 返回验证码记录的 id，供调用方在「两个码都验证通过」后再一起消费。
 */
async function verifyLatestCode(
  email: string,
  purpose: string | string[],
  code: string,
): Promise<string> {
  const purposes = Array.isArray(purpose) ? purpose : [purpose];
  const { rows } = await query<{
    id: string;
    code_hash: string;
    attempts: number;
    expires_at: Date | string;
  }>(
    `select id, code_hash, attempts, expires_at
       from email_codes
      where email = $1 and purpose = any($2::text[]) and consumed_at is null
      order by created_at desc
      limit 1`,
    [email, purposes],
  );

  const row = rows[0];
  if (!row) throw new HttpError(400, "验证码已失效，请重新发送");

  if (new Date(row.expires_at).getTime() <= Date.now()) {
    await query(`update email_codes set consumed_at = now() where id = $1`, [row.id]);
    throw new HttpError(400, "验证码已过期，请重新发送");
  }

  if (row.attempts >= MAX_ATTEMPTS) {
    await query(`update email_codes set consumed_at = now() where id = $1`, [row.id]);
    throw new HttpError(429, "验证码尝试次数过多，请重新发送");
  }

  const valid = await verifyPassword(code, row.code_hash);
  if (!valid) {
    await query(`update email_codes set attempts = attempts + 1 where id = $1`, [row.id]);
    throw new HttpError(400, "验证码不正确");
  }

  return row.id;
}

export async function verifyEmailCode(
  email: string,
  purpose: string | string[],
  code: string,
): Promise<string> {
  return verifyLatestCode(email, purpose, code);
}

/** 把已经验证通过的验证码标记为已使用 */
export async function consumeEmailCodeById(id: string): Promise<void> {
  const { rowCount } = await query(
    `update email_codes set consumed_at = now()
      where id = $1 and consumed_at is null`,
    [id],
  );
  if (!rowCount) throw new HttpError(400, "验证码已失效，请重新发送");
}

/** 校验并消费验证码；错误时抛 HttpError，成功时该码不可再用 */
export async function consumeEmailCode(
  email: string,
  purpose: string | string[],
  code: string,
): Promise<void> {
  const id = await verifyLatestCode(email, purpose, code);
  await consumeEmailCodeById(id);
}
