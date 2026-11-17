import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/components/preview/htmlPreviewPolicy.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "htmlPreviewPolicy.ts");
const { fileHtmlPolicy, sandboxedHtml } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

// A meta element before the doctype drops the frame into quirks mode, and every report written for
// standards mode lays out differently there. The doctype has to stay first.
test("the policy travels after the doctype so the document stays in standards mode", () => {
  const original = '<!doctype html>\n<html><body><p>report</p></body></html>';
  const doc = sandboxedHtml(original);
  assert.ok(doc.startsWith("<!doctype html>"), doc);
  assert.ok(doc.indexOf("Content-Security-Policy") > doc.indexOf("<!doctype html>"));
  assert.ok(doc.includes("<p>report</p>"));
});

test("a document without a doctype keeps its own mode and still gets the policy", () => {
  const doc = sandboxedHtml("<html><body>plain</body></html>");
  assert.ok(doc.startsWith("<meta charset=\"utf-8\">"), doc);
  assert.ok(doc.endsWith("<html><body>plain</body></html>"), doc);
});

// The iframe's sandbox stops the document reaching this app; only this policy stops it reaching the
// network, which is what keeps an uploaded file from sending its own contents anywhere.
test("the policy allows no network origin at all", () => {
  const policy = fileHtmlPolicy();
  assert.match(policy, /default-src 'none'/);
  assert.match(policy, /connect-src 'none'/);
  for (const origin of ["http:", "https:", "*"]) {
    assert.equal(policy.includes(origin), false, `policy allows ${origin}: ${policy}`);
  }
});
