import assert from "node:assert/strict";
import test from "node:test";
import { deferred, item, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const file = (path, bytes = "ab") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json" }) });
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
const byPath = (queue, path) => queue.tasks.find((task) => task.path === path);
const failures = (queue) => queue.tasks.filter((task) => task.status === "failed")
  .map(({ id, path, error }) => ({ id, path, error }));
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }

test("a failed mount-time session check is a queue-level pause that Resume retries", async (t) => {
  const server = uploadServer();
  let signedIn = false;
  let checks = 0;
  server.api.me = async () => {
    checks += 1;
    if (!signedIn) throw new TestApiError("signed out", 401);
    return { id: "owner-a" };
  };
  const harness = await mount(t, server.api);
  let queue = await harness.settle();
  assert.equal(queue.pauseMessage, "signed out", "a failed /api/auth/me is the queue's condition");

  const selected = file("private");
  harness.render().enqueue([selected], "");
  queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "queued");
  assert.equal(queue.tasks[0].file, selected.file, "the selected File is kept for the resumed attempt");
  assert.equal(queue.pauseMessage, "signed out");
  assert.deepEqual(server.calls, [], "no admission, part or commit is sent behind an unknown owner");
  assert.equal(checks, 1);

  // The session is still gone: Resume retries the check and keeps the same stop. It must not
  // clear the banner first and let the queue run into another 401 for every queued file.
  queue.resumeQueue();
  queue = await harness.settle();
  assert.equal(checks, 2);
  assert.equal(queue.pauseMessage, "signed out");
  assert.deepEqual(server.calls, []);

  // The session is restored: Resume retries the check, clears the stop, and the queue runs.
  signedIn = true;
  queue.resumeQueue();
  queue = await harness.until((state) => state.tasks[0]?.status === "completed", "a resumed queue publishes the file");
  assert.equal(queue.pauseMessage, null);
  assert.equal(checks, 3);
  assert.equal(callsOf(server, "create").length, 1);
  assert.equal(callsOf(server, "part").length, 1);
  assert.equal(callsOf(server, "commit").length, 1);
  assert.equal(harness.completed.length, 1);
});

test("navigating away and back preserves queued, in-flight and failed rows", async (t) => {
  const server = uploadServer(2);
  const folderGate = deferred();
  const partGate = deferred();
  let partSignal;
  server.api.mkdir = async (path) => {
    server.calls.push(["mkdir", path]);
    if (path === "q") await folderGate.promise;
    return item(path, "folder", null);
  };
  const sendPart = server.api.uploadPart;
  server.api.uploadPart = async (...args) => {
    // The moving file holds its part until the test navigates and returns.
    if (server.sessions.get(args[0])?.path === "moving/file") { partSignal = args[3]; await partGate.promise; }
    return sendPart(...args);
  };
  const create = server.api.createUpload;
  server.api.createUpload = async (input, key) => {
    if (input.path === "failed/file") {
      server.calls.push(["create", structuredClone(input), key]);
      throw new TestApiError("quota exceeded", 507, { code: "quota_exceeded" });
    }
    return create(input, key);
  };

  const harness = await mount(t, server.api);
  // q's task never settles, so q/child is still queued when the page leaves; failed/file is
  // a row failure; moving/file is the one actually moving bytes. q is enqueued on its own
  // first: a folder that holds a selected file is normally created by that file, and this
  // test needs its task to stay open long enough to block the child.
  harness.render().enqueue([{ path: "q", file: null }], "");
  harness.render().enqueue([file("failed/file"), file("moving/file"), file("q/child")], "");
  const before = await harness.until((queue) =>
    byPath(queue, "failed/file")?.status === "failed"
    && byPath(queue, "moving/file")?.status === "uploading"
    && Boolean(partSignal)
    && byPath(queue, "q/child")?.status === "queued", "the three row kinds exist before the navigation");
  assert.equal(byPath(before, "failed/file").error, "quota exceeded");
  const ids = Object.fromEntries(before.tasks.map((task) => [task.path, task.id]));

  // Clicking a link in the page only unmounts it: the module-owned queue keeps the sessions,
  // the pending folder and the failure records.
  harness.navigateAway();
  assert.equal(partSignal.aborted, false, "a navigation is not a cancel");
  assert.deepEqual(callsOf(server, "abort"), [], "a navigation aborts neither the upload nor its session");

  // Coming back re-subscribes; the rows are the same tasks, still in their states.
  let queue = await harness.settle();
  assert.equal(queue.pauseMessage, null);
  assert.equal(queue.tasks.length, before.tasks.length);
  for (const path of Object.keys(ids)) assert.equal(byPath(queue, path).id, ids[path], `${path} keeps its identity`);
  assert.equal(byPath(queue, "failed/file").status, "failed");
  assert.equal(byPath(queue, "failed/file").error, "quota exceeded");
  assert.equal(byPath(queue, "moving/file").status, "uploading");
  assert.equal(byPath(queue, "q/child").status, "queued");

  // The bytes that were moving continue to their publication, and the queued child starts
  // once the folder it waits on exists.
  folderGate.resolve();
  partGate.resolve();
  queue = await harness.until((state) =>
    byPath(state, "moving/file")?.status === "completed" && byPath(state, "q/child")?.status === "completed",
  "the migrated rows finish in place");
  assert.equal(byPath(queue, "failed/file").status, "failed", "the failure is not retried or forgotten by returning");
  assert.deepEqual(harness.completed.map(({ item: landed }) => landed.path).filter((path) => path !== "/q").sort(),
    ["/moving/file", "/q/child"]);
});

test("the failure list's fix link navigates without dropping the failure records", async (t) => {
  // The link's only effect is a navigation to /account#storage. What must survive it is the
  // failure list, which is a projection of the queue rather than page state.
  const server = uploadServer();
  server.api.createUpload = async (input, key) => {
    if (input.path === "broken/file") {
      server.calls.push(["create", structuredClone(input), key]);
      throw new TestApiError("quota exceeded", 507, { code: "quota_exceeded" });
    }
    throw new Error("this test only enqueues the failed file");
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([file("broken/file")], "");
  const before = await harness.until((queue) => byPath(queue, "broken/file")?.status === "failed");
  const records = failures(before);
  assert.equal(records.length, 1);
  const calls = JSON.parse(JSON.stringify(server.calls));

  harness.navigateAway();
  const queue = await harness.settle();
  assert.deepEqual(failures(queue), records, "the failure list is the same list after the fix link");
  assert.deepEqual(JSON.parse(JSON.stringify(server.calls)), calls, "visiting the fix link retries nothing by itself");
});
