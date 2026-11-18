import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

// The marker scan is plain TypeScript over bytes, so it runs in node unchanged; only the module
// syntax has to be transformed first.
const source = await readFile(new URL("../src/components/preview/pdfEncryption.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "pdfEncryption.ts");
const { PDF_TAIL_BYTES, pdfLooksEncrypted } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

const bytes = (text) => new TextEncoder().encode(text);

test("a trailer that names an encryption dictionary is recognised", () => {
  const tail = bytes(
    "trailer\n<</Size 154/Root 3 0 R/Info 1 0 R/Encrypt 153 0 R/ID [<a439b9c7><25878d05>]>>\nstartxref\n248344\n%%EOF\n",
  );
  assert.equal(pdfLooksEncrypted(tail), true);
});

test("a plain trailer is not", () => {
  const tail = bytes("trailer\n<</Size 154/Root 3 0 R/Info 1 0 R>>\nstartxref\n248344\n%%EOF\n");
  assert.equal(pdfLooksEncrypted(tail), false);
});

test("the marker may sit at either edge of the window", () => {
  assert.equal(pdfLooksEncrypted(bytes("/Encrypt 153 0 R")), true);
  assert.equal(pdfLooksEncrypted(bytes("x/Encrypt")), true);
  // A window that ends mid-marker says nothing; the caller keeps the ordinary viewer rather than
  // guessing, which is the same answer an unreadable tail produces.
  assert.equal(pdfLooksEncrypted(bytes("x/Encryp")), false);
});

test("the scan tolerates an empty window", () => {
  assert.equal(pdfLooksEncrypted(new Uint8Array(0)), false);
});

test("the window is large enough to reach a trailer past a metadata stream", () => {
  // The tail of a real statement carries its XMP metadata (unencrypted even when the document is)
  // before the trailer, so the window has to be a few kilobytes, not a few hundred bytes.
  assert.ok(PDF_TAIL_BYTES >= 16 * 1024);
  const padding = " ".repeat(PDF_TAIL_BYTES - 40);
  assert.equal(pdfLooksEncrypted(bytes(`${padding}/Encrypt 9 0 R`)), true);
});
