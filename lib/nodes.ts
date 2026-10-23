import { query } from "./db";
import { HttpError } from "./errors";
import { storage } from "./cos";
import { TRASH_PREFIX, ancestorsOf, dirKeyFor, keyFor, nameOf, parentOf } from "./paths";

export interface NodeRow {
  id: string;
  key: string;
  path: string;
  parent: string;
  name: string;
  is_dir: boolean;
  size: string | number;
  mime: string | null;
  etag: string | null;
  storage_class: string | null;
  updated_at: string;
  meta: Record<string, unknown>;
}

/** 保证祖先目录都有记录（直传进桶时也能自动补全） */
export async function ensureParents(path: string): Promise<void> {
  for (const dir of ancestorsOf(path)) {
    await query(
      `insert into nodes (key, path, parent, name, is_dir)
       values ($1, $2, $3, $4, true)
       on conflict do nothing`,
      [dirKeyFor(dir), dir, parentOf(dir), nameOf(dir)],
    );
  }
}

/** 回收站里的旧对象占着同一个 key 时，先挪到 _trash/ 再允许覆盖 */
export async function evictTrashedAt(path: string): Promise<void> {
  const key = keyFor(path);
  const { rows } = await query<{ id: string; key: string }>(
    `select id, key from nodes where key = $1 and deleted_at is not null`,
    [key],
  );
  for (const row of rows) {
    const trashKey = `${TRASH_PREFIX}${row.id}`;
    await storage().copy(row.key, trashKey);
    await storage().remove(row.key);
    await query(`update nodes set key = $1 where id = $2`, [trashKey, row.id]);
  }
}

export async function getLiveNode(path: string): Promise<NodeRow | null> {
  const { rows } = await query<NodeRow>(
    `select * from nodes where path = $1 and deleted_at is null limit 1`,
    [path],
  );
  return rows[0] ?? null;
}

export async function upsertFile(input: {
  path: string;
  size: number;
  mime?: string | null;
  etag?: string | null;
  storageClass?: string | null;
  meta?: Record<string, unknown>;
}): Promise<NodeRow> {
  const { rows } = await query<NodeRow>(
    `insert into nodes (key, path, parent, name, is_dir, size, mime, etag, storage_class, meta)
     values ($1, $2, $3, $4, false, $5, $6, $7, $8, $9)
     on conflict (path) where deleted_at is null
     do update set
       size = excluded.size,
       mime = excluded.mime,
       etag = excluded.etag,
       storage_class = excluded.storage_class,
       updated_at = now(),
       meta = nodes.meta || excluded.meta
     returning *`,
    [
      keyFor(input.path),
      input.path,
      parentOf(input.path),
      nameOf(input.path),
      input.size,
      input.mime ?? null,
      input.etag ?? null,
      input.storageClass ?? null,
      JSON.stringify(input.meta ?? {}),
    ],
  );
  return rows[0];
}

export async function softDelete(path: string): Promise<number> {
  // 转义 LIKE 通配符，否则目录名里的 % 和 _ 会误伤别的路径
  const escaped = path.replace(/[\\%_]/g, (char) => "\\" + char);
  const { rowCount } = await query(
    `update nodes set deleted_at = now()
      where deleted_at is null
        and (path = $1 or path like $2)`,
    [path, `${escaped}/%`],
  );
  return rowCount ?? 0;
}

export function assertNotRoot(path: string): void {
  if (!path) throw new HttpError(400, "不能操作根目录");
}
