import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

async function load(path) {
  const source = await readFile(new URL(path, import.meta.url), "utf8");
  const { code } = await transformWithOxc(source, path);
  return import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
}
const { filesHref, filesRoutePath } = await load("../src/filesPath.ts");
const { canPreviewFile } = await load("../src/components/preview/supported.ts");
const { htmlCardPolicy } = await load("../src/components/preview/htmlCardPolicy.ts");

test("every account's tree is the same /files address, with no account name in it", () => {
  // The address is the path and nothing else: the session says whose tree this is, so there is
  // no base to set and no account segment that could read like a published page.
  assert.equal(filesHref("/"), "/files");
  assert.equal(filesHref(""), "/files");
  assert.equal(filesHref("Photos/a.jpg"), "/files/Photos/a.jpg");
  assert.equal(filesHref("/Photos/a.jpg"), "/files/Photos/a.jpg");
  assert.equal(filesRoutePath("/"), "");
  assert.equal(filesRoutePath("/files"), "");
  assert.equal(filesRoutePath("/files/"), "");
  assert.equal(filesRoutePath("/files/Photos/a.jpg"), "Photos/a.jpg");
});

test("authored HTML permits same-origin data and blob media while blocking remote sources", () => {
  const directives = new Map(htmlCardPolicy("https://drive.example:8443").split(";").map((entry) => {
    const [name, ...sources] = entry.trim().split(/\s+/);
    return [name, sources];
  }));
  assert.deepEqual(directives.get("default-src"), ["'none'"]);
  assert.deepEqual(directives.get("img-src"), ["https://drive.example:8443", "data:", "blob:"]);
  assert.deepEqual(directives.get("media-src"), ["https://drive.example:8443", "data:", "blob:"]);
  assert.deepEqual(directives.get("style-src"), ["'unsafe-inline'"]);
  assert.deepEqual(directives.get("font-src"), ["data:"]);
  assert.equal(htmlCardPolicy("http://192.168.1.2:8080").includes("img-src http://192.168.1.2:8080 data: blob:"), true);
});

// A compressed file opens the preview — the viewer chunk loads, inflates the stream and renders
// what was inside it (compression.ts). The name still decides: a compressible `.bin` is not.
test("a compression extension opens the preview while the name alone decides", () => {
  for (const name of ["session.v4.jsonl.zstd", "session.v4.jsonl.zst", "notes.txt.gz", "archive.GZ"]) {
    assert.equal(canPreviewFile(name), true, name);
  }
  assert.equal(canPreviewFile("backup.bin", "application/zstd"), false);
});

test("navigation decodes accepted names exactly once and preserves native spelling", () => {
  for (const path of ["Photos/a +#?.jpg", "Notes/literal%2F%25.txt", "Cafe\u0301/  name .txt", "健康/步数.json"]) {
    assert.equal(filesRoutePath(filesHref(`/${path}`)), path);
  }
});

// SVG and playlists have no plugin here, so they still fall back to download. HTML used to be in
// that list because a preview pointed the browser at the application origin, where an uploaded
// document would run as us; `htmlPlugin` now reads the bytes itself and hands them to an
// opaque-origin sandbox, which is the property those cases were really protecting.
test("formats without a plugin fall back to download while sandboxed HTML is previewed", () => {
  for (const name of ["active.svg", "playlist.m3u8"]) assert.equal(canPreviewFile(name), false);
  assert.equal(canPreviewFile("renamed.jpg", "image/svg+xml"), false);
  for (const name of ["report.html", "report.HTM", "report.htm"]) assert.equal(canPreviewFile(name, "text/html"), true);
  // A text file that merely declares an HTML type stays a text preview: htmlPlugin claims by
  // extension, so a declaration alone cannot turn a file into a rendered document.
  assert.equal(canPreviewFile("renamed.txt", "text/html; charset=utf-8"), true);
  for (const [name, mime] of [["a.jpg", "image/jpeg"], ["a.pdf", "application/pdf"], ["a.txt", "text/plain"], ["a.mp4", "video/mp4"]]) {
    assert.equal(canPreviewFile(name, mime), true);
  }
});
