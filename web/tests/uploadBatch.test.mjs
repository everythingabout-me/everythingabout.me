import assert from "node:assert/strict";
import test from "node:test";
import { item, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

// The batch transport the browser actually uses: one admission page for the look-ahead and
// one settlement page per wave of finished files. The rest of the queue suite runs without
// it on purpose (a fake without the batch methods pins the single-file fallback), so these
// tests are the only place the page assembly and its per-item answers are exercised.
const entry = (path, bytes = "ab") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json", lastModified: 1443268800000 }) });
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }
function enqueue(harness, entries, dir = "") { harness.render().enqueue(entries, dir); }
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
const page = (server, kind, index = -1) => callsOf(server, kind).at(index)[1];
const sessionPath = (server, id) => server.sessions.get(id).path;

// A settlement page that is not full leaves after its own 500 ms wait, and a settlement the
// server answered transiently retries after 250 ms. A test has to drive both turns itself:
// `settle` deliberately leaves retry timers alone for the tests that assert their order.
async function advance(harness, predicate, label = "expected queue state") {
  for (let attempt = 0; attempt < 100; attempt++) {
    await harness.tick(250);
    const queue = await harness.tick(500);
    if (predicate(queue)) return queue;
  }
  assert.fail(`${label}: ${JSON.stringify(harness.render().tasks.map(({ path, status, error }) => ({ path, status, error })))}`);
}

test("one refused admission fails only its row while the rest of the page completes", async (t) => {
  const server = uploadServer(2, { batch: true });
  const create = server.api.createUpload;
  server.api.createUpload = async (input, key) => {
    if (input.path === "refused") throw new TestApiError("Injected staging limit", 507, { code: "staging_full", requestId: "req_refused" });
    return create(input, key);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("refused"), entry("sibling", "cd")]);
  const queue = await advance(harness, (queue) =>
    queue.tasks.some((task) => task.path === "refused" && task.status === "failed")
    && queue.tasks.some((task) => task.path === "sibling" && task.status === "completed"), "the sibling completes while the refused file fails");
  // The refusal is one item's answer inside a 200 page, not a failed request: both files
  // were admitted by the same call, and each item kept its own idempotency key.
  assert.equal(callsOf(server, "createBatch").length, 1, "both admissions left in one page");
  assert.deepEqual(page(server, "createBatch").map(({ input }) => input.path), ["refused", "sibling"]);
  assert.equal(new Set(page(server, "createBatch").map(({ key }) => key)).size, 2);
  const refused = queue.tasks.find((task) => task.path === "refused");
  assert.equal(refused.error, "Injected staging limit (staging_full · req_refused)");
  assert.equal(refused.uploadId, undefined, "a refused admission has no session to recover");
  assert.equal(refused.allocationRejected, true, "a permanent refusal is not retried by the page");
  assert.equal(queue.tasks.find((task) => task.path === "sibling").status, "completed");
  assert.equal(queue.pauseMessage, null, "one item's Problem is not a queue-level failure");
  assert.equal(harness.completed.length, 1, "only the admitted sibling reached the file list");
  assert.equal(server.sessions.size, 1, "the refused item allocated nothing");
});

test("a failed admission page pauses the queue with one message and resumes", async (t) => {
  const server = uploadServer(2, { batch: true });
  const transport = server.api.createUploadBatch;
  const pages = [];
  server.api.createUploadBatch = async (items) => {
    pages.push(items);
    if (pages.length === 1) throw new TestApiError("connection lost");
    return transport(items);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("a"), entry("b")]);
  let queue = await harness.until((queue) => queue.pauseMessage !== null, "a failed page pauses the queue");
  // The failure is one request-level event, so it is reported once and no row carries it:
  // the rows stay queued, with their intent and idempotency keys, until the cause clears.
  assert.equal(queue.pauseMessage, "connection lost");
  assert.deepEqual(queue.tasks.map(({ status, error }) => [status, error]), [["queued", undefined], ["queued", undefined]],
    "a page-level failure fails no row");
  assert.equal(harness.completed.length, 0);
  // Nothing else leaves while the queue is stopped: the pump does not start the rows, and
  // the stopped page is not retried on a timer the person cannot see.
  await harness.tick(2000);
  assert.equal(pages.length, 1, "the stopped page is not retried on a timer");
  assert.equal(server.sessions.size, 0, "a page that never reached the server allocated nothing");
  assert.equal(callsOf(server, "part").length, 0);
  queue.resumeQueue();
  queue = await advance(harness, (queue) => queue.tasks.every((task) => task.status === "completed"));
  assert.equal(queue.pauseMessage, null, "resuming clears the one message");
  assert.deepEqual(pages.slice(1).flat().map(({ input, key }) => [input.path, key]).sort(),
    pages[0].map(({ input, key }) => [input.path, key]).sort(), "a resumed page replays the keys the failed page carried");
  assert.equal(harness.completed.length, 2);
  assert.equal(server.sessions.size, 2);
});

