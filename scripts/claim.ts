/**
 * 把多用户改造之前留下的旧文件认领给某个账号。
 *
 * 旧数据在数据库里 owner_id 为空、COS key 是 drive/<路径>；
 * 新方案是每个用户一个前缀 drive/<用户ID>/<路径>，所以这里同时
 * 复制对象并改写 key。先跑 --dry-run 看清楚再正式执行。
 *
 *   npm run claim -- --email me@example.com --dry-run
 *   npm run claim -- --email me@example.com
 *   npm run claim -- --email me@example.com --keep-source   # 保留旧对象，不删源
 *
 * 注意：这是 O(对象数) 的服务端复制，TB 级数据会跑很久；可以分目录手工用
 * coscli 搬到 drive/<用户ID>/ 再跑 reconcile --owner。
 */
import { pool, query } from "../lib/db";
import { storage } from "../lib/cos";
import { dirKeyFor, keyFor } from "../lib/paths";

const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const keepSource = args.includes("--keep-source");
const emailArg = args.indexOf("--email");
const email = emailArg >= 0 ? args[emailArg + 1]?.trim().toLowerCase() : undefined;

interface LegacyNode {
  id: string;
  path: string;
  key: string;
  is_dir: boolean;
}

async function assign(id: string, ownerId: string, key: string): Promise<number> {
  const { rowCount } = await query(
    `update nodes
        set owner_id = $1, key = $2, updated_at = now()
      where id = $3 and owner_id is null`,
    [ownerId, key, id],
  );
  return rowCount ?? 0;
}

async function run() {
  if (!email) {
    console.error("用法: npm run claim -- --email you@example.com [--dry-run] [--keep-source]");
    process.exit(1);
  }

  const { rows: users } = await query<{ id: string; email: string }>(
    `select id, email from users where lower(email) = lower($1) limit 1`,
    [email],
  );
  const user = users[0];
  if (!user) {
    console.error(`找不到用户 ${email}，请先在网站上注册`);
    process.exit(1);
  }

  const { rows } = await query<LegacyNode>(
    `select id, path, key, is_dir
       from nodes
      where owner_id is null
        and deleted_at is null
      order by is_dir asc, path asc`,
  );

  if (!rows.length) {
    console.log("没有需要认领的旧数据");
    await pool().end();
    return;
  }

  console.log(`认领 ${rows.length} 条记录到 ${user.email}（${user.id}）`);
  let moved = 0;
  let skipped = 0;
  let failed = 0;

  for (const row of rows) {
    const targetKey = row.is_dir ? dirKeyFor(row.path, user.id) : keyFor(row.path, user.id);

    if (dryRun) {
      console.log(`  ${row.key}  →  ${targetKey}`);
      moved += 1;
      continue;
    }

    try {
      const source = await storage().head(row.key);
      if (!source) {
        if (row.is_dir) {
          // 空目录的占位对象丢了也能补回来
          await storage().putEmpty(targetKey);
        } else {
          console.warn(`  跳过（源对象不存在）: ${row.key}`);
          skipped += 1;
          continue;
        }
      } else {
        await storage().copy(row.key, targetKey);
      }

      const assigned = await assign(row.id, user.id, targetKey);
      if (!assigned) {
        console.warn(`  跳过（记录已被其他进程认领）: ${row.key}`);
        skipped += 1;
        continue;
      }
      if (source && !keepSource) {
        // 数据库已经指向新 key 之后再删源，失败顶多留个重复对象，不会丢数据
        await storage().remove(row.key);
      }
      moved += 1;
      if (moved % 100 === 0) console.log(`  已处理 ${moved}/${rows.length}`);
    } catch (error) {
      failed += 1;
      console.error(
        `  失败 ${row.key}: ${error instanceof Error ? error.message : String(error)}`,
      );
    }
  }

  console.log(
    `${dryRun ? "[dry-run] " : ""}完成：迁移 ${moved}，跳过 ${skipped}，失败 ${failed}`,
  );
  await pool().end();
  if (failed) process.exit(1);
}

run().catch(async (error) => {
  console.error(error);
  await pool().end();
  process.exit(1);
});
