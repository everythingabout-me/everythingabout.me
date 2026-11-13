import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

// The listing reducer is the whole of the pages's coupled listing state (path, rows,
// current entry, breadcrumbs, loading/error and the committed search). It is pure, so it is
// tested directly; the page it replaced had no test at all.
const chunkSource = await readFile(new URL("../src/pages/listChunk.ts", import.meta.url), "utf8");
const chunkModule = await transformWithOxc(chunkSource, "listChunk.ts");
const chunkURL = `data:text/javascript;base64,${Buffer.from(chunkModule.code).toString("base64")}`;
const source = (await readFile(new URL("../src/pages/listingState.ts", import.meta.url), "utf8"))
  .replace('from "./listChunk"', `from "${chunkURL}"`);
const { code } = await transformWithOxc(source, "listingState.ts");
const {
  crumbsForPath,
  initialListingState,
  joinPath,
  listingKey,
  listingReducer,
  mergeListingItems,
} = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

const file = (path, name = path.split("/").at(-1)) => ({
  id: `id:${path}`,
  parentId: null,
  path,
  parentPath: path.slice(0, path.lastIndexOf("/")) || "/",
  name,
  kind: "file",
  revision: "entry-v1",
  contentRevision: "content-v1",
  size: 3,
  mediaType: "text/plain",
  sourceModifiedAt: null,
  createdAt: "2024-01-01T00:00:00Z",
  updatedAt: "2024-01-01T00:00:00Z",
  checksum: null,
});
const folder = (path, revision = "entry-v1") => ({ ...file(path), kind: "folder", size: null, contentRevision: null, revision });
const run = (state, action) => listingReducer(state, action);

test("a first answer replaces the skeleton and clears the search", () => {
  const state = { ...initialListingState(), search: "photos", query: "photos" };
  const next = run(state, {
    type: "loaded", path: "Photos", items: [file("Photos/a.jpg")], current: folder("Photos"), crumbs: [{ name: "Photos", path: "/Photos" }],
  });
  assert.equal(next.loading, false);
  assert.equal(next.path, "Photos");
  assert.deepEqual(next.items.map((entry) => entry.path), ["Photos/a.jpg"]);
  assert.equal(next.current.path, "Photos");
  assert.deepEqual(next.crumbs, [{ name: "Photos", path: "/Photos" }]);
  assert.equal(next.query, "");
  assert.equal(next.search, "");
  assert.equal(next.rows, 100);
  assert.equal(next.error, null);
});

test("a request keeps the outgoing listing until it answers", () => {
  const loaded = run(initialListingState(), {
    type: "loaded", path: "Photos", items: [file("Photos/a.jpg")], current: folder("Photos"), crumbs: [],
  });
  const loading = run(loaded, { type: "load" });
  assert.equal(loading.loading, true);
  assert.equal(loading.path, "Photos", "navigation keeps the old rows on screen");
  assert.equal(loading.items.length, 1);
  assert.equal(listingKey(loading), "Photos|");
});

test("a failed load names the path but keeps the committed search and its rows", () => {
  const loaded = run(initialListingState(), {
    type: "loaded", path: "Photos", items: [file("Photos/a.jpg")], current: folder("Photos"), crumbs: [],
  });
  const failed = run(loaded, { type: "failed", path: "Photos", error: "offline" });
  assert.equal(failed.error, "offline");
  assert.equal(failed.loading, false);
  assert.equal(failed.path, "Photos");
  assert.equal(failed.items, loaded.items, "a failed refresh is not an empty folder");
  assert.equal(failed.rows, 100);

  // A failure that changes the identity (a deep link into a folder that does not exist)
  // starts the new listing at one chunk, and never claims the old path.
  const moved = run(failed, { type: "failed", path: "Missing", error: "no such folder" });
  assert.equal(moved.path, "Missing");
  assert.equal(moved.rows, 100);
});

test("a continuation page merges by id, keeps order and prefers the newer entry", () => {
  const previous = [folder("Photos/A"), file("Photos/a.jpg", "a.jpg")];
  const next = mergeListingItems(previous, [
    { ...file("Photos/a.jpg", "a.jpg"), revision: "entry-v2" },
    file("Photos/b.jpg", "b.jpg"),
  ]);
  assert.deepEqual(next.map((entry) => entry.path), ["Photos/A", "Photos/a.jpg", "Photos/b.jpg"]);
  assert.equal(next[1].revision, "entry-v2");
  assert.equal(mergeListingItems(previous, []), previous, "an empty page is not a new array");
});

