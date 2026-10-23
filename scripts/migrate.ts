/**
 * 按文件名顺序执行 sql/*.sql，全部放在一个事务里。
 *
 *   npm run db:init
 *
 * 用 Node 跑而不是 psql，这样能直接读 .env，不用先 export DATABASE_URL。
 */
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { pool } from "../lib/db";

async function run() {
  const dir = join(process.cwd(), "sql");
  const files = readdirSync(dir)
    .filter((file) => file.endsWith(".sql"))
    .sort();

  const client = await pool().connect();
  try {
    await client.query("begin");
    for (const file of files) {
      process.stdout.write(`→ ${file} `);
      await client.query(readFileSync(join(dir, file), "utf8"));
      console.log("✓");
    }
    await client.query("commit");
    console.log(`\n已执行 ${files.length} 个迁移。\n`);
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
