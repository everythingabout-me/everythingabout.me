import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "client.ts");
let instance = 0;
const client = () => import(`data:text/javascript;base64,${Buffer.from(`${code}\n// identity ${++instance}`).toString("base64")}`);
const fileID = "f7ca1104-12f0-4bbf-8c77-41d341f7d120";
const folderID = "650ff3fa-efea-4dcb-83ec-733748289eb0";
const rootID = "73ea6c34-2c46-4349-b3b8-d5fb7be0fb7c";
const resource = (id = fileID, path = "/Notes/a.txt", kind = "file") => ({
  id, parentId: path === "/" ? null : folderID, parentPath: path === "/" ? null : path.slice(0, path.lastIndexOf("/")) || "/",
  path, name: path.split("/").pop(), kind, revision: "nr_saved", contentRevision: kind === "file" ? "cr_saved" : null,
  size: kind === "file" ? 3 : null, mediaType: kind === "file" ? "text/plain" : null,
  sourceModifiedAt: null, createdAt: "2026-11-04T00:00:00Z", updatedAt: "2026-11-04T00:00:00Z", checksum: null,
});
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
function capture(t, respond = () => json(resource())) {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (input, init) => {
    const call = { url: new URL(input, "https://drive.example"), init };
    calls.push(call);
    return input === "/api/files/me" ? json({ csrfToken: "session-token" }) : respond(call);
  };
  t.after(() => { globalThis.fetch = original; });
  return calls;
}
const invalid = (error) => error.status === 400 && error.code === "invalid_selector";

test("ID reads select directly at the shared endpoint without a path lookup or ID cache", async (t) => {
  const { files } = await client();
  const calls = capture(t);
  const signal = new AbortController().signal;
  const result = await files.metadata({ id: fileID }, signal);
  assert.equal(result.id, fileID);
  assert.equal(result.parentId, folderID);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].url.pathname, "/api/files");
  assert.deepEqual([...calls[0].url.searchParams], [["id", fileID]]);
  assert.equal(calls[0].init.signal, signal);
  assert.equal(calls[0].init.credentials, "same-origin");
});

test("list, search and capabilities carry exactly the selected ID or path", async (t) => {
  const { files, api } = await client();
  const calls = capture(t, () => json({ items: [], nextCursor: null }));
  await files.list({ id: folderID }, { limit: 17, cursor: "cursor+#?%2F" });
  await files.search({ q: "a+#?%2F", id: folderID, kind: "file", limit: 3 });
  await api.search("needle", { id: folderID });
  await files.capabilities({ id: folderID });
  for (const call of calls) {
    assert.equal(call.url.searchParams.get("id"), folderID);
    assert.equal(call.url.searchParams.has("path"), false);
  }
  assert.equal(calls[0].url.searchParams.get("cursor"), "cursor+#?%2F");
  assert.equal(calls[0].url.searchParams.get("limit"), "17");
  assert.equal(calls[1].url.searchParams.get("q"), "a+#?%2F");
  await files.search({ q: "all" });
  await files.capabilities();
  for (const call of calls.slice(-2)) {
    assert.equal(call.url.searchParams.has("id"), false);
    assert.equal(call.url.searchParams.has("path"), false);
  }
});

test("content URLs support either selector and encode accepted path spelling exactly once", async () => {
  const { api, files } = await client();
  const path = "/Notes/cafe\u0301 +#?%2F.txt";
  for (const make of [api.contentURL, api.thumbnailURL, api.downloadHref]) {
    const byID = new URL(make({ id: fileID }), "https://drive.example");
    assert.equal(byID.searchParams.get("id"), fileID);
    assert.equal(byID.searchParams.has("path"), false);
    for (const selector of [path, { path }]) {
      const byPath = new URL(make(selector), "https://drive.example");
      assert.equal(byPath.searchParams.get("path"), path);
      assert.equal(byPath.searchParams.has("id"), false);
      assert.ok(byPath.search.includes("%252F"));
    }
  }
  assert.equal(new URL(files.contentURL({ id: fileID }, "inline"), "https://drive.example").searchParams.get("disposition"), "inline");
});