test("a failed settlement page pauses the queue without failing the files it carried", async (t) => {
  const server = uploadServer(2, { batch: true });
  const transport = server.api.commitUploadBatch;
  let attempts = 0;
  server.api.commitUploadBatch = async (ids) => {
    if (attempts++ === 0) throw new TestApiError("bad gateway", 502, { code: "temporarily_unavailable" });
    return transport(ids);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("a"), entry("b")]);
  let queue = await advance(harness, (queue) => queue.pauseMessage !== null, "the settlement page pauses the queue");
  assert.equal(queue.pauseMessage, "bad gateway");
  assert.equal(queue.tasks.every((task) => task.status === "queued"), true,
    "the settled session stays with its row and nothing is failed");
  assert.equal(harness.completed.length, 0);
  queue.resumeQueue();
  queue = await advance(harness, (queue) => queue.tasks.every((task) => task.status === "completed"));
  assert.equal(harness.completed.length, 2);
  assert.equal(server.sessions.size, 2);
  assert.equal(callsOf(server, "part").length, 2, "a settlement pause resends no bytes");
  assert.ok(attempts >= 2, "the resumed settlement page is sent again");
});

test("a standing replace reaches the admission page as an overwrite condition", async (t) => {
  const server = uploadServer(2, { batch: true });
  const create = server.api.createUpload;
  server.api.metadata = async (path) => { server.calls.push(["metadata", path]); return item(path); };
  server.api.createUpload = async (input, key) => {
    // The first attempt is refused without a revision: the row reads the destination once.
    if (input.path === "a" && input.condition.mode === "create") throw new TestApiError("exists", 409, { code: "name_conflict" });
    return create(input, key);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("a")]);
  const queue = await harness.until((queue) => queue.conflict === "a", "the existing destination is asked about in the page");
  queue.answerConflict("replace-all");
  await advance(harness, (queue) => queue.tasks[0]?.status === "completed", "the answered file completes");
  assert.deepEqual(page(server, "createBatch", 1)[0].input.condition, { mode: "replace", contentRevision: "content-v1" }, "the answered row names the revision it was refused on");
  // Files selected after the standing answer were never refused. Their first create says
  // "replace whatever is here" without a revision: one read per file is what the answer
  // exists to avoid, and the server binds the revision it observes at the write.
  harness.render().enqueue([entry("b"), entry("c")], "");
  await advance(harness, (queue) => queue.tasks.every((task) => task.status === "completed"));
  assert.equal(harness.render().conflictPolicy, "replace");
  assert.deepEqual(page(server, "createBatch").map(({ input }) => ({ path: input.path, condition: input.condition })), [
    { path: "b", condition: { mode: "overwrite" } },
    { path: "c", condition: { mode: "overwrite" } },
  ]);
  assert.equal(callsOf(server, "metadata").filter(([, path]) => path !== "a").length, 0, "a standing answer costs no destination read");
  assert.equal(callsOf(server, "create").filter(([, input]) => input.path !== "a").every(([, input]) => input.condition.mode === "overwrite"), true, "no later file is refused first");
});

