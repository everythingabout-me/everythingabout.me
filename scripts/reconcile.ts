/**
 * 把 COS 桶里的对象同步进数据库索引。
 *
 *   1) 用 coscli / rclone 把文件灌进 drive/ 前缀后，跑这个脚本建索引；
 *   2) 也可以读 COS 清单（Inventory）CSV，避免百万对象全量 List。
 *
 * 用法：
 *   npm run reconcile                          # 实时 List 扫描 drive/
 *   npm run reconcile -- --inventory inv.csv   # 读清单
 *   npm run reconcile -- --dry-run
 */
import { createReadStream } from "node:fs";
import { createInterface } from "node:readline";
import { pool } from "../lib/db";
import { storage } from "../lib/cos";
import { ancestorsOf, dirKeyFor, nameOf, parentOf, pathFromKey } from "../lib/paths";

const BATCH = 500;
const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const inventoryArg = args.indexOf("--inventory");
const inventoryFile = inventoryArg >= 0 ? args[inventoryArg + 1] : undefined;

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

async function* readInventory(file: string): AsyncGenerator<Scanned> {
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
    if (!key) continue;
    const path = pathFromKey(key);
    if (path === null || (key.endsWith("/") && path === "")) continue;

    yield {
      key,
      path,
      isDir: key.endsWith("/"),
      size: key.endsWith("/") ? 0 : Number(pick(row, "size") ?? 0),
      etag: pick(row, "etag")?.replace(/"/g, ""),
      storageClass: pick(row, "storageclass", "storage_class"),
    };
  }
}

async function run() {
  const client = await pool().connect();
  const startedAt = new Date();
  let seen = 0;
  let inserted = 0;
  const buffer: Scanned[] = [];

  const flush = async () => {
    if (!buffer.length) return;
    const keys = buffer.map((item) => item.key);
    const paths = buffer.map((item) => item.path);
    const parents = buffer.map((item) => parentOf(item.path));
    const names = buffer.map((item) => nameOf(item.path));
    const dirs = buffer.map((item) => item.isDir);
    const sizes = buffer.map((item) => item.size);
    const etags = buffer.map((item) => item.etag ?? null);
    const classes = buffer.map((item) => item.storageClass ?? null);

    const result = await client.query(
      `insert into nodes (key, path, parent, name, is_dir, size, etag, storage_class)
       select * from unnest(
         $1::text[], $2::text[], $3::text[], $4::text[],
         $5::bool[], $6::bigint[], $7::text[], $8::text[]
       )
       on conflict do nothing`,
      [keys, paths, parents, names, dirs, sizes, etags, classes],
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

  try {
    await client.query("begin");
    await client.query("create temp table scan_keys (key text primary key) on commit drop");

    if (inventoryFile) {
      for await (const item of readInventory(inventoryFile)) await accept(item);
    } else {
      await storage().list("drive/", async (page) => {
        for (const object of page.objects) {
          const path = pathFromKey(object.key);
          if (path === null || (object.key.endsWith("/") && path === "")) continue;
          await accept({
            key: object.key,
            path,
            isDir: object.key.endsWith("/"),
            size: object.size,
            etag: object.etag,
            storageClass: object.storageClass,
          });
        }
      });
    }
    await flush();

    // 补全父目录
    const { rows: parents } = await client.query<{ parent: string }>(
      `select distinct parent from nodes where deleted_at is null and parent <> ''`,
    );
    const allDirs = new Set<string>();
    for (const row of parents) {
      allDirs.add(row.parent);
      for (const ancestor of ancestorsOf(row.parent)) allDirs.add(ancestor);
    }
    if (allDirs.size) {
      const list = [...allDirs].sort((a, b) => a.length - b.length);
      await client.query(
        `insert into nodes (key, path, parent, name, is_dir)
         select * from unnest($1::text[], $2::text[], $3::text[], $4::text[])
         on conflict do nothing`,
        [
          list.map((dir) => dirKeyFor(dir)),
          list,
          list.map((dir) => parentOf(dir)),
          list.map((dir) => nameOf(dir)),
        ],
      );
    }

    // 扫描期间没见过的对象 = 已被外部删除
    const gone = await client.query(
      `update nodes set deleted_at = now(), updated_at = now()
        where deleted_at is null
          and updated_at < $1
          and key not in (select key from scan_keys)`,
      [startedAt],
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
