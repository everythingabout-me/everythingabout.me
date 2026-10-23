import { createHash, randomBytes, scrypt, timingSafeEqual } from "node:crypto";
import { cookies } from "next/headers";
import { query } from "./db";
import { HttpError } from "./errors";
import { env } from "./env";
import { toAuthUser, type AuthUser, type UserRow } from "./users";

const SESSION_COOKIE = "eam_session";

/** scrypt 参数存在哈希串里，以后调高成本不影响老用户登录 */
const SCRYPT = { N: 32768, r: 8, p: 1, keylen: 64 } as const;
const MAX_MEM = 128 * 1024 * 1024;

function scryptAsync(
  password: string,
  salt: Buffer,
  keylen: number,
  options: { N: number; r: number; p: number; maxmem: number },
): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    scrypt(password, salt, keylen, options, (error, key) =>
      error ? reject(error) : resolve(key),
    );
  });
}

export async function hashPassword(password: string): Promise<string> {
  const salt = randomBytes(16);
  const hash = await scryptAsync(password, salt, SCRYPT.keylen, { ...SCRYPT, maxmem: MAX_MEM });
  return [
    "scrypt",
    SCRYPT.N,
    SCRYPT.r,
    SCRYPT.p,
    salt.toString("base64"),
    hash.toString("base64"),
  ].join("$");
}

/**
 * 校验密码。哈希串格式不对、算法未知、长度不符都返回 false，不抛异常，
 * 避免把数据库里的脏数据变成 500。
 */
export async function verifyPassword(password: string, stored: string): Promise<boolean> {
  const parts = stored.split("$");
  if (parts.length !== 6 || parts[0] !== "scrypt") return false;

  const N = Number(parts[1]);
  const r = Number(parts[2]);
  const p = Number(parts[3]);
  const salt = Buffer.from(parts[4], "base64");
  const expected = Buffer.from(parts[5], "base64");
  if (!Number.isSafeInteger(N) || !Number.isSafeInteger(r) || !Number.isSafeInteger(p)) {
    return false;
  }
  // 防止被篡改的参数把内存打爆：N 上限 2^20，r/p 上限 32
  if (N < 1024 || N > 1_048_576 || r < 1 || r > 32 || p < 1 || p > 32) return false;
  if (!salt.length || !expected.length || expected.length > 256) return false;

  try {
    const actual = await scryptAsync(password, salt, expected.length, {
      N,
      r,
      p,
      maxmem: MAX_MEM,
    });
    return actual.length === expected.length && timingSafeEqual(actual, expected);
  } catch {
    return false;
  }
}

/** 用户不存在时也跑一次 scrypt，避免通过响应时间枚举邮箱 */
let dummyHashPromise: Promise<string> | null = null;
function dummyHash(): Promise<string> {
  dummyHashPromise ??= hashPassword(randomBytes(24).toString("hex"));
  return dummyHashPromise;
}
export async function burnPasswordCheck(password: string): Promise<void> {
  await verifyPassword(password, await dummyHash());
}

function hashToken(token: string): string {
  return createHash("sha256").update(token).digest("hex");
}

export interface SessionMeta {
  userAgent?: string | null;
  ip?: string | null;
}

export async function createSession(
  userId: string,
  meta: SessionMeta = {},
): Promise<{ token: string; expiresAt: Date }> {
  const token = randomBytes(32).toString("base64url");
  const expiresAt = new Date(Date.now() + env.sessionTtlDays * 86_400_000);

  await query(
    `insert into sessions (token_hash, user_id, expires_at, user_agent, ip)
     values ($1, $2, $3, $4, $5)`,
    [hashToken(token), userId, expiresAt, meta.userAgent ?? null, meta.ip ?? null],
  );
  // 顺手清掉过期会话，避免表无限增长
  await query(`delete from sessions where expires_at < now()`);

  return { token, expiresAt };
}

export async function getSessionToken(): Promise<string | null> {
  const jar = await cookies();
  return jar.get(SESSION_COOKIE)?.value ?? null;
}

export async function getSessionUser(): Promise<AuthUser | null> {
  const token = await getSessionToken();
  if (!token) return null;

  const { rows } = await query<UserRow>(
    `select u.*
       from sessions s
       join users u on u.id = s.user_id
      where s.token_hash = $1
        and s.expires_at > now()
      limit 1`,
    [hashToken(token)],
  );
  return rows[0] ? toAuthUser(rows[0]) : null;
}

export async function requireUser(): Promise<AuthUser> {
  const user = await getSessionUser();
  if (!user) throw new HttpError(401, "请先登录");
  return user;
}

export async function setSessionCookie(token: string, expiresAt: Date): Promise<void> {
  const jar = await cookies();
  jar.set(SESSION_COOKIE, token, {
    httpOnly: true,
    sameSite: "lax",
    secure: env.cookieSecure,
    path: "/",
    expires: expiresAt,
  });
}

export async function clearSessionCookie(): Promise<void> {
  const jar = await cookies();
  jar.set(SESSION_COOKIE, "", {
    httpOnly: true,
    sameSite: "lax",
    secure: env.cookieSecure,
    path: "/",
    maxAge: 0,
    expires: new Date(0),
  });
}

export async function destroySession(token: string): Promise<void> {
  await query(`delete from sessions where token_hash = $1`, [hashToken(token)]);
}

/** 注销：删库里的会话 + 清 cookie */
export async function logout(): Promise<void> {
  const token = await getSessionToken();
  if (token) await destroySession(token);
  await clearSessionCookie();
}
