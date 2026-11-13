import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";
import { item, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const source = await readFile(new URL("../src/pages/uploadRevision.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "uploadRevision.ts");
const { FolderRevision, withinFolder } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

const entry = (path, bytes = "ab") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json" }) });
const folder = (path, revision) => ({ ...item(path, "folder", null), revision });

test("a loaded revision costs no read, and one invalidation reads it back once", async () => {
  let reads = 0;
  let current = "nr_loaded";
  const tracker = new FolderRevision(async () => { reads += 1; return folder("Photos", current); });
  tracker.seed("nr_loaded");
  assert.equal(await tracker.revision(), "nr_loaded");
  assert.equal(reads, 0, "a listing the page just loaded needs no read");
  current = "nr_after_upload";
  tracker.invalidate();
  assert.equal(await tracker.revision(), "nr_after_upload");
  assert.equal(reads, 1);
  assert.equal(await tracker.revision(), "nr_after_upload");
  assert.equal(reads, 1, "the refreshed value is kept until the next settle invalidates it");
});

test("settles that land while a read is in flight make it read again, and waiting mutations share it", async () => {
  let reads = 0;
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  const tracker = new FolderRevision(async () => {
    reads += 1;
    const reading = reads;
    await gate;
    // The second read observes the later settle; the first one is already out of date.
    return folder("Photos", `nr_${Math.min(reading, 2)}`);
  });
  tracker.seed("nr_0");
  tracker.invalidate();
  const mutation = tracker.revision();
  const second = tracker.revision();
  // A settle arrives while the first metadata read is outstanding.
  tracker.invalidate();
  release();
  assert.deepEqual(await Promise.all([mutation, second]), ["nr_2", "nr_2"]);
  assert.equal(reads, 2, "two mutations made one read in flight, and one late settle made it re-read");
});

test("a failed read keeps the last value and stays stale for the next mutation", async () => {
  let fail = true;
  const tracker = new FolderRevision(async () => {
    if (fail) throw new Error("index unavailable");
    return folder("Photos", "nr_new");
  });
  tracker.seed("nr_loaded");
  tracker.invalidate();
  assert.equal(await tracker.revision(), "nr_loaded", "the last known value is the only one available");
  fail = false;
  assert.equal(await tracker.revision(), "nr_new", "the next mutation retries instead of trusting the stale value");
});

/**
 * One settled import into /Photos whose commit answers carry the folder's revision, as a
 * current server's do. `parentRevision` is omitted to model an answer that does not.
 */
async function importIntoPhotos(t, { carriesRevision }) {
  // The server is the judge: the folder's revision advances when the upload settles into
  // it, exactly as the index's direct-child membership does, and a mutation naming an old
  // value is refused with 412 — the failure this page must not send.
  const server = uploadServer(2, { batch: true });
  let folderRevision = "nr_loaded";
  server.api.metadata = async (path) => { server.calls.push(["metadata", path]); return folder("/Photos", folderRevision); };
  const commit = server.api.commitUpload;
  server.api.commitUpload = async (id) => {
    const result = await commit(id);
    folderRevision = "nr_after_upload";
    return carriesRevision ? { ...result, parentPath: "/Photos", parentRevision: folderRevision } : result;
  };
  const tracker = new FolderRevision(() => server.api.metadata("/Photos"));
  tracker.seed("nr_loaded");
  // The page's settle handler: a settle directly in the open folder adopts the revision its
  // commit answer named; an answer without one leaves the folder stale.
  const harness = await setup(server.api, {
    onUploaded: (_task, result) => {
      if (withinFolder(result.path, "/Photos") && result.parentPath === "/Photos" && tracker.adopt(result.parentRevision)) return;
      tracker.invalidate();
    },
  });
  t.after(harness.unmount);
  harness.render().enqueue([entry("Photos/a")], "");
  // A settlement page that is not full leaves after its own 500 ms wait, so the test drives
  // that turn itself rather than waiting on a timer it did not schedule.
  let queue = harness.render();
  for (let attempt = 0; attempt < 100 && queue.tasks[0]?.status !== "completed"; attempt++) {
    queue = await harness.tick(500);
  }
  assert.equal(queue.tasks[0]?.status, "completed");
  assert.equal(harness.completed.length, 1);
  return { server, tracker, folderRevision: () => folderRevision };
}

test("a commit answer that names the folder revision costs no metadata read", async (t) => {
  const { server, tracker, folderRevision } = await importIntoPhotos(t, { carriesRevision: true });
  // The next rename or delete sends the value the commit answered; the pre-import value is
  // the reported 412, and nothing read the folder back to learn the new one.
  const mutate = async (revision) => {
    if (revision !== folderRevision()) throw new TestApiError("metadata changed", 412, { code: "precondition_failed" });
    return "renamed";
  };
  await assert.rejects(mutate("nr_loaded"), { status: 412 });
  assert.equal(await tracker.revision(), "nr_after_upload", "the settle answer is the folder's new revision");
  assert.equal(await mutate(await tracker.revision()), "renamed");
  assert.deepEqual(server.calls.filter(([kind]) => kind === "metadata"), [],
    "the commit answer is the read this field replaces");
});

test("an answer without the revision still reads the folder back once", async (t) => {
  // An older server, or a response that omits the field: the coalesced read remains the
  // fallback, and it still costs one request for the whole import rather than one per file.
  const { server, tracker, folderRevision } = await importIntoPhotos(t, { carriesRevision: false });
  const mutate = async (revision) => {
    if (revision !== folderRevision()) throw new TestApiError("metadata changed", 412, { code: "precondition_failed" });
    return "renamed";
  };
  assert.equal(await tracker.revision(), "nr_after_upload");
  assert.equal(await mutate(await tracker.revision()), "renamed");
  assert.deepEqual(server.calls.filter(([kind]) => kind === "metadata").map(([, path]) => path), ["/Photos"],
    "one folder read for the whole import, not one per settled file");
});

test("withinFolder treats the root as everything and does not match a sibling prefix", () => {
  assert.equal(withinFolder("/Photos/a.jpg", ""), true);
  assert.equal(withinFolder("/Photos/a.jpg", "/Photos"), true);
  assert.equal(withinFolder("/Photos", "/Photos"), true);
  assert.equal(withinFolder("/Photos2/a.jpg", "/Photos"), false);
});
