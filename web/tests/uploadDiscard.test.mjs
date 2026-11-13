import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";
import { deferred, item, operation, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

// The row projection is what a person reads after the deletion: the same module the page
// imports, so these tests assert the counters and the failure list the sheet would show.
const rowsSource = await readFile(new URL("../src/pages/uploadRows.ts", import.meta.url), "utf8");
const { code: rowsCode } = await transformWithOxc(rowsSource, "uploadRows.ts");
const { buildUploadRows, failedTasks } = await import(`data:text/javascript;base64,${Buffer.from(rowsCode).toString("base64")}`);

const entry = (path, bytes = "ab") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json", lastModified: 1443268800000 }) });
const folder = (path) => ({ path, file: null });
const allComplete = (queue) => queue.tasks.length > 0 && queue.tasks.every((task) => task.status === "completed");
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
// The page's own selection for an accepted folder delete, in the queue's relative spelling.
const affectedIds = (queue, target) => queue.tasks
  .filter((task) => task.path === target || task.path.startsWith(`${target}/`))
  .map((task) => task.id);
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }
function enqueue(harness, entries, dir = "") { harness.render().enqueue(entries, dir); }

test("deleting a folder after its import drops every completed row without a request", async (t) => {
  const server = uploadServer(2);
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/one"), entry("folder/nested/two"), folder("folder/empty")]);
  let queue = await harness.until(allComplete, "the import lands before the deletion");
  assert.deepEqual(queue.tasks.map(({ status }) => status), ["completed", "completed", "completed"]);
  const requests = server.calls.length;

  const remaining = queue.discard(affectedIds(queue, "folder"));
  assert.deepEqual(remaining, [], "every row's server session is over, so none is handed back");
  assert.deepEqual(queue.discard(affectedIds(queue, "folder")), [],
    "a row that already left is not reported as still live and does not need cancelling");
  assert.deepEqual(queue.discard(["unknown"]), [], "an unknown id is not a row to cancel");

  queue = await harness.settle();
  assert.deepEqual(queue.tasks, [], "the delete covered every row");
  assert.equal(queue.hasActiveUploads, false);
  assert.equal(harness.listeners.get("beforeunload")?.size ?? 0, 0, "the tab guard leaves with the last active row");
  assert.equal(server.calls.length, requests, "discard sends no abort, no status read, no request at all");
  assert.equal(harness.completed.length, 3, "each landed file was reported once, and discard re-reports nothing");
  assert.deepEqual(failedTasks(queue.tasks), []);
  assert.deepEqual(buildUploadRows([], queue.tasks, ""), [], "the projection no longer holds the deleted folder");
});

