/**
 * 清理回收站：删除超过保留期的对象和记录。
 *
 *   npm run purge                # 默认 30 天
 *   npm run purge -- --days 7
 *   npm run purge -- --dry-run
 */
import { pool, query } from "../lib/db";
import { storage } from "../lib/cos";

const args = process.argv.slice(2);
const dryRun = args.includes("--dry-run");
const daysIndex = args.indexOf("--days");
const days = daysIndex >= 0 ? Number(args[daysIndex + 1]) : 30;

async function run() {
  const { rows } = await query<{ id: string; key: string; is_dir: boolean }>(
    `select id, key, is_dir from nodes
      where deleted_at is not null
        and deleted_at < now() - ($1 || ' days')::interval
      order by deleted_at asc
      limit 5000`,
    [String(days)],
  );

  if (!rows.length) {
    console.log("没有需要清理的对象");
    await pool().end();
    return;
  }

  if (dryRun) {
    console.log(`[dry-run] 将清理 ${rows.length} 条记录`);
    await pool().end();
    return;
  }

  const store = storage();
  for (const row of rows) {
    try {
      await store.remove(row.key);
    } catch (error) {
      console.warn("删除对象失败", row.key, error);
    }
  }

  await query(`delete from nodes where id = any($1::uuid[])`, [rows.map((row) => row.id)]);
  console.log(`已清理 ${rows.length} 条记录`);
  await pool().end();
}

run().catch((error) => {
  console.error(error);
  process.exit(1);
});
