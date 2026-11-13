import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const clientSource = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code: clientCode } = await transformWithOxc(clientSource, "client.ts");
let clientNumber = 0;
async function clientModule() {
  return import(`data:text/javascript;base64,${Buffer.from(`${clientCode}\n// instance ${++clientNumber}`).toString("base64")}`);
}

async function loadSource(path) {
  const source = await readFile(new URL(path, import.meta.url), "utf8");
  const { code } = await transformWithOxc(source, path);
  return import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);
}

const resource = (path, kind = "file") => ({
  id: `entry:${path}`, parentId: `entry:${path.slice(0, path.lastIndexOf("/")) || "/"}`,
  path, parentPath: path.slice(0, path.lastIndexOf("/")) || "/", name: path.split("/").pop(),
  kind, revision: "nr_1", contentRevision: kind === "file" ? "cr_1" : null,
  size: kind === "file" ? 3 : null, mediaType: kind === "file" ? "text/plain" : null,
  sourceModifiedAt: null, createdAt: "2026-11-01T00:00:00Z", updatedAt: "2026-11-01T00:00:00Z", checksum: null,
});
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const page = (paths, nextCursor) => ({ items: paths.map((path) => resource(path)), nextCursor });
const tick = () => new Promise((resolve) => setImmediate(resolve));