test("a row still in flight is cancelled for real while its finished sibling only leaves", async (t) => {
  const server = uploadServer(2);
  const partGate = deferred();
  let livePart = false;
  const uploadPart = server.api.uploadPart;
  server.api.uploadPart = async (id, number, blob, signal, progress) => {
    if (server.sessions.get(id)?.path === "folder/live") { livePart = true; await partGate.promise; }
    return uploadPart(id, number, blob, signal, progress);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/done"), entry("folder/live")]);
  let queue = await harness.until((state) => livePart
    && state.tasks.find((task) => task.path === "folder/done")?.status === "completed",
    "one file lands while the other still holds its part request");
  const done = queue.tasks.find((task) => task.path === "folder/done");
  const live = queue.tasks.find((task) => task.path === "folder/live");
  assert.equal(live.status, "uploading");
  const requests = server.calls.length;

  const remaining = queue.discard([done.id, live.id]);
  assert.deepEqual(remaining, [live.id], "only the row whose session may still be open is handed back");
  assert.equal(server.calls.length, requests, "discarding the finished row sends nothing");

  queue.cancel(remaining);
  await harness.settle();
  assert.deepEqual(callsOf(server, "abort").map(([, id]) => id), [live.uploadId], "only the live session is aborted");
  assert.deepEqual(callsOf(server, "status"), [], "the abort was definitive, so no status read is needed");
  assert.equal(server.sessions.get(done.uploadId).state, "committed", "the finished session is never touched");

  partGate.resolve();
  queue = await harness.until((state) => state.tasks.length === 0, "the cancelled row leaves the queue");
  assert.equal(callsOf(server, "commit").length, 1, "the aborted file never commits");
});

test("a row completed by its publication Operation is discarded without a request", async (t) => {
  const server = uploadServer(2);
  server.api.commitUpload = async (id) => {
    server.calls.push(["commit", id]);
    server.sessions.get(id).state = "committing";
    return operation("publish-1", "running", null, "upload_commit");
  };
  server.api.getOperation = async (id) => {
    server.calls.push(["operation", id]);
    return operation(id, "succeeded", item("file"), "upload_commit");
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file")]);
  await harness.until((state) => state.tasks[0]?.status === "finalizing" && state.tasks[0]?.operationId === "publish-1");
  await harness.tick();
  let queue = await harness.until((state) => state.tasks[0]?.status === "completed", "the publication settles");
  const requests = server.calls.length;

  assert.deepEqual(queue.discard([queue.tasks[0].id]), [], "a succeeded publication is terminal for its session");
  queue = await harness.settle();
  assert.deepEqual(queue.tasks, []);
  assert.equal(server.calls.length, requests, "no abort, no status read and no cancellation request");
  assert.deepEqual(callsOf(server, "abort"), []);
});

test("discarding completed rows leaves the failure list and the counters correct", async (t) => {
  const server = uploadServer(2);
  const uploadPart = server.api.uploadPart;
  server.api.uploadPart = async (id, number, blob, signal, progress) => {
    if (server.sessions.get(id)?.path === "folder/bad") throw new TestApiError("part refused", 400, { code: "invalid_part" });
    return uploadPart(id, number, blob, signal, progress);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/bad"), entry("folder/good")]);
  let queue = await harness.until((state) =>
    state.tasks.find((task) => task.path === "folder/bad")?.status === "failed"
    && state.tasks.find((task) => task.path === "folder/good")?.status === "completed",
    "one row fails while the other lands");
  const failedBefore = failedTasks(queue.tasks);
  assert.deepEqual(failedBefore.map(({ path }) => path), ["folder/bad"]);

  const remaining = queue.discard(queue.tasks.filter((task) => task.status === "completed").map((task) => task.id));
  assert.deepEqual(remaining, [], "the completed row's session is over");
  queue = await harness.settle();

  assert.deepEqual(queue.tasks.map(({ path, status }) => [path, status]), [["folder/bad", "failed"]],
    "discard removes only finished rows");
  assert.deepEqual(failedTasks(queue.tasks), failedBefore, "the failure keeps its row and its reason");
  const row = buildUploadRows([], queue.tasks, "").find((candidate) => candidate.path === "folder");
  assert.equal(row.upload.files, 1, "the counter describes what is still in the queue");
  assert.equal(row.upload.completedFiles, 0);
  assert.equal(row.upload.failed, 1);
  assert.equal(row.upload.percent, 0);
  assert.equal(queue.hasActiveUploads, false);
});

test("only a completed row whose session is known-terminal may leave without the server", async (t) => {
  const harness = await mount(t, uploadServer().api);
  const { isFinishedUpload } = harness;
  const base = { id: "row", status: "completed", uploadId: "session-1", isDir: false, path: "file" };
  assert.equal(isFinishedUpload({ ...base, sessionTerminal: true }), true);
  assert.equal(isFinishedUpload(base), false, "completed alone is not proof of a terminal session");
  assert.equal(isFinishedUpload({ ...base, status: "uploading", sessionTerminal: true }), false, "only a completed row leaves");
  assert.equal(isFinishedUpload({ ...base, isDir: true, uploadId: undefined }), true, "a completed folder never allocated a session");
  assert.equal(isFinishedUpload({ ...base, isDir: true }), false, "a folder row that names a session is not assumed finished");
});
