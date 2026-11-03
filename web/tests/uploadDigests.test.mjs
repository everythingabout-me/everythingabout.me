import assert from "node:assert/strict";
import { createHash, webcrypto } from "node:crypto";
import { createRequire } from "node:module";
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import test from "node:test";
import { transformWithOxc } from "vite";

const require = createRequire(import.meta.url);
const source = (await readFile(new URL("../src/api/uploadDigests.ts", import.meta.url), "utf8"))
  .replace('import("spark-md5")', `import(${JSON.stringify(pathToFileURL(require.resolve("spark-md5")).href)})`);
const { code } = await transformWithOxc(source, "uploadDigests.ts");
const { uploadDigests } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
if (!globalThis.crypto) globalThis.crypto = webcrypto;

for (const [value, expected] of [
  ["", "d41d8cd98f00b204e9800998ecf8427e"],
  ["a", "0cc175b9c0f1b6a831c399e269772661"],
  ["abc", "900150983cd24fb0d6963f7d28e17f72"],
  ["message digest", "f96b697d7cb7938d525a2f31aaf161d0"],
  ["abcdefghijklmnopqrstuvwxyz", "c3fcd3d76192e4007dfb496cca67e13b"],
]) {
  test(`part digests match standard MD5 and SHA-256 vectors (${JSON.stringify(value)})`, async () => {
    const result = await uploadDigests(new Blob([value]));
    assert.equal(Buffer.from(result.md5, "base64").toString("hex"), expected);
    assert.equal(result.sha256, createHash("sha256").update(value).digest("hex"));
  });
}

test("part digests hash exact binary bytes including full part boundaries and reuse one Blob result", async () => {
  const bytes = new Uint8Array(8 * 1024 * 1024 + 17);
  for (let i = 0; i < bytes.length; i++) bytes[i] = i % 251;
  const blob = new Blob([bytes]);
  const first = uploadDigests(blob);
  assert.equal(uploadDigests(blob), first);
  assert.deepEqual(await first, {
    sha256: createHash("sha256").update(bytes).digest("hex"),
    md5: createHash("md5").update(bytes).digest("base64"),
  });
});
