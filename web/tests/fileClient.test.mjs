import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "client.ts");
let moduleNumber = 0;
async function client() {
  return import(`data:text/javascript;base64,${Buffer.from(`${code}\n// instance ${++moduleNumber}`).toString("base64")}`);
}
const resource = (path, kind = "file") => ({
  path, parentPath: path.slice(0, path.lastIndexOf("/")) || "/", name: path.split("/").pop(),
  kind, revision: "nr_1", contentRevision: kind === "file" ? "cr_1" : null,
  size: kind === "file" ? 3 : null, mediaType: kind === "file" ? "text/plain" : null,
  sourceModifiedAt: null, createdAt: "2026-10-30T00:00:00Z", updatedAt: "2026-10-30T00:00:00Z", checksum: null,
});
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const problem = (status, code, operationId) => ({ type: `urn:eam:error:${code}`, title: code, status, code, detail: "Recover the saved operation.", requestId: "req_test", operationId });

function mockFetch(t, handler) {
  const original = globalThis.fetch;
  globalThis.fetch = handler;
  t.after(() => { globalThis.fetch = original; });
}

test("file paths and byte URLs preserve accepted names and encode query values once", async () => {
  const { api, filePath } = await client();
  assert.equal(filePath(""), "/");
  assert.equal(filePath("Notes/a.txt"), "/Notes/a.txt");
  const name = "/Photos/cafe\u0301 +#?%2F.svg";
  for (const href of [api.contentURL(name), api.thumbnailURL(name), api.downloadHref(name)]) {
    const url = new URL(href, "https://drive.example");
    assert.equal(url.origin, "https://drive.example");
    assert.equal(url.searchParams.get("path"), name);
    assert.ok(url.pathname.startsWith("/api/files/"));
  }
  assert.equal(new URL(api.downloadHref(name), "https://drive.example").searchParams.get("disposition"), "attachment");
});

test("navigation composes root metadata and every page without file IDs or envelopes", async (t) => {
  const { api } = await client();
  const calls = [];
  mockFetch(t, async (input) => {
    const url = new URL(input, "https://drive.example");
    calls.push(url);
    assert.equal(url.searchParams.get("path"), "/Photos");
    if (url.pathname === "/api/files") return json(resource("/Photos", "folder"));
    if (!url.searchParams.has("cursor")) return json({ items: [resource("/Photos/a.jpg")], nextCursor: "opaque+cursor" });
    assert.equal(url.searchParams.get("cursor"), "opaque+cursor");
    return json({ items: [resource("/Photos/a.jpg"), resource("/Photos/b.jpg")], nextCursor: null });
  });
  const listing = await api.list("Photos");
  assert.deepEqual(listing.items.map((file) => file.path), ["/Photos/a.jpg", "/Photos/b.jpg"]);
  assert.deepEqual(listing.breadcrumb, [{ name: "", path: "/" }, { name: "Photos", path: "/Photos" }]);
  assert.equal(listing.node.kind, "folder");
  assert.equal(listing.path, "/Photos");
  assert.equal(calls.length, 3);
});

test("file detail avoids listing children and missing paths remain distinguishable", async (t) => {
  const { api } = await client();
  const calls = [];
  mockFetch(t, async (input) => {
    const url = new URL(input, "https://drive.example");
    calls.push(url.pathname);
    return url.searchParams.get("path") === "/missing" ? json(problem(404, "not_found"), 404) : json(resource("/a.txt"));
  });
  assert.equal((await api.list("a.txt")).node.kind, "file");
  assert.equal((await api.list("missing")).node, null);
  assert.deepEqual(calls, ["/api/files", "/api/files"]);
});

test("Problem errors preserve code and operation identity without cookie fallback", async (t) => {
  const { api, ApiError, FileApiError } = await client();
  let count = 0;
  mockFetch(t, async () => { count++; return json(problem(409, "operation_in_progress", "op_saved"), 409); });
  await assert.rejects(api.metadata("/blocked"), (error) => {
    assert.ok(error instanceof FileApiError);
    assert.ok(error instanceof ApiError);
    assert.equal(error.code, "operation_in_progress");
    assert.equal(error.operationId, "op_saved");
    return true;
  });
  assert.equal(count, 1);
});

test("cookie mutations acquire CSRF once and use unified path and revision contracts", async (t) => {
  const { api } = await client();
  const calls = [];
  mockFetch(t, async (input, init) => {
    calls.push({ url: new URL(input, "https://drive.example"), init });
    assert.equal(init.credentials, "same-origin");
    if (input === "/api/files/me") return json({ csrfToken: "session-token" });
    assert.equal(init.headers.get("X-CSRF-Token"), "session-token");
    if (init.method === "DELETE") return new Response(null, { status: 204 });
    return json(resource("/Notes/new.txt"), 201);
  });
  await api.mkdir("Notes");
  await api.move("Notes/a.txt", "/Notes/new.txt", "nr_observed");
  await api.copy("/Notes/new.txt", "copy.txt", "nr_copy");
  await api.deletePath("Notes", true, "nr_delete");
  assert.equal(calls.filter(({ url }) => url.pathname.endsWith("/me")).length, 1);
  assert.deepEqual(JSON.parse(calls[1].init.body), { kind: "folder", path: "/Notes", parents: false });
  assert.deepEqual(JSON.parse(calls[2].init.body), { from: "/Notes/a.txt", to: "/Notes/new.txt", ifRevision: "nr_observed" });
  assert.equal(calls[2].init.headers.get("Prefer"), "respond-async");
  assert.deepEqual(JSON.parse(calls[3].init.body), { from: "/Notes/new.txt", to: "/copy.txt", ifSourceRevision: "nr_copy" });
  assert.equal(calls[4].url.searchParams.get("recursive"), "true");
  assert.equal(calls[4].init.headers.get("If-Revision"), "nr_delete");
});

test("session creation retains its idempotency key and returns direct workflow resources", async (t) => {
  const { api } = await client();
  const calls = [];
  const operation = { id: "op_commit", kind: "upload_commit", state: "outcome_unknown" };
  mockFetch(t, async (input, init) => {
    if (input === "/api/files/me") return json({ csrfToken: "token" });
    calls.push({ input, init });
    return input.endsWith("/commit") ? json(operation, 202) : json({ id: "u_saved", state: "open", path: "/Notes/new.txt" }, 201);
  });
  const signal = new AbortController().signal;
  const session = await api.createUpload({ path: "Notes/new.txt", size: 3, mediaType: "text/plain", condition: { mode: "replace", contentRevision: "cr_old" }, sourceModifiedAt: "2026-10-30T00:00:00Z" }, "create-saved", signal);
  assert.equal(session.id, "u_saved");
  assert.equal(calls[0].init.headers.get("Idempotency-Key"), "create-saved");
  assert.equal(calls[0].init.signal, signal);
  assert.equal(JSON.parse(calls[0].init.body).path, "/Notes/new.txt");
  assert.deepEqual(await api.commitUpload(session.id), operation);
});

test("part progress does not acknowledge bytes until the plain durable receipt arrives", async (t) => {
  const { api } = await client();
  mockFetch(t, async () => json({ csrfToken: "token" }));
  const original = globalThis.XMLHttpRequest;
  let xhr;
  globalThis.XMLHttpRequest = class {
    constructor() { xhr = this; this.upload = {}; this.headers = new Map(); }
    open(method, url) { this.method = method; this.url = url; }
    setRequestHeader(name, value) { this.headers.set(name, value); }
    send(blob) { this.blob = blob; }
    abort() { this.onabort?.(); }
  };
  t.after(() => { globalThis.XMLHttpRequest = original; });
  const progress = [];
  let acknowledged = false;
  const result = api.uploadPart("u+saved", 2, new Blob(["abc"]), undefined, (value) => progress.push(value)).then((receipt) => { acknowledged = true; return receipt; });
  while (!xhr?.blob) await new Promise((resolve) => setImmediate(resolve));
  assert.equal(xhr.method, "PUT");
  assert.equal(xhr.url, "/api/files/uploads/u%2Bsaved/parts/2");
  assert.equal(xhr.headers.get("X-CSRF-Token"), "token");
  xhr.upload.onprogress({ loaded: 3 });
  assert.deepEqual(progress, [{ loaded: 3, total: 3 }]);
  assert.equal(acknowledged, false);
  const receipt = { number: 2, size: 3, sha256: "abc123" };
  xhr.status = 201;
  xhr.responseText = JSON.stringify(receipt);
  xhr.onload();
  assert.deepEqual(await result, receipt);
});
