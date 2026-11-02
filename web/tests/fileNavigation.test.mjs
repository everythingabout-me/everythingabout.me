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

test("root and absolute resources map to the existing relative SPA route", () => {
  assert.equal(filesHref("/"), "/files");
  assert.equal(filesHref(""), "/files");
  assert.equal(filesHref("/Photos/a.jpg"), "/files/Photos/a.jpg");
  assert.equal(filesRoutePath("/files"), "");
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

test("navigation decodes accepted names exactly once and preserves native spelling", () => {
  for (const path of ["Photos/a +#?.jpg", "Notes/literal%2F%25.txt", "Cafe\u0301/  name .txt", "健康/步数.json"]) {
    assert.equal(filesRoutePath(filesHref(`/${path}`)), path);
  }
});

test("active and playlist media use the download fallback", () => {
  for (const name of ["active.svg", "active.html", "active.htm", "playlist.m3u8"]) assert.equal(canPreviewFile(name), false);
  assert.equal(canPreviewFile("renamed.jpg", "image/svg+xml"), false);
  assert.equal(canPreviewFile("renamed.txt", "text/html; charset=utf-8"), false);
  for (const [name, mime] of [["a.jpg", "image/jpeg"], ["a.pdf", "application/pdf"], ["a.txt", "text/plain"], ["a.mp4", "video/mp4"]]) {
    assert.equal(canPreviewFile(name, mime), true);
  }
});
