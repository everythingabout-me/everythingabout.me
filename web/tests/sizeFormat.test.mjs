import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/pages/sizeFormat.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "sizeFormat.ts");
const { formatBytes, formatPreciseBytes, formatSize, formatSpeed } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

test("uses decimal units, the same base Finder shows", () => {
  assert.equal(formatBytes(999), "999 B");
  assert.equal(formatBytes(1000), "1 KB");
  assert.equal(formatBytes(383000), "383 KB");
  assert.equal(formatBytes(2120000000), "2.12 GB");
});

test("matches Finder for the 21 GB clip that prompted this", () => {
  // Finder: "Size: 21,253,968,461 bytes (21.25 GB on disk)".
  assert.equal(formatBytes(21253968461), "21.25 GB");
});

test("promotes a value that rounds past its unit instead of printing 1000", () => {
  assert.equal(formatBytes(999900), "1 MB");
  assert.equal(formatBytes(999999999999), "1 TB");
});

test("drops trailing zeros like Finder", () => {
  assert.equal(formatBytes(3900000000), "3.9 GB");
  assert.equal(formatBytes(1000000000), "1 GB");
});

test("formatSize reserves the dash for a missing size", () => {
  assert.equal(formatSize(0), "—");
  assert.equal(formatSize(137), "137 B");
});

test("speed and progress keep their own precision", () => {
  assert.equal(formatSpeed(27800000), "27.8 MB/s");
  assert.equal(formatSpeed(0), "0 B/s");
  assert.equal(formatPreciseBytes(3736621547), "3.74 GB");
  assert.equal(formatPreciseBytes(0), "0 B");
});