test("ID folder navigation retains selector across pages and deduplicates moved entries by stable ID", async (t) => {
  const { api } = await client();
  const calls = capture(t, ({ url }) => {
    assert.equal(url.searchParams.get("id"), folderID);
    assert.equal(url.searchParams.has("path"), false);
    if (url.pathname === "/api/files") return json(resource(folderID, "/Current", "folder"));
    if (!url.searchParams.has("cursor")) return json({ items: [resource(fileID, "/Current/old.txt")], nextCursor: "next" });
    return json({ items: [resource(fileID, "/Current/new.txt"), resource(rootID, "/Current/old.txt")], nextCursor: null });
  });
  const listing = await api.list({ id: folderID });
  assert.equal(listing.path, "/Current");
  assert.deepEqual(listing.breadcrumb, [{ name: "", path: "/" }, { name: "Current", path: "/Current" }]);
  assert.deepEqual(listing.items.map(({ id, path }) => ({ id, path })), [
    { id: fileID, path: "/Current/new.txt" }, { id: rootID, path: "/Current/old.txt" },
  ]);
  assert.equal(calls.length, 3);
});

test("retired ID navigation preserves not_found and never invents a destination", async (t) => {
  const { api, FileApiError } = await client();
  const calls = capture(t, () => json({ type: "urn:eam:error:not_found", title: "Not found", status: 404, code: "not_found", detail: "Not found.", requestId: "req_test" }, 404));
  await assert.rejects(api.list({ id: fileID }), (error) => error instanceof FileApiError && error.status === 404);
  // The speculative first page rides along with the metadata read, so a vanished ID costs one
  // round trip rather than a metadata wait followed by a page read.
  assert.equal(calls.length, 2);
  for (const { url } of calls) assert.deepEqual([...url.searchParams], [["id", fileID]]);
});

test("root and scoped-root metadata retain a real ID while concealed parent fields stay null", async (t) => {
  const { api } = await client();
  capture(t, ({ url }) => json(url.searchParams.get("id") === rootID ? resource(rootID, "/", "folder")
    : { ...resource(folderID, "/Apps/Notes", "folder"), parentId: null, parentPath: null }));
  for (const id of [rootID, folderID]) {
    const result = await api.metadata({ id });
    assert.equal(result.id, id);
    assert.equal(result.parentId, null);
    assert.equal(result.parentPath, null);
  }
});

test("parent destinations, mixed source forms and ID deletion retain CSRF and revision conditions", async (t) => {
  const { api } = await client();
  const calls = capture(t, ({ init }) => init.method === "DELETE" ? new Response(null, { status: 204 }) : json(resource()));
  const destination = { parent: { id: folderID }, name: "new +#?%2F.txt" };
  await api.mkdir({ parent: { path: "Notes" }, name: "2026" });
  await api.move({ id: fileID }, destination, "nr_move");
  await api.copy("Notes/a.txt", { path: "Archive/copy.txt" }, "nr_copy");
  await api.deletePath({ id: fileID }, true, "nr_delete");
  const writes = calls.filter(({ init }) => init.method);
  assert.deepEqual(JSON.parse(writes[0].init.body), { kind: "folder", destination: { parent: { path: "/Notes" }, name: "2026" }, parents: false });
  assert.deepEqual(JSON.parse(writes[1].init.body), { from: { id: fileID }, to: destination, ifRevision: "nr_move" });
  assert.deepEqual(JSON.parse(writes[2].init.body), { from: "/Notes/a.txt", to: { path: "/Archive/copy.txt" }, ifSourceRevision: "nr_copy" });
  assert.deepEqual([...writes[3].url.searchParams], [["id", fileID], ["recursive", "true"]]);
  assert.equal(writes[3].init.headers.get("If-Revision"), "nr_delete");
  for (const { init } of writes) assert.equal(init.headers.get("X-CSRF-Token"), "session-token");
  assert.equal(writes[1].init.headers.get("Prefer"), "respond-async");
  assert.equal(calls.filter(({ url }) => url.pathname.endsWith("/me")).length, 1);
});

