import { query } from "./db";

export interface UserRow {
  id: string;
  email: string;
  password_hash: string | null;
  created_at: Date | string;
  updated_at: Date | string;
  email_changed_at: Date | string | null;
  last_login_at: Date | string | null;
}

/** 暴露给前端的用户形状（永远不含 password_hash） */
export interface AuthUser {
  id: string;
  email: string;
  hasPassword: boolean;
  createdAt: string;
  emailChangedAt: string | null;
}

function iso(value: Date | string | null): string | null {
  if (!value) return null;
  return value instanceof Date ? value.toISOString() : new Date(value).toISOString();
}

export function toAuthUser(row: UserRow): AuthUser {
  return {
    id: row.id,
    email: row.email,
    hasPassword: Boolean(row.password_hash),
    createdAt: iso(row.created_at) ?? new Date(0).toISOString(),
    emailChangedAt: iso(row.email_changed_at),
  };
}

export async function countUsers(): Promise<number> {
  const { rows } = await query<{ n: string }>("select count(*) as n from users");
  return Number(rows[0]?.n ?? 0);
}

export async function findUserByEmail(email: string): Promise<UserRow | null> {
  const { rows } = await query<UserRow>(
    `select * from users where lower(email) = lower($1) limit 1`,
    [email],
  );
  return rows[0] ?? null;
}

export async function findUserById(id: string): Promise<UserRow | null> {
  const { rows } = await query<UserRow>(`select * from users where id = $1 limit 1`, [id]);
  return rows[0] ?? null;
}

export async function createUser(
  email: string,
  passwordHash: string | null = null,
): Promise<UserRow> {
  const { rows } = await query<UserRow>(
    `insert into users (email, password_hash)
     values ($1, $2)
     returning *`,
    [email, passwordHash],
  );
  return rows[0];
}

export async function setUserPassword(
  id: string,
  passwordHash: string | null,
): Promise<UserRow> {
  const { rows } = await query<UserRow>(
    `update users
        set password_hash = $1, updated_at = now()
      where id = $2
      returning *`,
    [passwordHash, id],
  );
  return rows[0];
}

export async function updateUserEmail(id: string, email: string): Promise<UserRow> {
  const { rows } = await query<UserRow>(
    `update users
        set email = $1,
            updated_at = now(),
            email_changed_at = now()
      where id = $2
      returning *`,
    [email, id],
  );
  return rows[0];
}

export async function touchLastLogin(id: string): Promise<void> {
  await query(`update users set last_login_at = now() where id = $1`, [id]);
}