async function until(predicate, label) {
  for (let i = 0; i < 200; i++) {
    if (predicate()) return;
    await tick();
  }
  assert.fail(label);
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function mockFetch(t, handler) {
  const original = globalThis.fetch;
  globalThis.fetch = handler;
  t.after(() => { globalThis.fetch = original; });
}

test("the first page resolves before the last page does", async (t) => {
  const { openListing, listingRequestConcurrency } = await clientModule();
  const secondPage = deferred();
  const lastPage = deferred();
  const calls = [];
  let inFlight = 0;
  let peak = 0;
  mockFetch(t, async (input) => {
    const url = new URL(input, "https://drive.example");
    calls.push({ path: url.pathname, cursor: url.searchParams.get("cursor"), limit: url.searchParams.get("limit") });
    if (url.pathname === "/api/files") return json(resource("/Big", "folder"));
    const cursor = url.searchParams.get("cursor");
    if (!cursor) return json(page(["/Big/a", "/Big/b"], "cursor-2"));
    inFlight += 1;
    peak = Math.max(peak, inFlight);
    try {
      if (cursor === "cursor-2") {
        await secondPage.promise;
        return json(page(["/Big/c", "/Big/d"], "cursor-3"));
      }
      await lastPage.promise;
      return json(page(["/Big/e"], null));
    } finally {
      inFlight -= 1;
    }
  });

  const stream = openListing("/Big");
  const first = await stream.first;
  assert.deepEqual(first.items.map((item) => item.path), ["/Big/a", "/Big/b"]);

  const delivered = [];
  let settled = false;
  const rest = stream.rest((items) => delivered.push(...items)).then(() => { settled = true; });
  await until(() => calls.some((call) => call.cursor === "cursor-2"), "the second page should start");
  // The first page has already resolved, and the folder is still waiting on the rest.
  assert.equal(delivered.length, 0);
  assert.equal(settled, false);

  secondPage.resolve();
  await until(() => delivered.length === 2, "the second page should arrive");
  assert.equal(settled, false);
  await until(() => calls.some((call) => call.cursor === "cursor-3"), "the third page should start");
  assert.equal(settled, false);

  lastPage.resolve();
  await rest;
  assert.equal(settled, true);
  assert.deepEqual(delivered.map((item) => item.path), ["/Big/c", "/Big/d", "/Big/e"]);
  // One cursor chain is one request at a time; continuation pages ask for the API maximum.
  assert.deepEqual(calls.filter((call) => call.cursor).map((call) => call.limit), ["1000", "1000"]);
  assert.equal(peak, 1);
  assert.ok(peak <= listingRequestConcurrency);
});

test("page reads across listings stay inside the derived request bound", async (t) => {
  const { openListing, listingRequestConcurrency } = await clientModule();
  const gate = deferred();
  let inFlight = 0;
  let peak = 0;
  mockFetch(t, async (input) => {
    const url = new URL(input, "https://drive.example");
    if (url.pathname === "/api/files") return json(resource(url.searchParams.get("path"), "folder"));
    const cursor = url.searchParams.get("cursor");
    if (!cursor) return json(page([`${url.searchParams.get("path")}/a`], `next:${url.searchParams.get("path")}`));
    inFlight += 1;
    peak = Math.max(peak, inFlight);
    try {
      await gate.promise;
      return json(page([`${url.searchParams.get("path")}/b`], null));
    } finally {
      inFlight -= 1;
    }
  });

  // One open folder plus hover prefetches: more streams than the pool admits, all resting
  // at once. The bound is the client's, not a property of a single cursor chain.
  const streams = [];
  for (let i = 0; i < listingRequestConcurrency * 3; i++) {
    const stream = openListing(`/Folder${i}`);
    await stream.first;
    streams.push(stream);
  }
  const rests = streams.map((stream) => stream.rest(() => undefined));
  await until(() => peak === listingRequestConcurrency, "the request pool should fill");
  assert.ok(peak <= listingRequestConcurrency, `peak ${peak} exceeded bound ${listingRequestConcurrency}`);

  gate.resolve();
  await Promise.all(rests);
  assert.ok(peak <= listingRequestConcurrency);
  assert.equal(peak, listingRequestConcurrency);
});

test("hovering during the first read joins it instead of issuing a duplicate", async (t) => {
  const { openListing, prefetchListing } = await clientModule();
  const gate = deferred();
  const calls = [];
  mockFetch(t, async (input) => {
    const url = new URL(input, "https://drive.example");
    calls.push(url.pathname);
    if (url.pathname === "/api/files") return json(resource("/Photos", "folder"));
    await gate.promise;
    return json(page(["/Photos/a"], null));
  });

  // Two hovers in the same turn are one prefetch, and opening the folder consumes that
  // request rather than starting a second metadata/list pair.
  prefetchListing("/Photos");
  prefetchListing("/Photos");
  await until(() => calls.length === 2, "the hover should request metadata and one page");
  assert.equal(calls.length, 2);
  const stream = openListing("/Photos");
  prefetchListing("/Photos");
  await tick();
  assert.equal(calls.length, 2);
  gate.resolve();
  const first = await stream.first;
  assert.equal(first.items.length, 1);
  assert.equal(calls.length, 2);
});

test("hovering while the rest streams cannot re-request the folder", async (t) => {
  const { openListing, prefetchListing } = await clientModule();
  const gate = deferred();
  let calls = 0;
  mockFetch(t, async (input) => {
    const url = new URL(input, "https://drive.example");
    calls += 1;
    if (url.pathname === "/api/files") return json(resource("/Big", "folder"));
    if (!url.searchParams.has("cursor")) return json(page(["/Big/a"], "cursor-2"));
    await gate.promise;
    return json(page(["/Big/b"], null));
  });

  const stream = openListing("/Big");
  await stream.first;
  const delivered = [];
  const rest = stream.rest((items) => delivered.push(...items));
  await until(() => calls === 3, "the continuation should be in flight");
  const before = calls;
  prefetchListing("/Big");
  prefetchListing("/Big");
  await tick();
  // The page is already reading this folder; a hover must join that read, not warm a new one.
  assert.equal(calls, before);
  gate.resolve();
  await rest;
  assert.deepEqual(delivered.map((item) => item.path), ["/Big/b"]);
  assert.equal(calls, before);
});

test("a 2000-entry folder mounts one bounded chunk before the sentinel asks for more", async () => {
  const { LIST_CHUNK_SIZE, chunkRows, growRowCount, visibleRowCount } = await loadSource("../src/pages/listChunk.ts");
  const entries = Array.from({ length: 2000 }, (_, index) => ({ id: `entry-${index}`, path: `/Big/${index}` }));

  const firstPaint = chunkRows(entries, LIST_CHUNK_SIZE);
  assert.equal(firstPaint.length, LIST_CHUNK_SIZE);
  assert.ok(firstPaint.length < entries.length);

  let rendered = LIST_CHUNK_SIZE;
  let steps = 0;
  while (rendered < entries.length) {
    const next = growRowCount(rendered, entries.length);
    assert.equal(next - rendered, LIST_CHUNK_SIZE);
    assert.ok(chunkRows(entries, next).length <= next);
    rendered = next;
    steps += 1;
  }
  assert.equal(rendered, entries.length);
  assert.equal(steps, 19);
  assert.equal(visibleRowCount(entries.length, 150), 150);
  assert.equal(visibleRowCount(entries.length, entries.length + 100), entries.length);
  assert.equal(chunkRows(entries, entries.length).length, entries.length);
});