test("a settlement page retries only the item the server answered transiently", async (t) => {
  const server = uploadServer(2, { batch: true });
  const commit = server.api.commitUpload;
  const attempts = [];
  let flakyAttempts = 0;
  server.api.commitUpload = async (id) => {
    const path = sessionPath(server, id);
    attempts.push(path);
    if (path === "flaky" && flakyAttempts++ === 0) throw new TestApiError("settlement interrupted", 500, { code: "internal_error" });
    if (path === "refused") throw new TestApiError("The upload is incomplete.", 422, { code: "incomplete_upload" });
    return commit(id);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("flaky"), entry("good"), entry("refused")]);
  const queue = await advance(harness, (queue) =>
    queue.tasks.find((task) => task.path === "flaky")?.status === "completed"
    && queue.tasks.find((task) => task.path === "good")?.status === "completed"
    && queue.tasks.find((task) => task.path === "refused")?.status === "failed", "a transient settlement retries the file the server named");
  const flakyId = [...server.sessions.keys()].find((id) => sessionPath(server, id) === "flaky");
  const pages = callsOf(server, "commitBatch").map(([, ids]) => ids);
  assert.equal(pages[0].length, 3, "the three settlements left in one page");
  assert.deepEqual(pages[0].map((id) => sessionPath(server, id)).sort(), ["flaky", "good", "refused"]);
  assert.deepEqual(pages.at(-1), [flakyId], "the retry is a page carrying only the retried session");
  assert.equal(attempts.filter((path) => path === "flaky").length, 2, "only the transiently refused item is settled again");
  assert.equal(attempts.filter((path) => path === "refused").length, 1, "a 4xx settlement answer is final");
  assert.equal(queue.tasks.find((task) => task.path === "refused").error, "The upload is incomplete. (incomplete_upload · req_fake)");
  assert.equal(harness.completed.length, 2);
});

test("pages stay inside the server's admission and settlement limits", async (t) => {
  const server = uploadServer(2, { batch: true });
  const harness = await mount(t, server.api);
  const files = Array.from({ length: 33 }, (_, index) => entry(`file-${String(index).padStart(2, "0")}`, "a"));
  enqueue(harness, files);
  const queue = await advance(harness, (queue) => queue.tasks.length === 33 && queue.tasks.every((task) => task.status === "completed"), "every file settles");
  assert.equal(queue.tasks.length, files.length);
  // The server refuses a page larger than its limit (32 admissions, 16 settlements), so the
  // page sizes are a transport contract, not an implementation detail. A full admission page
  // is one request either side of the limit; settlement pages fill to sixteen and the
  // remainder leaves on its own wait.
  const commitPages = callsOf(server, "commitBatch").map(([, ids]) => ids);
  // A page is bounded by the server's limit; a partial one leaves on its own timer, so its
  // exact size is not a contract (a page of 31 then 2 also obeys it). What must hold is the
  // bound, full coverage, and the selection's order across pages.
  const createPages = callsOf(server, "createBatch").map(([, items]) => items);
  assert.equal(createPages.every((items) => items.length >= 1 && items.length <= 32), true, "no admission page exceeds the server's limit");
  assert.deepEqual(createPages.flat().map(({ input }) => input.path), files.map(({ path }) => path), "the pages carry the selection in the order the list shows");
  assert.equal(createPages[0].length, 32, "a full admission page leaves at once");
  assert.equal(commitPages[0].length, 16, "a full settlement page leaves at once");
  assert.equal(commitPages.every((ids) => ids.length >= 1 && ids.length <= 16), true, "no settlement page exceeds the server's limit");
  assert.equal(commitPages.flat().length, files.length, "every session is settled on exactly one page");
});

test("an item refused because a folder holds the destination fails without asking", async (t) => {
  // What the browser acceptance script stages with a real folder: a file cannot replace a
  // folder, so the server's per-item 409 is final. The row names the Problem and the sibling
  // still completes; asking a replace question here would leave the acceptance import
  // blocked behind a decision that can never be answered.
  const server = uploadServer(2, { batch: true });
  const create = server.api.createUpload;
  server.api.metadata = async (path) => { server.calls.push(["metadata", path]); return item(path, path === "fail.txt" ? "folder" : "file", null); };
  server.api.createUpload = async (input, key) => {
    if (input.path === "fail.txt") throw new TestApiError("A folder occupies the destination.", 409, { code: "name_conflict", requestId: "req_occupied" });
    return create(input, key);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("fail.txt"), entry("sibling.txt")]);
  const queue = await advance(harness, (queue) =>
    queue.tasks.find((task) => task.path === "fail.txt")?.status === "failed"
    && queue.tasks.find((task) => task.path === "sibling.txt")?.status === "completed", "the folder is not replaced and the sibling completes");
  assert.equal(queue.conflict, null, "a folder destination is not a replace question");
  assert.equal(queue.tasks.find((task) => task.path === "fail.txt").error, "A folder occupies the destination. (name_conflict · req_occupied)");
  assert.equal(harness.completed.length, 1);
});