test("a search stands in for the folder and starts the row chunk over", () => {
  const loaded = run(initialListingState(), {
    type: "loaded", path: "Photos", items: [file("Photos/a.jpg")], current: folder("Photos"), crumbs: [],
  });
  const grown = run(loaded, { type: "grow-rows", total: 250 });
  assert.equal(grown.rows, 200, "one more chunk of rows");
  const searched = run(grown, {
    type: "searched", path: "Photos", query: "a", items: [file("Photos/a.jpg")], crumbs: crumbsForPath("Photos"),
  });
  assert.equal(searched.query, "a");
  assert.equal(searched.rows, 100, "a search result is a different listing");
  assert.equal(listingKey(searched), "Photos|a");
  assert.equal(searched.loading, false);
  assert.equal(searched.error, null);
});

test("a settlement replaces its own path and keeps the list order", () => {
  const loaded = run(initialListingState(), {
    type: "loaded", path: "Photos", items: [folder("Photos/A"), file("Photos/b.jpg", "b.jpg")], current: folder("Photos"), crumbs: [],
  });
  const next = run(loaded, { type: "item", item: { ...file("Photos/b.jpg", "b.jpg"), revision: "entry-v2" } });
  assert.deepEqual(next.items.map((entry) => entry.name), ["A", "b.jpg"]);
  assert.equal(next.items[1].revision, "entry-v2");
  const inserted = run(next, { type: "item", item: folder("Photos/0empty") });
  assert.deepEqual(inserted.items.map((entry) => entry.name), ["0empty", "A", "b.jpg"], "folders sort first");
});

test("a deleted folder takes its subtree and a deleted file only itself", () => {
  const loaded = run(initialListingState(), {
    type: "loaded",
    path: "",
    items: [folder("Photos"), file("Photos/a.jpg"), file("Photos2")],
    current: null,
    crumbs: [],
  });
  const removed = run(loaded, { type: "remove", path: "Photos", folder: true });
  assert.deepEqual(removed.items.map((entry) => entry.path), ["Photos2"], "a sibling with the prefix is not the subtree");
  const single = run(loaded, { type: "remove", path: "Photos/a.jpg", folder: false });
  assert.deepEqual(single.items.map((entry) => entry.path), ["Photos", "Photos2"]);
});

test("the revision update only touches the open folder", () => {
  const loaded = run(initialListingState(), {
    type: "loaded", path: "Photos", items: [], current: folder("Photos", "entry-v1"), crumbs: [],
  });
  const updated = run(loaded, { type: "revision", revision: "entry-v2" });
  assert.equal(updated.current.revision, "entry-v2");
  assert.equal(run(updated, { type: "revision", revision: "entry-v3" }).current.revision, "entry-v3");
  const fileState = run(initialListingState(), {
    type: "loaded", path: "a.txt", items: [], current: file("a.txt"), crumbs: [],
  });
  assert.equal(run(fileState, { type: "revision", revision: "nope" }), fileState, "a file is its own identity");
});

test("rows grow by one chunk, clamp to the list, and can be shown in full", () => {
  const loaded = run(initialListingState(), {
    type: "loaded", path: "", items: [], current: null, crumbs: [],
  });
  assert.equal(run(loaded, { type: "grow-rows", total: 250 }).rows, 200);
  assert.equal(run(loaded, { type: "grow-rows", total: 42 }).rows, 42);
  assert.equal(run(loaded, { type: "all-rows", total: 5_000 }).rows, 5_000);
});

test("the search box text is its own cell until the search is committed", () => {
  const typing = run(initialListingState(), { type: "search-text", value: "ph" });
  assert.equal(typing.search, "ph");
  assert.equal(typing.query, "", "typing is not a committed search");
  assert.equal(listingKey(typing), "|");
});

test("paths keep the route and the upload path in one spelling", () => {
  assert.deepEqual(crumbsForPath(""), [{ name: "", path: "/" }]);
  assert.deepEqual(crumbsForPath("/Photos/2024"), [
    { name: "", path: "/" },
    { name: "Photos", path: "/Photos" },
    { name: "2024", path: "/Photos/2024" },
  ]);
  assert.equal(joinPath("", "Photos"), "Photos");
  assert.equal(joinPath("Photos", "2024"), "Photos/2024");
});
