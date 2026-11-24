import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { gzipSync } from "node:zlib";
import { transformWithOxc } from "vite";

// The compression reader is plain TypeScript over bytes, so it runs in node unchanged; only the
// module syntax has to be transformed first. Zstandard is exercised against real frames in
// zstdInflate.test.mjs — here it is a stub, because what this file checks is the *chain*: which
// suffix picks which decoder, what the inner name becomes, and where the limits apply.
const source = await readFile(new URL("../src/components/preview/compression.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "compression.ts");
const {
  CompressionError,
  MAX_DECOMPRESSED_BYTES,
  compressionKind,
  inflateChain,
  inflateGzip,
  uncompressedName,
} = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

const text = (bytes) => Buffer.from(bytes).toString("utf8");

/** Stands in for the Zstandard decoder: marks what it touched so the chain is visible. */
const zstdStub = async (bytes, maxBytes) => {
  if (bytes.length > maxBytes) return { ok: false, kind: "too_large", detail: "stub" };
  return { ok: true, bytes: new Uint8Array(Buffer.concat([Buffer.from("zstd:"), Buffer.from(bytes)])) };
};

test("a compression suffix decides the format and names the file inside", () => {
  assert.equal(compressionKind("session.v4.jsonl.zstd"), "zstd");
  assert.equal(uncompressedName("session.v4.jsonl.zstd"), "session.v4.jsonl");
  assert.equal(compressionKind("dump.zst"), "zstd");
  assert.equal(compressionKind("notes.txt.gz"), "gzip");
  assert.equal(uncompressedName("notes.txt.gz"), "notes.txt");
  assert.equal(compressionKind("archive.GZ"), "gzip");
  assert.equal(uncompressedName("archive.GZ"), "archive");
  // A name is not a suffix: the last extension decides, and a hidden file has none.
  for (const name of [".gz", "gz", "archive.zip", "a.gz.txt", "photo.png", ""]) {
    assert.equal(compressionKind(name), null, name);
    assert.equal(uncompressedName(name), name, name);
  }
});

test("a gzip file previews as its contents, under the name the suffix left", async () => {
  const body = "line one\nline two\n".repeat(500);
  const { name, bytes } = await inflateChain(new Uint8Array(gzipSync(body)), "notes.txt.gz");
  assert.equal(name, "notes.txt");
  assert.equal(text(bytes), body);
});

test("a Zstandard file reaches the decoder with its own suffix, and the chain follows", async () => {
  const inner = await inflateChain(new Uint8Array(Buffer.from("payload")), "session.jsonl.zstd", { zstd: zstdStub });
  assert.equal(inner.name, "session.jsonl");
  assert.equal(text(inner.bytes), "zstd:payload");

  // gzip(zstd(x)) — the innermost name is the one that decides the renderer.
  const twice = await inflateChain(new Uint8Array(gzipSync(Buffer.from("payload"))), "a.jsonl.zst.gz", { zstd: zstdStub });
  assert.equal(twice.name, "a.jsonl");
  assert.equal(text(twice.bytes), "zstd:payload");
});

test("nothing to undo answers the file itself", async () => {
  const { name, bytes } = await inflateChain(new Uint8Array(Buffer.from("plain")), "notes.txt", { zstd: zstdStub });
  assert.equal(name, "notes.txt");
  assert.equal(text(bytes), "plain");
});

test("a stream past the output limit is refused while it inflates", async () => {
  // 2 MB of one byte compresses to a couple of kilobytes, so the cap has to be enforced on the
  // output rather than on the input.
  const packed = gzipSync("x".repeat(2_000_000));
  assert.ok(packed.length < 1_000_000);
  await assert.rejects(
    () => inflateChain(new Uint8Array(packed), "big.txt.gz", { maxBytes: 1_000_000 }),
    (error) => error instanceof CompressionError && error.kind === "too_large",
  );
});

test("bytes that are not the format the name claims are named as such", async () => {
  const notGzip = await inflateGzip(new Uint8Array(Buffer.from("definitely not gzip")), MAX_DECOMPRESSED_BYTES);
  assert.equal(notGzip.ok, false);
  assert.equal(notGzip.kind, "not_compressed");
  await assert.rejects(
    () => inflateChain(new Uint8Array(Buffer.from("definitely not gzip")), "a.txt.gz"),
    (error) => error instanceof CompressionError && error.kind === "not_compressed",
  );
});

test("a truncated gzip stream is damaged, not empty", async () => {
  const packed = gzipSync("a body long enough to notice the missing tail\n".repeat(50));
  await assert.rejects(
    () => inflateChain(new Uint8Array(packed.subarray(0, packed.length - 30)), "a.txt.gz"),
    (error) => error instanceof CompressionError && error.kind === "damaged",
  );
});

test("layers are followed to a point and then refused", async () => {
  const three = gzipSync(gzipSync(gzipSync("deep")));
  const { name, bytes } = await inflateChain(new Uint8Array(three), "deep.txt.gz.gz.gz");
  assert.equal(name, "deep.txt");
  assert.equal(text(bytes), "deep");

  const four = gzipSync(three);
  await assert.rejects(
    () => inflateChain(new Uint8Array(four), "deep.txt.gz.gz.gz.gz"),
    (error) => error instanceof CompressionError && error.kind === "nested",
  );
});

test("an input past the byte limit is refused before the decoder sees it", async () => {
  // The output cap cannot stop a bomb whose compressed input is already larger than the budget:
  // the bytes are refused on the way in, before any decoder allocates for them.
  const packed = gzipSync("small");
  await assert.rejects(
    () => inflateChain(new Uint8Array(packed), "a.txt.gz", { maxBytes: packed.length - 1 }),
    (error) => error instanceof CompressionError && error.kind === "too_large",
  );
});
