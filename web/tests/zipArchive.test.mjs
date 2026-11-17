import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { deflateRawSync } from "node:zlib";
import { transformWithOxc } from "vite";

// The archive reader is plain TypeScript over Uint8Array, so it runs in node unchanged; only the
// module syntax has to be transformed first.
const source = await readFile(new URL("../src/components/preview/zipArchive.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "zipArchive.ts");
const { HttpByteSource, MAX_ENTRY_BYTES, MemoryByteSource, ZipError, readZipDirectory, readZipEntry } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

const u16 = (value) => {
  const bytes = Buffer.alloc(2);
  bytes.writeUInt16LE(value);
  return bytes;
};
const u32 = (value) => {
  const bytes = Buffer.alloc(4);
  bytes.writeUInt32LE(value >>> 0);
  return bytes;
};
const u64 = (value) => {
  const bytes = Buffer.alloc(8);
  bytes.writeBigUInt64LE(BigInt(value));
  return bytes;
};

/**
 * A minimal writer for the fixtures: only the fields the reader looks at are filled in, which is
 * why this lives in the test rather than in the app.
 */
function buildZip(entries, { zip64 = false, comment = "" } = {}) {
  const parts = [];
  const directory = [];
  let offset = 0;
  for (const entry of entries) {
    const nameBytes = entry.nameBytes ?? Buffer.from(entry.name, "utf8");
    const raw = Buffer.isBuffer(entry.data) ? entry.data : Buffer.from(entry.data ?? "", "utf8");
    const method = entry.method ?? (entry.deflate ? 8 : 0);
    const compressed = method === 8 ? deflateRawSync(raw) : raw;
    const flags = entry.flags ?? 0;
    const extra = zip64
      ? Buffer.concat([u16(0x0001), u16(24), u64(raw.length), u64(compressed.length), u64(offset)])
      : Buffer.alloc(0);
    const sizeField = zip64 ? 0xffffffff : compressed.length;
    const rawField = zip64 ? 0xffffffff : raw.length;
    const local = Buffer.concat([
      u32(0x04034b50),
      u16(20),
      u16(flags),
      u16(method),
      u16(0),
      u16(0),
      u32(0),
      u32(sizeField),
      u32(rawField),
      u16(nameBytes.length),
      u16(extra.length),
      nameBytes,
      extra,
      compressed,
    ]);
    parts.push(local);
    directory.push(
      Buffer.concat([
        u32(0x02014b50),
        u16(20),
        u16(20),
        u16(flags),
        u16(method),
        u16(0),
        u16(0),
        u32(0),
        u32(sizeField),
        u32(rawField),
        u16(nameBytes.length),
        u16(extra.length),
        u16(0),
        u16(0),
        u16(0),
        u32(0),
        u32(zip64 ? 0xffffffff : offset),
        nameBytes,
        extra,
      ]),
    );
    offset += local.length;
  }
  const central = Buffer.concat(directory);
  const commentBytes = Buffer.from(comment, "utf8");
  if (!zip64) {
    parts.push(central);
    parts.push(
      Buffer.concat([
        u32(0x06054b50),
        u16(0),
        u16(0),
        u16(entries.length),
        u16(entries.length),
        u32(central.length),
        u32(offset),
        u16(commentBytes.length),
        commentBytes,
      ]),
    );
    return new Uint8Array(Buffer.concat(parts));
  }
  const zip64RecordOffset = offset + central.length;
  parts.push(central);
  parts.push(
    Buffer.concat([
      u32(0x06064b50),
      u64(44),
      u16(45),
      u16(45),
      u32(0),
      u32(0),
      u64(entries.length),
      u64(entries.length),
      u64(central.length),
      u64(offset),
    ]),
  );
  parts.push(Buffer.concat([u32(0x07064b50), u32(0), u64(zip64RecordOffset), u32(1)]));
  parts.push(
    Buffer.concat([
      u32(0x06054b50),
      u16(0),
      u16(0),
      u16(0xffff),
      u16(0xffff),
      u32(0xffffffff),
      u32(0xffffffff),
      u16(commentBytes.length),
      commentBytes,
    ]),
  );
  return new Uint8Array(Buffer.concat(parts));
}

/** Counts what a parse actually transferred: the point of ranges is that it stays small. */
class CountingSource {
  constructor(inner) {
    this.inner = inner;
    this.bytes = 0;
    this.reads = 0;
  }

  get size() {
    return this.inner.size;
  }

  async read(offset, length, signal) {
    this.bytes += Math.max(0, Math.min(length, this.inner.size - offset));
    this.reads += 1;
    return this.inner.read(offset, length, signal);
  }
}

const memory = (bytes) => new MemoryByteSource(bytes);

test("the directory lists names and sizes without touching entry data", async () => {
  const archive = buildZip([
    { name: "docs/", data: "" },
    { name: "docs/report.txt", data: "hello from the report", deflate: true },
    { name: "image.bin", data: "raw bytes", deflate: false },
  ]);
  const counted = new CountingSource(memory(archive));
  const { entries } = await readZipDirectory(counted);

  assert.deepEqual(
    entries.map((entry) => [entry.name, entry.directory, entry.method, entry.uncompressedSize]),
    [
      ["docs/", true, 0, 0],
      ["docs/report.txt", false, 8, 21],
      ["image.bin", false, 0, 9],
    ],
  );
  assert.ok(counted.bytes < 2048, `directory read pulled ${counted.bytes} bytes`);

  const text = await readZipEntry(counted, entries[1]);
  assert.equal(Buffer.from(text).toString("utf8"), "hello from the report");
  // One header read plus the compressed payload, not the archive.
  assert.ok(counted.bytes < 4096, `directory plus one entry pulled ${counted.bytes} bytes`);
});

test("a stored entry comes back byte for byte", async () => {
  const payload = Buffer.from(Array.from({ length: 1024 }, (_, index) => index % 251));
  const archive = buildZip([{ name: "blob.bin", data: payload }]);
  const { entries } = await readZipDirectory(memory(archive));
  const bytes = await readZipEntry(memory(archive), entries[0]);
  assert.deepEqual(Buffer.from(bytes), payload);
});

test("an entry is inflated in full", async () => {
  const payload = "压缩包里的中文内容，按 UTF-8 编码。".repeat(500);
  const archive = buildZip([{ name: "notes.txt", data: payload, deflate: true }]);
  const { entries } = await readZipDirectory(memory(archive));
  const bytes = await readZipEntry(memory(archive), entries[0]);
  assert.equal(Buffer.from(bytes).toString("utf8"), payload);
  // Sanity: the fixture really is compressed, so the test is not passing on a stored entry.
  assert.ok(entries[0].compressedSize < entries[0].uncompressedSize);
});

test("a ZIP64 archive reads its directory and an entry's data", async () => {
  const archive = buildZip(
    [
      { name: "one.txt", data: "first", deflate: true },
      { name: "two.txt", data: "second", deflate: true },
    ],
    { zip64: true },
  );
  const { entries } = await readZipDirectory(memory(archive));
  assert.deepEqual(entries.map((entry) => entry.name), ["one.txt", "two.txt"]);
  assert.equal(Buffer.from(await readZipEntry(memory(archive), entries[1])).toString("utf8"), "second");
});

test("the end-of-directory record survives a comment that contains its signature", async () => {
  // A comment is arbitrary bytes; a naive forward/backward search for the signature can stop on
  // the one inside it. The comment length check in the reader is what disambiguates.
  const comment = "note PK\u0005\u0006 not the record";
  const archive = buildZip([{ name: "a.txt", data: "a", deflate: true }], { comment });
  const { entries } = await readZipDirectory(memory(archive));
  assert.deepEqual(entries.map((entry) => entry.name), ["a.txt"]);
});

test("a code-page name is decoded instead of shown as mojibake", async () => {
  // What a Chinese Windows tool writes: GBK bytes with the UTF-8 flag left off.
  const nameBytes = Buffer.from([0xb9, 0xa4, 0xd7, 0xf7, 0x2e, 0x74, 0x78, 0x74]);
  const archive = buildZip([{ nameBytes, data: "x", deflate: true }]);
  const { entries } = await readZipDirectory(memory(archive));
  assert.equal(entries[0].name, "工作.txt");
});

test("an encrypted entry is refused with a reason the page can name", async () => {
  const archive = buildZip([{ name: "secret.txt", data: "x", deflate: true, flags: 0x1 }]);
  const { entries } = await readZipDirectory(memory(archive));
  assert.equal(entries[0].encrypted, true);
  await assert.rejects(readZipEntry(memory(archive), entries[0]), (error) => error instanceof ZipError && error.kind === "encrypted");
});

test("a compression method no browser can inflate is reported as unsupported", async () => {
  const archive = buildZip([{ name: "packed.bin", data: "x", method: 12 }]);
  const { entries } = await readZipDirectory(memory(archive));
  await assert.rejects(readZipEntry(memory(archive), entries[0]), (error) => error instanceof ZipError && error.kind === "unsupported");
});

test("bytes that are not an archive fail as a named problem, not a crash", async () => {
  await assert.rejects(
    readZipDirectory(memory(new Uint8Array(Buffer.from("this is not a zip file at all, sorry")))),
    (error) => error instanceof ZipError && error.kind === "not_archive",
  );
  await assert.rejects(
    readZipDirectory(memory(new Uint8Array(10))),
    (error) => error instanceof ZipError && error.kind === "not_archive",
  );
});

test("an entry larger than the preview limit is refused before it is inflated", async () => {
  const archive = buildZip([{ name: "huge.bin", data: "x" }]);
  const { entries } = await readZipDirectory(memory(archive));
  entries[0].uncompressedSize = MAX_ENTRY_BYTES + 1;
  await assert.rejects(readZipEntry(memory(archive), entries[0]), (error) => error instanceof ZipError && error.kind === "too_large");
});

test("a source without ranges downloads once and then answers from memory", async () => {
  const archive = buildZip([{ name: "a.txt", data: "hello", deflate: true }]);
  const requests = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    requests.push({ url, range: init?.headers?.Range ?? null });
    return new Response(archive, { status: 200, headers: { "content-type": "application/zip" } });
  };
  try {
    const source = new HttpByteSource({ url: "https://example.test/a.zip", size: archive.length, acceptRanges: false });
    const { entries } = await readZipDirectory(source);
    const bytes = await readZipEntry(source, entries[0]);
    assert.equal(Buffer.from(bytes).toString("utf8"), "hello");
    assert.equal(requests.length, 1);
    assert.equal(requests[0].range, null);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("a ranged source asks only for the byte ranges it needs", async () => {
  const archive = buildZip([{ name: "a.txt", data: "hello", deflate: true }]);
  const requests = [];
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, init) => {
    const range = init.headers.Range;
    requests.push(range);
    const match = /^bytes=(\d+)-(\d+)$/.exec(range ?? "");
    if (!match) return new Response(archive, { status: 200 });
    const start = Number(match[1]);
    const end = Number(match[2]);
    return new Response(archive.subarray(start, end + 1), { status: 206 });
  };
  try {
    const source = new HttpByteSource({ url: "https://example.test/a.zip", size: archive.length, acceptRanges: true });
    const { entries } = await readZipDirectory(source);
    await readZipEntry(source, entries[0]);
    assert.ok(requests.length >= 2);
    for (const range of requests) assert.match(range, /^bytes=\d+-\d+$/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});
