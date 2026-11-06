import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const moduleURL = (code) => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
const source = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "client.ts");
let sequence = 0;
const response = (status, payload) => ({ status, ok: status >= 200 && status < 300, json: async () => payload, blob: async () => new Blob([JSON.stringify(payload)]) });
async function setup(t, handler) {
  const original = { fetch: globalThis.fetch, window: globalThis.window };
  const calls = [];
  globalThis.window = { location: { origin: "https://app.example" } };
  globalThis.fetch = async (url, init) => {
    calls.push({ url, init });
    if (url === "/api/files/me") return response(200, { csrfToken: "csrf-token" });
    return handler(url, init);
  };
  t.after(() => { globalThis.fetch = original.fetch; globalThis.window = original.window; });
  const client = await import(moduleURL(`${code}\n// download instance ${++sequence}`));
  return { ...client, calls };
}

test("read grants encode selectors once and carry cookie CSRF", async (t) => {
  const { api, calls } = await setup(t, async () => response(200, { items: [{ id: "f1", kind: "url", url: "https://bucket.example/private", headers: {}, expiresAt: "2026-11-06T12:00:00Z", size: 4, contentType: "text/plain", acceptRanges: true, conditions: [] }] }));
  const grants = await api.downloadGrants([{ path: "/Notes/ +%2F?#.md" }, { id: "f2" }], "inline");
  assert.equal(grants.items.length, 1);
  const post = calls.find(({ url }) => url === "/api/files/downloads");
  assert.ok(post);
  assert.equal(post.init.method, "POST");
  assert.equal(post.init.headers.get("X-CSRF-Token"), "csrf-token");
  assert.deepEqual(JSON.parse(post.init.body), { items: [{ path: "/Notes/ +%2F?#.md" }, { id: "f2" }], disposition: "inline" });
});

test("a class A url grant is returned unchanged for media", async (t) => {
  const { api } = await setup(t, async () => response(200, { items: [{ id: "f1", kind: "url", url: "https://bucket.example/private", headers: {}, expiresAt: "2026-11-06T12:00:00Z", size: 4, contentType: "text/plain", acceptRanges: true, conditions: [] }] }));
  assert.equal(await api.downloadURL("/Notes/a.md", "inline"), "https://bucket.example/private");
});

test("a bearer grant is fetched with the provider headers and never app credentials", async (t) => {
  const { api, calls } = await setup(t, async (url) => {
    if (url.startsWith("https://drive.example/")) return response(200, "bytes");
    return response(200, { items: [{ id: "f1", kind: "bearer", url: "https://drive.example/file", headers: { Authorization: "Bearer provider" }, expiresAt: "2026-11-06T12:00:00Z", size: 4, contentType: "text/plain", acceptRanges: true, conditions: [] }] });
  });
  const url = await api.downloadURL("/Notes/a.md", "inline");
  assert.ok(url.startsWith("blob:"));
  const provider = calls.find(({ url }) => url.startsWith("https://drive.example/"));
  assert.equal(provider.init.headers.Authorization, "Bearer provider");
  assert.equal(provider.init.credentials, "omit");
});

test("a per-item Problem is raised instead of a fake grant", async (t) => {
  const { api } = await setup(t, async () => response(200, { items: [{ type: "urn:eam:error:not_found", title: "Not Found", status: 404, code: "not_found", detail: "The file was not found." }] }));
  await assert.rejects(api.downloadGrant("/missing", "attachment"), { status: 404, code: "not_found" });
});