test("uploads distinguish creation destinations from replacement selectors and preserve request identity", async (t) => {
  const { api } = await client();
  const calls = capture(t, () => json({ id: "session", path: "/Notes/a.txt", state: "open" }, 201));
  const common = { size: 3, mediaType: "text/plain", sourceModifiedAt: "2026-11-04T00:00:00Z" };
  const signal = new AbortController().signal;
  await api.createUpload({ ...common, destination: { parent: { id: folderID }, name: "a.txt" } }, "create-by-parent", signal);
  await api.createUpload({ ...common, target: { id: fileID }, condition: { mode: "replace", contentRevision: "cr_observed" } }, "replace-by-id", signal);
  await api.createUpload({ ...common, target: { path: "Notes/a.txt" }, condition: { mode: "replace", contentRevision: "cr_observed" } }, "replace-by-path", signal);
  await api.createUpload({ ...common, destination: { path: "Notes/new.txt" }, condition: { mode: "create" } }, "create-by-path", signal);
  const writes = calls.filter(({ init }) => init.method === "POST");
  assert.deepEqual(JSON.parse(writes[0].init.body), { ...common, destination: { parent: { id: folderID }, name: "a.txt" } });
  assert.deepEqual(JSON.parse(writes[1].init.body), { ...common, target: { id: fileID }, condition: { mode: "replace", contentRevision: "cr_observed" } });
  assert.deepEqual(JSON.parse(writes[2].init.body).target, { path: "/Notes/a.txt" });
  assert.deepEqual(JSON.parse(writes[3].init.body).destination, { path: "/Notes/new.txt" });
  for (let i = 0; i < writes.length; i++) {
    assert.equal(writes[i].init.headers.get("Idempotency-Key"), ["create-by-parent", "replace-by-id", "replace-by-path", "create-by-path"][i]);
    assert.equal(writes[i].init.signal, signal);
    assert.equal(writes[i].init.headers.get("X-CSRF-Token"), "session-token");
    assert.equal(Object.hasOwn(JSON.parse(writes[i].init.body), "path"), false);
  }
});

test("ambiguous, empty and malformed selectors fail before any lookup or CSRF request", async (t) => {
  const { api, files } = await client();
  const calls = capture(t);
  const bad = [{}, { id: fileID, path: "/a" }, { id: "" }, { path: "" }, { id: null }, { id: fileID, extra: true }, null, []];
  for (const selector of bad) {
    for (const call of [() => api.metadata(selector), () => files.list(selector), () => api.contentURL(selector),
      () => api.thumbnailURL(selector), () => api.deletePath(selector), () => api.capabilities(selector)]) {
      assert.throws(call, invalid);
    }
    await assert.rejects(api.list(selector), invalid);
  }
  assert.throws(() => files.search({ q: "x", id: folderID, path: "/Notes" }), invalid);
  assert.throws(() => files.search({ q: "x", id: undefined }), invalid);
  assert.equal(calls.length, 0);
});

test("mixed destinations and wrong upload modes fail before any mutation or CSRF bootstrap", async (t) => {
  const { api } = await client();
  const calls = capture(t);
  for (const destination of [{ id: fileID }, {}, { path: "/a", parent: { id: folderID }, name: "a" },
    { parent: { id: folderID, path: "/Notes" }, name: "a" }, { parent: { id: folderID }, name: "" },
    { parent: folderID, name: "a" }, { parent: { id: folderID } }]) {
    assert.throws(() => api.mkdir(destination), invalid);
    assert.throws(() => api.move({ id: fileID }, destination, "nr_saved"), invalid);
    assert.throws(() => api.copy({ path: "/a" }, destination, "nr_saved"), invalid);
  }
  assert.throws(() => api.mkdir({ parent: { id: folderID }, name: "a" }, true), invalid);
  for (const input of [
    { path: "/a", destination: { path: "/a" } },
    { target: { id: fileID }, destination: { path: "/a" }, condition: { mode: "replace", contentRevision: "cr_saved" } },
    { target: { id: fileID } },
    { target: { id: fileID }, condition: { mode: "create" } },
    { destination: { path: "/a" }, condition: { mode: "replace", contentRevision: "cr_saved" } },
    { target: { id: fileID, path: "/a" }, condition: { mode: "replace", contentRevision: "cr_saved" } },
    { target: { id: fileID }, condition: { mode: "replace" } },
    { destination: { path: "/a" }, condition: { mode: "unknown" } },
  ]) assert.throws(() => api.createUpload({ size: 3, mediaType: "text/plain", ...input }, "key"), invalid);
  assert.equal(calls.length, 0);
});

test("redacted parents are safe in upload-row projection", async () => {
  const rowsSource = await readFile(new URL("../src/pages/uploadRows.ts", import.meta.url), "utf8");
  const transformed = await transformWithOxc(rowsSource, "uploadRows.ts");
  const { buildUploadRows } = await import(`data:text/javascript;base64,${Buffer.from(transformed.code).toString("base64")}`);
  assert.deepEqual(buildUploadRows([{ ...resource(), parentId: null, parentPath: null }], [], "Notes"), []);
});
