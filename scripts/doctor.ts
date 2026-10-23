/**
 * 自检：确认 .env、数据库、COS 权限三件事都是通的。
 *
 *   npm run check            # 只读检查
 *   npm run check -- --write # 额外测试一次写入+删除
 */
import { pool } from "../lib/db";
import { env } from "../lib/env";
import { storage } from "../lib/storage";
import { storage as cos } from "../lib/cos";

const writeTest = process.argv.includes("--write");
let failed = 0;

function ok(label: string, detail = "") {
  console.log(`  ✓ ${label}${detail ? `  ${detail}` : ""}`);
}

function bad(label: string, error: unknown, hint?: string) {
  failed += 1;
  const message = error instanceof Error ? error.message : JSON.stringify(error);
  console.log(`  ✗ ${label}  ${message}`);
  if (hint) console.log(`    → ${hint}`);
}

async function main() {
  console.log("\n配置");
  console.log(`  bucket   ${env.cos.bucket}`);
  console.log(`  region   ${env.cos.region}`);
  console.log(`  database ${env.databaseUrl.replace(/\/\/[^@]*@/, "//***@")}`);

  console.log("\n数据库");
  try {
    const { rows } = await pool().query<{ t: string | null }>(
      "select to_regclass('public.nodes') as t",
    );
    if (!rows[0]?.t) {
      bad("nodes 表不存在", new Error("missing"), "先跑 npm run db:init");
    } else {
      const count = await pool().query<{ n: string }>("select count(*) as n from nodes");
      ok("连接正常，nodes 表就绪", `当前 ${count.rows[0].n} 条记录`);
    }
  } catch (error) {
    bad("连接失败", error, "检查 DATABASE_URL；端口被别的库占用时换一个端口");
  }

  console.log("\nCOS");
  const probe = `drive/__doctor__/probe-${Date.now()}.txt`;
  try {
    const head = await cos().head(probe);
    ok("鉴权与桶可达", head ? "（意外：探针对象存在）" : "（探针对象不存在，符合预期）");
  } catch (error) {
    bad("headObject 失败", error, "多为桶名缺 APPID、region 填错、或密钥无权限");
  }

  try {
    const cred = await cos().temporaryCredentials(probe, 900);
    ok("STS 临时凭证签发成功", `有效期至 ${new Date(cred.expiredTime * 1000).toLocaleString()}`);
  } catch (error) {
    bad(
      "STS 签发失败",
      error,
      "子账号需要 sts:GetFederationToken 权限，或密钥不是有效的 API 密钥",
    );
  }

  if (writeTest) {
    try {
      await storage().putEmpty(probe);
      const head = await storage().head(probe);
      await storage().remove(probe);
      ok("写入 / 读取 / 删除", `大小 ${head?.size ?? 0} 字节`);
    } catch (error) {
      bad("写入失败", error, "检查子账号是否有 PutObject / DeleteObject 权限");
    }
  }

  await pool().end();
  console.log(failed === 0 ? "\n一切正常。\n" : `\n有 ${failed} 项没通过。\n`);
  process.exit(failed === 0 ? 0 : 1);
}

main().catch(async (error) => {
  console.error(error);
  await pool().end().catch(() => {});
  process.exit(1);
});
