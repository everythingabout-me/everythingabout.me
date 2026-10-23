import { query } from "./db";
import { HttpError } from "./errors";
import { storage } from "./cos";
import { TRASH_PREFIX, ancestorsOf, dirKeyFor, keyFor, nameOf, parentOf } from "./paths";

export interface NodeRow {
  id: string;
  owner_id: string;
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

/** 把一条回收站记录的对象挪到 _trash/，并把 DB 里的 key 改过去 */
async function moveToTrash(row: { id: string; key: string }): Promise<void> {
  const trashKey = `${TRASH_PREFIX}${row.id}`;
  // 对象可能已经被手动清掉；这时只改 DB key，避免同名新建被唯一约束挡住
  const exists = await storage().head(row.key);
  if (exists) await storage().copy(row.key, trashKey);
  await query(`update nodes set key = $1 where id = $2`, [trashKey, row.id]);
  // 先改 DB 再删源：删源失败最多留个重复对象，不会把索引指向不存在的 key
  if (exists) await storage().remove(row.key);
}

/** 保证祖先目录都有记录（直传进桶时也能自动补全） */
export async function ensureParents(path: string, ownerId: string): Promise<void> {
  const dirs = ancestorsOf(path);
  if (!dirs.length) return;

  // 祖先目录可能在回收站里；先把它们的占位对象挪进 _trash/，
  // 否则新插入的目录会撞上 nodes.key 的全局唯一约束。
  const { rows } = await query<{ id: string; key: string }>(
    `select id, key from nodes
      where owner_id = $1
        and deleted_at is not null
        and path = any($2::text[])`,
    [ownerId, dirs],
  );
  for (const row of rows) await moveToTrash(row);

  for (const dir of dirs) {
    await query(
      `insert into nodes (owner_id, key, path, parent, name, is_dir)
       values ($1, $2, $3, $4, $5, true)
       on conflict (owner_id, path) where deleted_at is null do nothing`,
      [ownerId, dirKeyFor(dir, ownerId), dir, parentOf(dir), nameOf(dir)],
    );
  }
}

/** 回收站里的旧对象占着同一个 key 时，先挪到 _trash/ 再允许覆盖 */
export async function evictTrashedAt(
  path: string,
  ownerId: string,
  isDir = false,
): Promise<void> {
  const key = isDir ? dirKeyFor(path, ownerId) : keyFor(path, ownerId);
  const { rows } = await query<{ id: string; key: string }>(
    `select id, key from nodes where key = $1 and deleted_at is not null`,
    [key],
  );
  for (const row of rows) await moveToTrash(row);
}

export async function getLiveNode(path: string, ownerId: string): Promise<NodeRow | null> {
  const { rows } = await query<NodeRow>(
    `select * from nodes
      where owner_id = $1 and path = $2 and deleted_at is null
      limit 1`,
    [ownerId, path],
  );
  return rows[0] ?? null;
}

export async function upsertFile(input: {
  ownerId: string;
  path: string;
  size: number;
  mime?: string | null;
  etag?: string | null;
  storageClass?: string | null;
  meta?: Record<string, unknown>;
}): Promise<NodeRow> {
  const { rows } = await query<NodeRow>(
    `insert into nodes (owner_id, key, path, parent, name, is_dir, size, mime, etag, storage_class, meta)
     values ($1, $2, $3, $4, $5, false, $6, $7, $8, $9, $10)
     on conflict (owner_id, path) where deleted_at is null
     do update set
       key = excluded.key,
       size = excluded.size,
       mime = excluded.mime,
       etag = excluded.etag,
       storage_class = excluded.storage_class,
       updated_at = now(),
       meta = nodes.meta || excluded.meta
     returning *`,
    [
      input.ownerId,
      keyFor(input.path, input.ownerId),
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

export async function softDelete(path: string, ownerId: string): Promise<number> {
  // 转义 LIKE 通配符，否则目录名里的 % 和 _ 会误伤别的路径
  const escaped = path.replace(/[\\%_]/g, (char) => "\\" + char);
  const { rowCount } = await query(
    `update nodes set deleted_at = now()
      where owner_id = $1
        and deleted_at is null
        and (path = $2 or path like $3)`,
    [ownerId, path, `${escaped}/%`],
  );
  return rowCount ?? 0;
}

export function assertNotRoot(path: string): void {
  if (!path) throw new HttpError(400, "不能操作根目录");
}
