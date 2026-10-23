/**
 * 把 COS 桶里的对象同步进数据库索引。
 *
 *   1) 用 coscli / rclone 把文件灌进某个用户的 drive/<用户ID>/ 前缀后，跑这个脚本建索引；
 *   2) 也可以读 COS 清单（Inventory）CSV，避免百万对象全量 List。
 *
 * 用法：
 *   npm run reconcile -- --owner me@example.com              # 实时 List 扫描该用户
 *   npm run reconcile -- --owner me@example.com --inventory inv.csv
 *   npm run reconcile -- --dry-run                           # 不带 --owner 时只扫旧的无主数据
 */
import { createReadStream } from "node:fs";
import { createInterface } from "node:readline";
import { pool, query } from "../lib/db";
import { storage } from "../lib/cos";
import {
  DRIVE_PREFIX,
  ancestorsOf,
  dirKeyFor,
  nameOf,
  ownerPrefix,
  ownerSegmentOf,
  parentOf,
  pathFromKey,
} from "../lib/paths";

const BATCH = 500;
const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const inventoryArg = args.indexOf("--inventory");
const inventoryFile = inventoryArg >= 0 ? args[inventoryArg + 1] : undefined;
const ownerArg = args.indexOf("--owner");
const ownerEmail = ownerArg >= 0 ? args[ownerArg + 1]?.trim() : undefined;

interface Scanned {
  key: string;
  path: string;
  isDir: boolean;
  size: number;
  etag?: string;
  storageClass?: string;
}

function pick(row: Record<string, string>, ...names: string[]): string | undefined {
  for (const name of names) {
    const value = row[name];
    if (value) return value;
  }
  return undefined;
}

function toScanned(
  key: string,
  path: string,
  size: number,
  etag?: string,
  storageClass?: string,
): Scanned {
  return { key, path, isDir: key.endsWith("/"), size: key.endsWith("/") ? 0 : size, etag, storageClass };
}

