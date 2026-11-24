import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

// `fzstd` is the one dependency this decoder has, and a `data:` module cannot resolve a bare
// specifier; rewriting it to the resolved file URL is what lets the real decoder run here.
const decoderURL = import.meta.resolve("fzstd");
const source = (await readFile(new URL("../src/components/preview/zstd.ts", import.meta.url), "utf8"))
  .replace('from "fzstd"', `from ${JSON.stringify(decoderURL)}`);
const { code } = await transformWithOxc(source, "zstd.ts");
const { inflateZstd } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

// The compression chain, to prove the layer is wired to this decoder under the right name.
const chainSource = await readFile(new URL("../src/components/preview/compression.ts", import.meta.url), "utf8");
const chainCode = (await transformWithOxc(chainSource, "compression.ts")).code;
const { CompressionError, inflateChain } = await import(
  `data:text/javascript;base64,${Buffer.from(chainCode).toString("base64")}`
);

/**
 * A session log, compressed by the reference implementation (zstd 1.5.7, level 3), so the decoder
 * is tested against frames this repository did not write: `zstd -3 payload.jsonl` declares the
 * content size, and `cat payload.jsonl | zstd -3` (the streamed frame) declares none and a 2 MiB
 * window. The bytes are embedded rather than compressed at test time because node's own zstd API
 * does not exist on every node this repository supports.
 */
const lines = [
  '{"type":"session","v":4,"id":"3271969c-791f-4d7e-9d43-70d472230890"}',
  '{"type":"message","role":"user","text":"能支持预览吗？"}',
  '{"type":"message","role":"assistant","text":"这个格式暂不支持预览，下载后用本地应用打开。"}',
  '{"type":"tool","name":"bash","input":"git status --porcelain"}',
  '{"type":"tool","name":"bash","output":""}',
  '{"type":"summary","tokens":18452,"files":12,"model":"deepseek-flash"}',
  "",
];
const payload = Buffer.from(lines.join("\n"));

const ONE_SHOT = Buffer.from(
  "KLUv/WSmAI0JAKbRQjlApTgdr+tXm9xRFBigITCphBtzwJwETkQyoUOG1NFoqxUt9fcCvRRJq6IoB0sgj4wEN7+iOWpQ4wIxADYANgCsQTiXXBUJhaKsCpNYklJhl2QLq+RlclnScglcE4uifB6rj9U8weM3W7u5P8/c5t4go5JnInz4wK8izBiVJDzuRm/XbkrxnSJAoMKFKcEiwMEn3AiFJATT528B38d33F5mu8czhnYHUkbau/R25FaMx2318ZmmA59njW6Pz6y9ns5H5wMhFkIdIIcI1BCFGCEADiCQRDIxjEqf79QQEM4zTbOL1DA3x4x9PmPMao8UZ/XySAVUlcl8b6yx9/Gbjmqa19ezW6UZWzrLBQwAxQy8KQdDDIMQ05rZOurAkZeFX8EBEobcEEdmwb5AtHAGI9WGXQ==",
  "base64",
);
const STREAMED = Buffer.from(
  "KLUv/QRYjQkAplJFOVCFOB2o+N/WzsysOFu29yhunGAE04WJmIZhdwy7zW0jWX1oqZ/FfylymJkZwjao0Uh5PWA2MyPCGjQAOQA4AJXazZsjc8glgUQoyqowiDUjFXZNtrBqUqbLmpOL4JosKnJ6LLus6A0uz9msm/v03GHuEQKcsPQAEm0qxohQDC9GDZCIYsDl6u2su1qEpYmOyJBBWsAokMAZAYBjSIF0+nvA9/Ie93d61qx3eQ5zPvq79fbjZpXT36/jsstbuhacnrfKXV6z9m40BDShKNEwfsARKEAM4BE3POMHHEF0xFCI5AASBY7MLV01yrwwd8eMnV5jzG6X1ETfrGWUS2qgkkz0zfHF3stjvLptX9/PclnVeOLZLggATTkYYhgjxAhZB/dewRcSxqEhLhSIFs4j1YZd",
  "base64",
);

const decode = (frame, maxBytes = 128 * 1000 * 1000) => inflateZstd(new Uint8Array(frame), maxBytes);

test("a frame that declares its size decodes to exactly that file", () => {
  const result = decode(ONE_SHOT);
  assert.equal(result.ok, true);
  assert.equal(result.bytes.length, payload.length);
  assert.deepEqual(Buffer.from(result.bytes), payload);
});

test("a streamed frame with no declared size decodes to exactly that file", () => {
  // The size is only in the blocks — and `fzstd` pads a caller-supplied buffer instead of
  // reporting a length, which is why this path is decoded without one.
  const result = decode(STREAMED);
  assert.equal(result.ok, true);
  assert.deepEqual(Buffer.from(result.bytes), payload);
});

test("several frames in one object are all decoded", () => {
  const result = decode(Buffer.concat([ONE_SHOT, STREAMED]));
  assert.equal(result.ok, true);
  assert.deepEqual(Buffer.from(result.bytes), Buffer.concat([payload, payload]));
});

test("skippable frames in front of the data are skipped", () => {
  const skippable = Buffer.from([0x50, 0x2a, 0x4d, 0x18, 0x04, 0x00, 0x00, 0x00, 1, 2, 3, 4]);
  const result = decode(Buffer.concat([skippable, ONE_SHOT]));
  assert.equal(result.ok, true);
  assert.deepEqual(Buffer.from(result.bytes), payload);
});

test("a frame that claims more than the preview holds is refused before the allocation", () => {
  // Single segment off, an 8-byte content size: 1 TiB. Nothing is decoded.
  const header = Buffer.alloc(4 + 1 + 1 + 8);
  header.set([0x28, 0xb5, 0x2f, 0xfd, 0xc0, 0x00], 0);
  header.writeBigUInt64LE(2n ** 40n, 6);
  const result = decode(header);
  assert.equal(result.ok, false);
  assert.equal(result.kind, "too_large");
});

test("a frame that declares only a window is judged by what the decoder would allocate", () => {
  // No content size, window descriptor 0x90: windowLog 28, a 256 MB window.
  const header = Buffer.from([0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x90]);
  const result = decode(header);
  assert.equal(result.ok, false);
  assert.equal(result.kind, "too_large");
});

test("bytes that are not a Zstandard frame are named as such", () => {
  const result = decode(Buffer.from("definitely not zstd"));
  assert.equal(result.ok, false);
  assert.equal(result.kind, "not_compressed");
});

test("a truncated frame is damaged, not a short preview", () => {
  const result = decode(ONE_SHOT.subarray(0, ONE_SHOT.length - 10));
  assert.equal(result.ok, false);
  assert.equal(result.kind, "damaged");
});

test("the chain inflates a .zstd file through this decoder and renames it", async () => {
  const inner = await inflateChain(new Uint8Array(STREAMED), "session.v4.jsonl.zstd", { zstd: inflateZstd });
  assert.equal(inner.name, "session.v4.jsonl");
  assert.deepEqual(Buffer.from(inner.bytes), payload);
});

test("the chain reports what this decoder refused instead of swallowing it", async () => {
  await assert.rejects(
    // Past the compressed size, under the content size: the input passes, the decoder refuses.
    () => inflateChain(new Uint8Array(ONE_SHOT), "session.jsonl.zstd", { zstd: inflateZstd, maxBytes: 400 }),
    (error) => error instanceof CompressionError && error.kind === "too_large",
  );
});