async function* readInventory(
  file: string,
  prefix: string,
  ownerId: string | null,
): AsyncGenerator<Scanned> {
  const rl = createInterface({ input: createReadStream(file), crlfDelay: Infinity });
  let header: string[] | null = null;
  for await (const raw of rl) {
    const line = raw.trim();
    if (!line) continue;
    const cells = line.split(",").map((cell) => cell.replace(/^"|"$/g, ""));
    if (!header) {
      const lowered = cells.map((cell) => cell.toLowerCase());
      if (lowered.some((cell) => cell.includes("key"))) {
        header = lowered;
        continue;
      }
      // 无表头时按 COS 清单默认列序
      header = ["bucket", "key", "size", "lastmodifieddate", "etag", "storageclass"];
    }
    const row: Record<string, string> = {};
    header.forEach((name, index) => (row[name] = cells[index] ?? ""));

    const key = pick(row, "key", "objectkey", "object_key");
    if (!key || !key.startsWith(prefix)) continue;
    // 不带 --owner 时只处理旧的无主数据，跳过已有归属的 drive/<用户ID>/
    if (!ownerId && ownerSegmentOf(key)) continue;

    const path = pathFromKey(key, ownerId);
    if (path === null || (key.endsWith("/") && path === "")) continue;

    yield toScanned(
      key,
      path,
      Number(pick(row, "size") ?? 0),
      pick(row, "etag")?.replace(/"/g, ""),
      pick(row, "storageclass", "storage_class"),
    );
  }
}

async function run() {
  let ownerId: string | null = null;
  if (ownerEmail) {
    const { rows } = await query<{ id: string }>(
      `select id from users where lower(email) = lower($1) limit 1`,
      [ownerEmail],
    );
    if (!rows[0]) throw new Error(`找不到用户 ${ownerEmail}，请先在网站上注册`);
    ownerId = rows[0].id;
  }

  const prefix = ownerId ? ownerPrefix(ownerId) : DRIVE_PREFIX;
  const client = await pool().connect();
  const startedAt = new Date();
  let seen = 0;
  let inserted = 0;
  const buffer: Scanned[] = [];

  const flush = async () => {
    if (!buffer.length) return;
    const owners = buffer.map(() => ownerId);
    const keys = buffer.map((item) => item.key);
    const paths = buffer.map((item) => item.path);
    const parents = buffer.map((item) => parentOf(item.path));
    const names = buffer.map((item) => nameOf(item.path));
    const dirs = buffer.map((item) => item.isDir);
    const sizes = buffer.map((item) => item.size);
    const etags = buffer.map((item) => item.etag ?? null);
    const classes = buffer.map((item) => item.storageClass ?? null);

    const result = await client.query(
      `insert into nodes (owner_id, key, path, parent, name, is_dir, size, etag, storage_class)
       select * from unnest(
         $1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[],
         $6::bool[], $7::bigint[], $8::text[], $9::text[]
       )
       on conflict do nothing`,
      [owners, keys, paths, parents, names, dirs, sizes, etags, classes],
    );
    inserted += result.rowCount ?? 0;
    seen += buffer.length;

    await client.query(
      `insert into scan_keys (key) select unnest($1::text[]) on conflict do nothing`,
      [keys],
    );

    await client.query(
      `update nodes n
          set size = s.size,
              etag = s.etag,
              storage_class = s.storage_class,
              updated_at = now()
         from (
           select unnest($1::text[]) as key,
                  unnest($2::bigint[]) as size,
                  unnest($3::text[]) as etag,
                  unnest($4::text[]) as storage_class
         ) s
        where n.key = s.key
          and n.deleted_at is null
          and (n.size is distinct from s.size or n.etag is distinct from s.etag)`,
      [keys, sizes, etags, classes],
    );

    buffer.length = 0;
  };

  const accept = async (item: Scanned) => {
    buffer.push(item);
    if (buffer.length >= BATCH) await flush();
  };

  console.log(
    `扫描范围：${prefix}${ownerId ? "" : "（旧的无主数据）"}${dryRun ? " [dry-run]" : ""}`,
  );

  try {
    await client.query("begin");
    await client.query("create temp table scan_keys (key text primary key) on commit drop");

    if (inventoryFile) {
      for await (const item of readInventory(inventoryFile, prefix, ownerId)) await accept(item);
    } else {
      await storage().list(prefix, async (page) => {
        for (const object of page.objects) {
          if (!object.key.startsWith(prefix)) continue;
          if (!ownerId && ownerSegmentOf(object.key)) continue;
          const path = pathFromKey(object.key, ownerId);
          if (path === null || (object.key.endsWith("/") && path === "")) continue;
          await accept(
            toScanned(object.key, path, object.size, object.etag, object.storageClass),
          );
        }
      });
    }
    await flush();

    // 补全父目录
    const { rows: parents } = await client.query<{ parent: string }>(
      `select distinct parent
         from nodes
        where deleted_at is null
          and parent <> ''
          and owner_id is not distinct from $1`,
      [ownerId],
    );
    const allDirs = new Set<string>();
    for (const row of parents) {
      allDirs.add(row.parent);
      for (const ancestor of ancestorsOf(row.parent)) allDirs.add(ancestor);
    }
    if (allDirs.size) {
      const list = [...allDirs].sort((a, b) => a.length - b.length);
      await client.query(
        `insert into nodes (owner_id, key, path, parent, name, is_dir)
         select * from unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[])
         on conflict do nothing`,
        [
          list.map(() => ownerId),
          list.map((dir) => (ownerId ? dirKeyFor(dir, ownerId) : `${DRIVE_PREFIX}${dir}/`)),
          list,
          list.map((dir) => parentOf(dir)),
          list.map((dir) => nameOf(dir)),
        ],
      );
    }

    // 扫描期间没见过的对象 = 已被外部删除（只影响本次扫描的归属范围）
    const gone = await client.query(
      `update nodes set deleted_at = now(), updated_at = now()
        where deleted_at is null
          and updated_at < $1
          and owner_id is not distinct from $2
          and key not in (select key from scan_keys)`,
      [startedAt, ownerId],
    );

    await client.query(dryRun ? "rollback" : "commit");
    console.log(
      `${dryRun ? "[dry-run] " : ""}扫描 ${seen} 个对象，新增 ${inserted} 条，标记删除 ${gone.rowCount ?? 0} 条`,
    );
  } catch (error) {
    await client.query("rollback");
    throw error;
  } finally {
    client.release();
    await pool().end();
  }
}

run().catch((error) => {
  console.error(error);
  process.exit(1);
});
