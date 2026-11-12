import assert from "node:assert/strict";
import test from "node:test";
import { conflict, deferred, digest, item, memoryStorage, operation, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const entry = (path, bytes = "abc") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json", lastModified: 1443268800000 }) });
const allComplete = (queue) => queue.tasks.length > 0 && queue.tasks.every((task) => task.status === "completed");
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }
function enqueue(harness, entries, dir = "") { harness.render().enqueue(entries, dir, () => false); }
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
function assertGuard(harness, active) {
  assert.equal(harness.render().hasActiveUploads, active);
  assert.equal(harness.listeners.get("beforeunload")?.size ?? 0, active ? 1 : 0, "register exactly one native guard only while active");
  const event = harness.dispatchBeforeUnload();
  assert.equal(event.defaultPrevented, active);
  assert.equal(event.returnValue, active ? "" : undefined, "the guard sets the legacy beforeunload returnValue");
}

for (const lostAck of ["create", "part", "commit"]) {
  test(`lost ${lostAck} ACK recovers the original identity without duplicate bytes or allocation`, async (t) => {
    const server = uploadServer();
    const method = { create: "createUpload", part: "uploadPart", commit: "commitUpload" }[lostAck];
    const original = server.api[method];
    let lose = true;
    server.api[method] = async (...args) => {
      const result = await original(...args);
      if (lose) { lose = false; throw new TestApiError("response lost"); }
      return result;
    };
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")], "Health");
    // A part or a commit retries itself: both are idempotent against the session, and the
    // server's commit answers with the publication or the Operation that is publishing it.
    // Only an allocation the page cannot prove still needs the person's Retry.
    if (lostAck === "commit") { await harness.settle(); await harness.tick(250); }
    let queue = await harness.until(lostAck === "create" ? (queue) => queue.tasks[0]?.status === "failed" : allComplete);
    const id = queue.tasks[0].id;
    if (lostAck === "create") {
      const before = queue.tasks[0];
      assert.ok(before.intent, "intent remains in memory after a lost allocation ACK");
      assert.ok(before.file instanceof File, "the original File remains available for same-page Retry");
      assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null, "upload drafts are never persisted");
      queue.retry(id);
      queue = await harness.until(allComplete);
    }
    assert.equal(queue.tasks[0].id, id);
    assert.equal(queue.tasks[0].file, null);
    assert.equal(server.sessions.size, 1);
    assert.equal(callsOf(server, "part").length, 1, "a durable receipt suppresses resend after lost ACK");
    assert.equal(callsOf(server, "commit").length, lostAck === "commit" ? 2 : 1);
    assert.equal(harness.completed.length, 1);
    const creates = callsOf(server, "create");
    assert.equal(creates.length, lostAck === "create" ? 2 : 1);
    if (lostAck === "create") {
      assert.equal(creates[0][2], creates[1][2]);
      assert.deepEqual(creates[0][1], creates[1][1]);
    } else if (lostAck === "part") assert.ok(callsOf(server, "status").some(([, id]) => id === "session-1"));
    assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null);
  });
}

test("known rejected allocation stays failed across renders and retries only on explicit same-page action", async (t) => {
  const server = uploadServer();
  const attempts = [];
  server.api.createUpload = async (intent, key) => {
    attempts.push([structuredClone(intent), key]);
    throw new TestApiError("quota exceeded", 507, { code: "quota_exceeded" });
  };
  const harness = await mount(t, server.api);
  const selected = entry("file", "ab");
  enqueue(harness, [selected]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  const id = queue.tasks[0].id;
  assert.equal(queue.tasks[0].uploadId, undefined);
  assert.equal(queue.tasks[0].file, selected.file);
  await harness.tick();
  queue = harness.render();
  assert.equal(attempts.length, 1, "renders and polling must not retry an explicit server rejection");
  assert.equal(queue.tasks[0].id, id);
  assert.equal(queue.tasks[0].error, "quota exceeded");
  assertGuard(harness, false);
  queue.retry(id);
  queue = await harness.until((queue) => queue.tasks[0]?.status === "failed" && attempts.length === 2);
  assert.equal(attempts[0][1], attempts[1][1], "explicit Retry replays the same rejected allocation key");
  assert.deepEqual(attempts[0][0], attempts[1][0]);
  queue.cancel(id);
  await harness.until((queue) => queue.tasks.length === 0);
  assert.equal(attempts.length, 2, "Cancel must not replay a known rejected create");
  assert.equal(callsOf(server, "abort").length, 0);
  assert.equal(server.sessions.size, 0);
  assert.equal(callsOf(server, "part").length, 0);
  assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null);
  assertGuard(harness, false);
});

test("same-page reselection verifies receipt hashes and sends only missing numbered parts", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  let loseAck = true;
  server.api.uploadPart = async (...args) => {
    const receipt = await send(...args);
    if (loseAck) throw new TestApiError("ACK unavailable", 400);
    return receipt;
  };
  const harness = await mount(t, server.api);
  const original = entry("folder/file", "abcd");
  enqueue(harness, [original]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  const id = queue.tasks[0].id;
  assert.equal(queue.tasks[0].file, original.file, "failure retains the selected File in this page");
  assert.equal(queue.tasks[0].uploadId, "session-1");
  assert.equal(queue.tasks[0].error, "ACK unavailable");
  assert.equal(callsOf(server, "part").length, 2, "both parts were already in flight");
  assertGuard(harness, false);
  loseAck = false;
  enqueue(harness, [entry("folder/file", "axcd")]);
  queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(queue.tasks[0].error, "drive.reselectedFileChanged");
  assert.equal(callsOf(server, "part").length, 2, "same size with changed bytes cannot resume");
  assert.equal(callsOf(server, "commit").length, 0, "receipt mismatch cannot publish");
  enqueue(harness, [entry("folder/file", "abcd")]);
  queue = await harness.until(allComplete, "the original must replace the rejected File selection on the same page");
  assert.equal(queue.tasks[0].id, id);
  assert.equal(queue.tasks[0].file, null);
  assert.deepEqual(callsOf(server, "part").map(([, id, number]) => [id, number]), [["session-1", 1], ["session-1", 2]]);
  assert.equal(callsOf(server, "commit").length, 1);
  assert.equal(server.sessions.size, 1);
  assertGuard(harness, false);
});

test("pending deletes remain persisted and scoped to the authenticated owner", async (t) => {
  const server = uploadServer();
  const storage = memoryStorage();
  const first = await mount(t, server.api, { storage });
  await first.settle();
  first.persistence.trackFileOperation(operation("private-delete"), "private");
  const saved = storage.getItem("eam.pending-file-operations.v1:owner-a");
  assert.deepEqual(JSON.parse(saved), [{ id: "private-delete", path: "/private" }]);
  first.unmount();
  const other = await mount(t, { ...server.api, me: async () => ({ id: "owner-b" }) }, { storage, owner: "owner-b" });
  assert.deepEqual((await other.settle()).tasks, []);
  assert.deepEqual(other.persistence.pendingFileOperations(), []);
  assert.equal(other.persistence.pendingFileOperation("private/file"), undefined);
  assert.equal(storage.getItem("eam.pending-file-operations.v1:owner-a"), saved);
  other.persistence.trackFileOperation(operation("other-delete"), "other");
  other.unmount();
  const originalOwner = await mount(t, server.api, { storage });
  await originalOwner.settle();
  assert.deepEqual(originalOwner.persistence.pendingFileOperations(), [{ id: "private-delete", path: "/private" }]);
  assert.equal(originalOwner.persistence.pendingFileOperation("private/file").id, "private-delete");
  assert.equal(originalOwner.persistence.pendingFileOperation("other/file"), undefined);
});

for (const status of ["queued", "waiting", "uploading", "finalizing", "failed", "completed", "cancelled"]) {
  test(`fresh mount discards obsolete ${status} records without replaying actions and preserves other owner data`, async (t) => {
    const server = uploadServer();
    const storage = memoryStorage();
    const saved = JSON.stringify([{ id: "obsolete-task", createKey: "old-key", path: "file", name: "file", dir: "", isDir: false,
      size: 2, status, loaded: 2, speed: 0, uploaded: true, uploadId: "old-session", operationId: "old-commit",
      intent: { path: "file", size: 2, condition: { mode: "create" } }, cancelRequested: status === "cancelled",
      waitingFor: { operationId: "old-delete", path: "file", state: "running" } }]);
    const pending = JSON.stringify([{ id: "pending-delete", path: "/folder" }]);
    storage.setItem("eam.uploads.v1:owner-a", saved);
    storage.setItem("eam.uploads.v1:owner-b", saved);
    storage.setItem("eam.pending-file-operations.v1:owner-a", pending);
    storage.setItem("eam.pending-file-operations.v1:owner-b", pending);
    const reads = [], writes = [], actions = [];
    const observedStorage = {
      ...storage,
      getItem: (key) => { reads.push(key); return storage.getItem(key); },
      setItem: (key, value) => { writes.push(key); storage.setItem(key, value); },
    };
    const api = Object.fromEntries(Object.entries(server.api).map(([name, method]) => [name, name === "me" ? method : (...args) => {
      actions.push(name); return method(...args);
    }]));
    const harness = await mount(t, api, { storage: observedStorage });
    assert.deepEqual(harness.render().tasks, []);
    assert.deepEqual((await harness.tick()).tasks, []);
    assertGuard(harness, false);
    assert.equal(storage.getItem("eam.uploads.v1:owner-a"), null);
    assert.equal(storage.getItem("eam.uploads.v1:owner-b"), saved);
    assert.equal(storage.getItem("eam.pending-file-operations.v1:owner-a"), pending);
    assert.equal(storage.getItem("eam.pending-file-operations.v1:owner-b"), pending);
    assert.deepEqual(harness.persistence.pendingFileOperations(), [{ id: "pending-delete", path: "/folder" }]);
    assert.ok(reads.every((key) => !key.startsWith("eam.uploads.v1:")), "obsolete upload records are never read");
    assert.deepEqual(writes, [], "mount does not replace old records with an empty upload draft");
    assert.deepEqual(actions, [], "old uploads, cancellation requests and operations cannot replay any API action");
    assert.deepEqual(server.calls, []);
    assert.deepEqual(harness.completed, []);
  });
}

test("receipt pagination must have one watermark and validate every resumed part before sending bytes", async (t) => {
  for (const scenario of ["changed watermark", "duplicate receipt", "wrong hash", "wrong size"]) {
    // A resumed session: the server reports one part already held, so the queue must list
    // receipts and validate them before sending anything.
    const server = uploadServer(2, { resumeParts: 1 });
    server.api.uploadParts = async (id, cursor) => {
      const receipt = { number: 1, size: 2, sha256: digest("ab") };
      if (scenario === "wrong hash") receipt.sha256 = digest("xx");
      if (scenario === "wrong size") receipt.size = 1;
      if (scenario === "changed watermark" || scenario === "duplicate receipt") {
        return cursor ? { items: [receipt], watermark: scenario === "changed watermark" ? 2 : 1, nextCursor: null }
          : { items: [receipt], watermark: 1, nextCursor: "next" };
      }
      return { items: [receipt], watermark: 1, nextCursor: null };
    };
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "abcd")]);
    await harness.until((queue) => queue.tasks[0]?.status === "failed", scenario);
    assert.equal(callsOf(server, "part").length, 0, scenario);
    assert.equal(callsOf(server, "commit").length, 0, scenario);
  }
});

// A service-wide flag must not be asked per path: during an import the parent is a folder
// the upload itself creates, so a per-parent probe 404s and used to fail every file.
test("capabilities are asked once, at the root, for files whose parents do not exist yet", async (t) => {
  const server = uploadServer();
  const asked = [];
  server.api.capabilities = async (selector) => {
    asked.push(selector);
    if (selector !== undefined) throw new TestApiError("path not found", 404, { code: "not_found" });
    return { sourceModifiedAt: true, transferClass: "C", transferKind: "relay" };
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("Health/watch/a", "ab"), entry("Health/watch/b", "cd"), entry("Health/other/c", "ef")]);
  const queue = await harness.until(allComplete);
  assert.equal(queue.tasks.length, 3);
  assert.deepEqual(asked, [undefined]);
  assert.ok(queue.tasks.every((task) => task.intent?.sourceModifiedAt !== undefined), "the supported flag still reaches every intent");
});

// A failed probe costs the optional modification time, not the file, and is not retried
// once per file — that retry is what turned one refusal into a request storm.
test("a failed capability probe costs the modification time, not the files", async (t) => {
  const server = uploadServer();
  let asked = 0;
  server.api.capabilities = async () => { asked++; throw new TestApiError("temporarily unavailable", 503); };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("a", "ab"), entry("b", "cd")]);
  const queue = await harness.until(allComplete);
  assert.equal(asked, 1);
  assert.ok(queue.tasks.every((task) => task.intent && !("sourceModifiedAt" in task.intent)));
});

// The destination's ancestors are created inside the create request, so a sibling's folder
// creation can refuse it for a few milliseconds. Waiting on that finished Operation turned a
// healthy import into rows that said "waiting for deletion" and cost a poll interval each.
test("a create blocked by a finished folder creation retries instead of waiting", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  let attempts = 0;
  server.api.createUpload = async (...args) => {
    if (++attempts === 1) throw Object.assign(conflict("mkdir-1"), { operationKind: "mkdir" });
    return create(...args);
  };
  server.api.uploadStatus = async (id) => { server.calls.push(["status", id]); throw new TestApiError("not an upload", 404); };
  server.api.getOperation = async (id) => operation(id, "succeeded");
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/file", "ab")]);
  const queue = await harness.until(allComplete);
  assert.equal(attempts, 2, "the refused create was retried once the blocker had ended");
  assert.equal(queue.tasks[0].waitingFor, undefined);
  assert.equal(queue.tasks[0].status, "completed");
});

test("zero-byte upload commits an empty session without a part request", async (t) => {
  const server = uploadServer();
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("empty", "")]);
  const queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].size, 0);
  assert.equal(callsOf(server, "part").length, 0);
  assert.equal(callsOf(server, "commit").length, 1);
});

// The pool is the client's only bound on part requests in flight. It sits above the six
// sockets one HTTP/1.1 origin gives the browser, so a part waiting behind a CORS preflight
// never leaves a socket idle, and it stays the single number every file shares.
test("a global sixteen-part pool has no per-file cap and refills in file order", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  const gates = new Map();
  let active = 0, peak = 0;
  const perFile = new Map(), peaks = new Map();
  server.api.uploadPart = async (...args) => {
    const [id, number] = args;
    active++; peak = Math.max(peak, active);
    perFile.set(id, (perFile.get(id) ?? 0) + 1);
    peaks.set(id, Math.max(peaks.get(id) ?? 0, perFile.get(id)));
    const gate = deferred();
    gates.set(`${id}:${number}`, gate);
    try { await gate.promise; return await send(...args); }
    finally { active--; perFile.set(id, perFile.get(id) - 1); }
  };
  const harness = await mount(t, server.api);
  // 64 bytes in 2-byte parts is more parts than the pool holds, so the first file can fill
  // it and still have parts left when one finishes.
  enqueue(harness, [...Array.from({ length: 4 }, (_, i) => entry(`large-${i}`, "x".repeat(64))), entry("small", "x")]);
  await harness.until(() => active === 16);
  await harness.tick();
  assert.equal(peak, 16, "the pool caps part requests across files");
  assert.deepEqual([...peaks.entries()], [["session-1", 16]], "one file may use the whole pool");
  // Finishing one part immediately starts exactly one more, without waiting for its batch.
  const first = [...gates.keys()][0];
  gates.get(first).resolve();
  await harness.until(() => gates.size === 17);
  assert.equal(active, 16);
  const started = [...gates.keys()].at(-1);
  assert.equal(started, "session-1:17", "the earliest-started file keeps the pool until it runs out of parts");
  const release = () => { for (const gate of gates.values()) gate.resolve(); };
  const timer = setInterval(release, 1);
  t.after(() => clearInterval(timer));
  await harness.until(allComplete);
  assert.equal(peak, 16);
});

test("shared delete Operation polls once, retains Files, frees slots and resumes only exact succeeded identity", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  let state = "running";
  server.api.createUpload = async (...args) => {
    if (args[0].path.startsWith("folder/") && state !== "succeeded") throw conflict();
    return create(...args);
  };
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, state); };
  const harness = await mount(t, server.api);
  const files = [entry("folder/a"), entry("folder/b"), entry("unrelated")];
  enqueue(harness, files);
  let queue = await harness.until((queue) => queue.tasks[2]?.status === "completed");
  assert.deepEqual(queue.tasks.map((task) => task.status), ["waiting", "waiting", "completed"]);
  assert.equal(queue.tasks[0].file, files[0].file);
  await harness.tick();
  assert.deepEqual(callsOf(server, "operation"), [["operation", "delete-1"]]);
  state = "succeeded";
  await harness.tick();
  queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].speed, 0);
  assert.equal(callsOf(server, "part").filter(([, id]) => id !== "session-1").length, 4);
});

// The row may only claim a deletion when the blocker is one. A move that is still running,
// or any blocker whose kind the server did not send, is named as another file operation.
test("a live non-deletion blocker is never described as a deletion", async (t) => {
  const server = uploadServer();
  server.api.createUpload = async () => { throw Object.assign(conflict("move-1"), { operationKind: "move" }); };
  server.api.uploadStatus = async (id) => { server.calls.push(["status", id]); throw new TestApiError("not an upload", 404); };
  server.api.getOperation = async (id) => operation(id, "running", null, "move");
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/file", "ab")]);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  assert.equal(queue.tasks[0].waitingFor.kind, "operation");
  assert.equal(queue.tasks[0].waitMessage, "drive.waitingForOperation");
});

test("a deletion that did not finish, an unknown outcome, an unreadable status and a wrong identity never authorize bytes by themselves", async (t) => {
  const server = uploadServer();
  server.api.createUpload = async () => { server.calls.push(["create"]); throw conflict(); };
  let response = operation("delete-1", "running");
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); if (response instanceof Error) throw response; return response; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/file")]);
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  // Every one of these answers leaves the task parked: the bytes wait for the person, not
  // for a later moment. `failed` and `cancelled` additionally offer Continue, because a
  // terminal Operation has released its guard and no automatic pass can ever end that
  // wait; the other answers are still reading the truth and keep retrying.
  const resumable = (task) => task.waitingFor?.state === "failed" || task.waitingFor?.state === "cancelled";
  for (const [value, expected] of [
    [operation("delete-1", "failed"), true],
    [operation("delete-1", "cancelled"), true],
    [operation("delete-1", "outcome_unknown"), false],
    [new Error("offline"), false],
    [operation("other", "succeeded"), false],
    [{}, false],
  ]) {
    response = value;
    const queue = await harness.tick();
    assert.equal(queue.tasks[0].status, "waiting");
    assert.equal(queue.tasks[0].speed, 0);
    assert.equal(resumable(queue.tasks[0]), expected);
    assertGuard(harness, true);
    assert.equal(callsOf(server, "create").length, 1);
    assert.equal(callsOf(server, "part").length, 0);
  }
});

test("pending ancestor delete blocks a folder tree while prefix siblings continue", async (t) => {
  const server = uploadServer();
  const mkdir = server.api.mkdir;
  const gate = deferred();
  let done = false;
  server.api.mkdir = async (path) => { if (path === "root") await gate.promise; return mkdir(path); };
  server.api.getOperation = async (id) => operation(id, done ? "succeeded" : "running");
  const harness = await mount(t, server.api);
  await harness.settle();
  harness.persistence.trackFileOperation(operation("delete-root"), "root");
  // Folders that would contain files are created by the upload itself now, so a
  // blocking test uses a folder tree with no file under it.
  enqueue(harness, [{ path: "root", file: null }, { path: "root/nested", file: null }, entry("rootish/file")]);
  let queue = await harness.until((queue) => queue.tasks[2]?.status === "completed");
  assert.equal(queue.tasks[0].status, "waiting");
  assert.equal(queue.tasks[1].status, "queued");
  assert.equal(callsOf(server, "mkdir").length, 0);
  assert.equal(callsOf(server, "create").length, 1);
  done = true;
  await harness.tick();
  await harness.settle();
  assert.equal(callsOf(server, "create").length, 1, "the sibling upload never waits for this tree");
  gate.resolve();
  queue = await harness.until(allComplete);
  assert.deepEqual(callsOf(server, "mkdir").map(([, path]) => path), ["root", "root/nested"]);
});

test("existing folder merges and a conflicting file uses its content revision for confirmed overwrite", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  server.api.mkdir = async () => { throw new TestApiError("exists", 409, { code: "name_conflict" }); };
  server.api.metadata = async (path) => item(path, path === "folder" ? "folder" : "file");
  server.api.createUpload = async (...args) => { if (args[0].condition.mode === "create") throw new TestApiError("exists", 409, { code: "name_conflict" }); return create(...args); };
  let prompts = 0;
  const harness = await mount(t, server.api, { confirm: () => { prompts++; return true; } });
  enqueue(harness, [{ path: "folder", file: null }, entry("folder/file")]);
  await harness.until(allComplete, "existing folders must merge before children upload");
  assert.equal(prompts, 1);
  assert.deepEqual(callsOf(server, "create")[0][1].condition, { mode: "replace", contentRevision: "content-v1" });
});

test("async mkdir completion is polled by its original Operation and never reallocates mkdir", async (t) => {
  const server = uploadServer();
  server.api.mkdir = async (path) => { server.calls.push(["mkdir", path]); return operation("mkdir-1", "running", null, "mkdir"); };
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, "succeeded", item("folder", "folder"), "mkdir"); };
  const harness = await mount(t, server.api);
  // An empty folder is the case that still needs an explicit create.
  enqueue(harness, [{ path: "folder", file: null }]);
  await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
  // An explicit retry also must resolve the durable operation, not issue mkdir again.
  harness.render().retry(harness.render().tasks[0].id);
  await harness.settle();
  await harness.tick();
  await harness.until(allComplete);
  assert.equal(callsOf(server, "mkdir").length, 1);
});

test("cancel before commit aborts the session and a late part ACK cannot publish", async (t) => {
  const server = uploadServer();
  const gate = deferred(), cancellation = deferred();
  const abort = server.api.abortUpload;
  let signal;
  server.api.uploadPart = async (id, number, blob, receivedSignal) => { server.calls.push(["part", id, number]); signal = receivedSignal; await gate.promise; return { number, size: blob.size, sha256: digest("ab") }; };
  server.api.abortUpload = async (...args) => { await cancellation.promise; return abort(...args); };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  await harness.until(() => Boolean(signal));
  assertGuard(harness, true);
  harness.render().cancel(harness.render().tasks[0].id);
  const cancelling = await harness.settle();
  assert.equal(signal.aborted, true);
  assert.equal(cancelling.tasks[0].status, "finalizing");
  assert.equal(cancelling.tasks[0].cancelRequested, true);
  assertGuard(harness, true);
  cancellation.resolve();
  await harness.until((queue) => queue.tasks.length === 0);
  assert.equal(callsOf(server, "abort").length, 1);
  assertGuard(harness, false);
  gate.resolve();
  await harness.settle();
  assert.equal(callsOf(server, "commit").length, 0);
  assert.equal(harness.completed.length, 0);
  assert.deepEqual(harness.render().tasks, []);
});

test("cancel after publication follows commit Operation and reports late success without deleting committed bytes", async (t) => {
  const server = uploadServer();
  server.api.commitUpload = async (id) => { server.calls.push(["commit", id]); server.sessions.get(id).state = "committing"; return operation("publish-1", "running", null, "upload_commit"); };
  server.api.abortUpload = async (id) => { server.calls.push(["abort", id]); throw new TestApiError("committing", 409, { code: "commit_in_progress" }); };
  server.api.uploadStatus = async (id) => ({ ...server.sessions.get(id), operationId: "publish-1" });
  server.api.cancelOperation = async (id) => { server.calls.push(["cancelOperation", id]); return operation(id, "outcome_unknown", null, "upload_commit"); };
  server.api.getOperation = async (id) => operation(id, "succeeded", item("file"), "upload_commit");
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  let queue = await harness.until((queue) => queue.tasks[0]?.operationId === "publish-1");
  queue.cancel(queue.tasks[0].id);
  queue = await harness.settle();
  assert.equal(queue.tasks[0].cancelRequested, true);
  assert.equal(queue.tasks[0].status, "finalizing");
  assertGuard(harness, true);
  assert.deepEqual(callsOf(server, "cancelOperation"), [["cancelOperation", "publish-1"]]);
  await harness.tick();
  assert.deepEqual(harness.render().tasks, []);
  assertGuard(harness, false);
  assert.equal(harness.completed.length, 1);
  assert.equal(harness.completed[0].item.path, "/file");
});

test("cancel lost allocation ACK recovers and aborts the same idempotent session", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  let lose = true;
  server.api.createUpload = async (...args) => { const result = await create(...args); if (lose) { lose = false; throw new TestApiError("lost"); } return result; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file")]);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  queue.cancel(queue.tasks[0].id);
  await harness.settle();
  assert.equal(server.sessions.size, 1);
  assert.equal(callsOf(server, "create")[0][2], callsOf(server, "create")[1][2]);
  assert.deepEqual(callsOf(server, "abort"), [["abort", "session-1"]]);
  assert.equal(callsOf(server, "part").length, 0);
});

test("cancel answers with what the gesture covered, and an empty gesture answers nothing", async (t) => {
  const server = uploadServer();
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("tree/one"), entry("tree/two")]);
  const queue = harness.render();
  // The rows leave the sheet at once, so the gesture — not a row — is what can still say how
  // many items it covered. Without it a cancelled import at 3% of 20,000 looked exactly like
  // one that had never held more than the handful of rows not yet torn down.
  const summary = queue.cancel(queue.tasks.map((task) => task.id));
  assert.deepEqual(summary, { items: 2, completedItems: 0 });
  assert.equal(queue.cancel(["unknown"]), null);
  await harness.until((queue) => queue.tasks.length === 0);
  for (const session of server.sessions.values()) assert.equal(session.state, "aborted");
});

test("cancel waiting folder removes descendants and a late poll cannot revive replacement tasks", async (t) => {
  const server = uploadServer();
  const gate = deferred();
  server.api.mkdir = async () => { throw conflict(); };
  server.api.getOperation = async () => gate.promise;
  const harness = await mount(t, server.api);
  const entries = [{ path: "folder", file: null }, { path: "folder/child", file: null }];
  enqueue(harness, entries);
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  await harness.tick();
  const oldId = harness.render().tasks[0].id;
  harness.render().cancel(oldId);
  await harness.settle();
  assert.deepEqual(harness.render().tasks, []);
  enqueue(harness, entries);
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  gate.resolve(operation("delete-1", "succeeded"));
  await harness.settle();
  assert.notEqual(harness.render().tasks[0].id, oldId);
  assert.equal(harness.render().tasks[0].status, "waiting");
  assert.equal(callsOf(server, "part").length, 0);
});

test("unmount aborts in-flight polling and ignores its late succeeded response", async (t) => {
  const server = uploadServer();
  const gate = deferred();
  let signal;
  server.api.createUpload = async () => { server.calls.push(["create"]); throw conflict(); };
  server.api.getOperation = async (id, receivedSignal) => { signal = receivedSignal; return gate.promise; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file")]);
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  await harness.tick();
  harness.unmount();
  assert.equal(signal.aborted, true);
  assert.equal(harness.timers.size, 0);
  gate.resolve(operation("delete-1", "succeeded"));
  await harness.settle();
  assert.equal(callsOf(server, "create").length, 1);
  assert.equal(harness.completed.length, 0);
});

test("unmount suppresses a late commit ACK and its completion callback", async (t) => {
  const server = uploadServer();
  const gate = deferred();
  let signal;
  server.api.commitUpload = async (id, receivedSignal) => { signal = receivedSignal; return gate.promise; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  await harness.until(() => Boolean(signal));
  harness.unmount();
  gate.resolve(item("file"));
  await harness.settle();
  assert.equal(signal.aborted, true);
  assert.equal(harness.completed.length, 0);
});

test("a lost commit ACK is retried and follows the session's Operation without resending bytes", async (t) => {
  const server = uploadServer();
  let attempts = 0;
  server.api.commitUpload = async (id) => {
    server.calls.push(["commit", id]);
    // The first answer is lost after the server froze the session; the retry finds it already
    // publishing and gets the durable Operation instead of a second publication.
    if (++attempts === 1) { server.sessions.get(id).state = "committing"; throw new TestApiError("commit ACK lost"); }
    return operation(id, "running", null, "upload_commit");
  };
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, "succeeded", item("file"), "upload_commit"); };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
  // The lost answer is retried after the documented 250 ms backoff.
  await harness.tick(250);
  queue = await harness.until((queue) => Boolean(queue.tasks[0]?.operationId));
  assert.ok(queue.tasks[0].file instanceof File);
  assert.equal(queue.tasks[0].operationId, "session-1");
  assertGuard(harness, true);
  await harness.tick();
  queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].file, null);
  assert.equal(callsOf(server, "part").length, 1);
  assert.equal(callsOf(server, "commit").length, 2);
  assert.deepEqual(callsOf(server, "operation"), [["operation", "session-1"]]);
  assertGuard(harness, false);
});

test("manual retry never overtakes a recorded deletion; Continue is the gesture that clears it", async (t) => {
  const server = uploadServer();
  server.api.getOperation = async (id) => operation(id, "failed");
  const harness = await mount(t, server.api);
  await harness.settle();
  harness.persistence.trackFileOperation(operation("delete-recorded"), "file");
  enqueue(harness, [entry("file")]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  await harness.tick();
  queue = harness.render();
  assert.equal(queue.tasks[0].waitMessage, "drive.deletionDidNotFinish");
  queue.retry(queue.tasks[0].id);
  queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "waiting", "retry cannot bypass the recorded deletion");
  assert.equal(callsOf(server, "create").length, 0, "retry allocates nothing while the record stands");
  assert.equal(callsOf(server, "part").length, 0, "no byte moves before the person decides");
  assert.equal(callsOf(server, "commit").length, 0);
});

test("consistent receipt pages skip every saved part, including noncontiguous numbers", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  server.api.createUpload = async (...args) => {
    const session = await create(...args);
    server.receipts.get(session.id).set(1, { number: 1, size: 2, sha256: digest("ab") });
    server.receipts.get(session.id).set(3, { number: 3, size: 1, sha256: digest("e") });
    return { ...session, receivedParts: 2, receivedBytes: 3 };
  };
  server.api.uploadParts = async (id, cursor) => {
    server.calls.push(["receipts", id, cursor]);
    const receipts = server.receipts.get(id);
    return cursor ? { items: [...receipts.values()].filter((receipt) => receipt.number !== 1), watermark: 2, nextCursor: null }
      : { items: [receipts.get(1)], watermark: 2, nextCursor: "page-2" };
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "abcde")]);
  await harness.until(allComplete);
  assert.deepEqual(callsOf(server, "receipts").map(([, , cursor]) => cursor), [undefined, "page-2"]);
  assert.deepEqual(callsOf(server, "part").map(([, , number]) => number), [2]);
  assert.equal(callsOf(server, "commit").length, 1);
});

test("authentication resolution gates all queued bytes and a different-size reselect stays local", async (t) => {
  const server = uploadServer();
  const user = deferred();
  const create = server.api.createUpload;
  server.api.me = async () => user.promise;
  server.api.createUpload = async (...args) => { const result = await create(...args); throw new TestApiError(`lost ${result.id}`); };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  await harness.settle();
  assert.equal(callsOf(server, "create").length, 0);
  assert.equal(harness.render().tasks[0].status, "queued");
  assertGuard(harness, true);
  user.resolve({ id: "owner-a" });
  const failed = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  const originalFile = failed.tasks[0].file;
  const creates = callsOf(server, "create").length;
  enqueue(harness, [entry("file", "longer")]);
  const queue = await harness.settle();
  assert.equal(queue.tasks[0].error, "drive.reselectedFileChanged");
  assert.equal(queue.tasks[0].file, originalFile, "wrong-size selection leaves the original File available");
  assert.equal(callsOf(server, "create").length, creates);
  assert.equal(callsOf(server, "part").length, 0);
  assertGuard(harness, false);
});

test("failed authentication leaves queued Files local and cannot restore another owner", async (t) => {
  const server = uploadServer();
  server.api.me = async () => { throw new TestApiError("signed out", 401); };
  const harness = await mount(t, server.api);
  const selected = entry("private");
  enqueue(harness, [selected]);
  const queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "queued");
  assert.equal(queue.tasks[0].file, selected.file);
  assert.equal(server.calls.length, 0);
  assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null);
});

for (const phase of ["open", "committing"]) {
  test(`same-page failed cancellation of a ${phase} session retries only on explicit action without resending bytes`, async (t) => {
    const server = uploadServer();
    const abort = server.api.abortUpload;
    let attempts = 0;
    if (phase === "open") {
      server.api.uploadPart = async (id, number) => { server.calls.push(["part", id, number]); throw new TestApiError("rejected", 400); };
      server.api.abortUpload = async (id) => {
        if (++attempts === 1) { server.calls.push(["abort", id]); throw new TestApiError("offline"); }
        return abort(id);
      };
    } else {
      server.api.commitUpload = async (id) => {
        server.calls.push(["commit", id]); server.sessions.get(id).state = "committing";
        return operation("commit-session-1", "running", null, "upload_commit");
      };
      server.api.cancelOperation = async (id) => {
        server.calls.push(["cancelOperation", id]);
        if (++attempts === 1) throw new TestApiError("offline");
        return operation(id, "cancelled", null, "upload_commit");
      };
    }
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")]);
    let queue = await harness.until((queue) => phase === "open" ? queue.tasks[0]?.status === "failed" : Boolean(queue.tasks[0]?.operationId));
    const id = queue.tasks[0].id;
    const parts = callsOf(server, "part").length;
    const commits = callsOf(server, "commit").length;
    queue.cancel(id);
    queue = await harness.until((queue) => queue.tasks[0]?.status === "failed" && queue.tasks[0].cancelRequested);
    assert.equal(queue.tasks[0].error, "offline");
    assertGuard(harness, false);
    await harness.tick();
    assert.equal(attempts, 1, "render and polling do not silently retry a failed cancellation");
    harness.render().retry(id);
    await harness.until((queue) => queue.tasks.length === 0);
    assert.equal(attempts, 2);
    assert.deepEqual(callsOf(server, "abort"), [["abort", "session-1"], ["abort", "session-1"]]);
    assert.equal(callsOf(server, "create").length, 1);
    assert.equal(callsOf(server, "part").length, parts);
    assert.equal(callsOf(server, "commit").length, commits);
    assert.equal(server.sessions.size, 1);
    assert.equal(harness.completed.length, 0);
    if (phase === "committing") assert.deepEqual(callsOf(server, "cancelOperation"), [["cancelOperation", "commit-session-1"], ["cancelOperation", "commit-session-1"]]);
    assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null);
    assertGuard(harness, false);
  });
}

for (const terminal of ["expired", "aborted", "failed"]) {
  test(`explicit retry replaces an authoritatively ${terminal} session with a fresh key`, async (t) => {
    const server = uploadServer();
    const send = server.api.uploadPart;
    let first = true;
    server.api.uploadPart = async (...args) => {
      if (first) { first = false; server.sessions.get(args[0]).state = terminal; throw new TestApiError("response lost"); }
      return send(...args);
    };
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")]);
    const failed = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    assert.equal(server.sessions.size, 1);
    failed.retry(failed.tasks[0].id);
    await harness.until(allComplete, `explicit retry must recover a ${terminal} upload`);
    const creates = callsOf(server, "create");
    assert.equal(creates.length, 2);
    assert.notEqual(creates[0][2], creates[1][2], "never replay terminal session allocation");
    assert.ok(callsOf(server, "status").some(([, id]) => id === "session-1"), "terminal proof comes from the original server session");
    assert.deepEqual(callsOf(server, "part").map(([, id]) => id), ["session-2"]);
    assert.deepEqual(callsOf(server, "commit"), [["commit", "session-2"]]);
  });

  test(`cancel retires an authoritatively ${terminal} session after its terminal HTTP error`, async (t) => {
    const server = uploadServer();
    server.api.uploadPart = async (id) => { server.sessions.get(id).state = terminal; throw new TestApiError("lost response"); };
    server.api.abortUpload = async (id) => { server.calls.push(["abort", id]); throw new TestApiError("no longer open", 410, { code: terminal === "expired" ? "upload_expired" : "upload_gone" }); };
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")]);
    const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    queue.cancel(queue.tasks[0].id);
    await harness.until((queue) => queue.tasks.length === 0, `cancel should retire ${terminal} upload after authoritative status`);
    assert.ok(callsOf(server, "status").some(([, id]) => id === "session-1"));
    assert.equal(server.sessions.size, 1);
    assert.equal(callsOf(server, "commit").length, 0);
    assert.equal(harness.completed.length, 0);
  });
}

for (const boundary of ["upload abort", "Operation cancel"]) {
  test(`already_committed from ${boundary} reconciles the original published result`, async (t) => {
    const server = uploadServer();
    let operationCommitted = false;
    server.api.commitUpload = async (id) => {
      // The commit is accepted and publishing; the client holds the Operation, not the file.
      server.sessions.get(id).state = "committing";
      return operation("commit-session-1", "running", null, "upload_commit");
    };
    server.api.abortUpload = async (id) => {
      server.calls.push(["abort", id]);
      const session = server.sessions.get(id);
      if (boundary === "upload abort") {
        // The publication won the race: by the time the abort arrives the file is published.
        session.state = "committed"; session.result = item("file", "file", session.size);
        throw new TestApiError("already committed", 409, { code: "already_committed" });
      }
      throw new TestApiError("publication active", 409, { code: "commit_in_progress" });
    };
    server.api.cancelOperation = async (id) => { server.calls.push(["cancelOperation", id]); operationCommitted = true; throw new TestApiError("publication completed", 409, { code: "already_committed" }); };
    server.api.getOperation = async (id) => {
      server.calls.push(["operation", id]);
      return operation(id, operationCommitted || boundary === "upload abort" ? "succeeded" : "running", item("file"), "upload_commit");
    };
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")]);
    const queue = await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
    queue.cancel(queue.tasks[0].id);
    await harness.until((queue) => queue.tasks.length === 0, "already committed cancellation must reconcile success");
    assert.equal(harness.completed.length, 1);
    assert.equal(harness.completed[0].item.path, "/file");
    assert.equal(server.sessions.size, 1);
    assert.equal(callsOf(server, "part").length, 1);
    if (boundary === "upload abort") assert.ok(callsOf(server, "status").some(([, id]) => id === "session-1"));
    else assert.deepEqual(callsOf(server, "operation"), [["operation", "commit-session-1"]]);
  });
}

test("an unavailable original session never authorizes a fresh allocation", async (t) => {
  const server = uploadServer();
  server.api.uploadPart = async () => { throw new TestApiError("part ACK lost"); };
  let statusError;
  server.api.uploadStatus = async () => { throw statusError; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  await harness.until((queue) => queue.tasks[0]?.status === "failed");
  for (const error of [new TestApiError("missing", 404), new TestApiError("unavailable", 503), new Error("offline")]) {
    statusError = error;
    harness.render().retry(harness.render().tasks[0].id);
    await harness.until((queue) => queue.tasks[0]?.status === "failed");
    assert.equal(server.sessions.size, 1);
    assert.equal(callsOf(server, "create").length, 1);
    assert.equal(callsOf(server, "commit").length, 0);
  }
});

test("name conflict after a lost pre-allocation request still offers revision-bound overwrite", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  let first = true, prompts = 0;
  const attempts = [];
  server.api.createUpload = async (...args) => {
    attempts.push(structuredClone(args.slice(0, 2)));
    if (first) { first = false; throw new TestApiError("request never reached server"); }
    if (args[0].condition.mode === "create") throw new TestApiError("exists", 409, { code: "name_conflict" });
    return create(...args);
  };
  const harness = await mount(t, server.api, { confirm: () => { prompts++; return true; } });
  enqueue(harness, [entry("file", "ab")]);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  queue.retry(queue.tasks[0].id);
  await harness.until(allComplete);
  assert.equal(prompts, 1);
  assert.equal(attempts[0][1], attempts[1][1], "first recover the uncertain allocation key");
  assert.notEqual(attempts[1][1], attempts[2][1], "accepted overwrite binds a new intent and key");
  assert.deepEqual(attempts[2][0].condition, { mode: "replace", contentRevision: "content-v1" });
});

for (const supported of [false, true]) {
  test(`source modification time is ${supported ? "included for an admitted backend" : "omitted when unsupported"}`, async (t) => {
    const server = uploadServer();
    server.api.capabilities = async () => ({ sourceModifiedAt: supported });
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")]);
    await harness.until(allComplete);
    const intent = callsOf(server, "create")[0][1];
    if (supported) assert.equal(intent.sourceModifiedAt, "2015-09-26T12:00:00.000Z");
    else assert.equal(Object.hasOwn(intent, "sourceModifiedAt"), false);
  });
}

test("same-page renders and selections in different directories retain independent queued Files", async (t) => {
  const server = uploadServer();
  const user = deferred();
  server.api.me = async () => user.promise;
  const harness = await mount(t, server.api);
  const first = entry("file", "ab"), second = entry("file", "cd");
  enqueue(harness, [first], "Health");
  const original = harness.render().tasks[0];
  enqueue(harness, [second], "Notes");
  let queue = await harness.settle();
  assert.deepEqual(queue.tasks.map(({ path, status }) => [path, status]), [["Health/file", "queued"], ["Notes/file", "queued"]]);
  assert.equal(queue.tasks[0].id, original.id);
  assert.equal(queue.tasks[0].file, first.file);
  assert.equal(queue.tasks[1].file, second.file);
  assert.notEqual(queue.tasks[0].id, queue.tasks[1].id);
  assertGuard(harness, true);
  assert.deepEqual(server.calls, []);
  assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null);
  user.resolve({ id: "owner-a" });
  queue = await harness.until(allComplete);
  assert.deepEqual(callsOf(server, "create").map(([, intent]) => [intent.path, intent.mediaType]), [["Health/file", "application/json"], ["Notes/file", "application/json"]]);
  assert.deepEqual(queue.tasks.map((task) => task.file), [null, null]);
  assertGuard(harness, false);
});

test("explicit retry uses the original terminal commit Operation to allocate a fresh session safely", async (t) => {
  const server = uploadServer();
  const commit = server.api.commitUpload;
  server.api.commitUpload = async (id) => {
    if (id !== "session-1") return commit(id);
    server.calls.push(["commit", id]);
    server.sessions.get(id).state = "failed";
    return operation("commit-session-1", "running", null, "upload_commit");
  };
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, "failed", null, "upload_commit"); };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  await harness.until((queue) => queue.tasks[0]?.operationId === "commit-session-1");
  await harness.tick();
  assert.equal(harness.render().tasks[0].status, "failed");
  harness.render().retry(harness.render().tasks[0].id);
  await harness.until(allComplete, "a failed asynchronous commit must permit explicit safe restart");
  assert.equal(server.sessions.size, 2);
  assert.ok(callsOf(server, "operation").some(([, id]) => id === "commit-session-1"));
  assert.deepEqual(callsOf(server, "part").map(([, id]) => id), ["session-1", "session-2"]);
  assert.notEqual(callsOf(server, "create")[0][2], callsOf(server, "create")[1][2]);
});

test("native unload guard follows queued, waiting, uploading and finalizing lifecycle until completion", async (t) => {
  const server = uploadServer();
  const user = deferred(), parts = deferred(), publication = deferred();
  const create = server.api.createUpload, send = server.api.uploadPart, commit = server.api.commitUpload;
  let deletionDone = false, signal;
  server.api.me = async () => user.promise;
  server.api.createUpload = async (...args) => { if (!deletionDone) throw conflict(); return create(...args); };
  server.api.getOperation = async (id) => operation(id, deletionDone ? "succeeded" : "running");
  server.api.uploadPart = async (...args) => { signal = args[3]; await parts.promise; return send(...args); };
  server.api.commitUpload = async (...args) => { await publication.promise; return commit(...args); };
  const harness = await mount(t, server.api);
  assertGuard(harness, false);
  const selected = entry("file", "ab");
  enqueue(harness, [selected]);
  assert.equal(harness.render().tasks[0].status, "queued");
  assertGuard(harness, true);
  const listener = [...harness.listeners.get("beforeunload")][0];
  user.resolve({ id: "owner-a" });
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  const id = queue.tasks[0].id;
  assertGuard(harness, true);
  assert.equal(queue.tasks[0].file, selected.file);
  deletionDone = true;
  await harness.tick();
  queue = await harness.until((queue) => queue.tasks[0]?.status === "uploading" && Boolean(signal));
  assertGuard(harness, true);
  assert.equal(signal.aborted, false);
  parts.resolve();
  queue = await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
  assert.equal(queue.tasks[0].id, id);
  assertGuard(harness, true);
  assert.equal([...harness.listeners.get("beforeunload")][0], listener, "active transitions keep one existing handler");
  publication.resolve();
  queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].file, null);
  assertGuard(harness, false);
  assert.equal(harness.completed.length, 1);
  assert.equal(harness.storage.getItem("eam.uploads.v1:owner-a"), null);
});

test("reload aborts the old data request and starts empty without status, allocation or cancellation replay", async (t) => {
  const server = uploadServer();
  const storage = memoryStorage(), gate = deferred();
  const send = server.api.uploadPart;
  let signal;
  server.api.uploadPart = async (...args) => { signal = args[3]; await gate.promise; return send(...args); };
  const first = await mount(t, server.api, { storage });
  enqueue(first, [entry("file", "ab")]);
  const uploading = await first.until((queue) => queue.tasks[0]?.status === "uploading" && Boolean(signal));
  assertGuard(first, true);
  const { file, ...legacy } = uploading.tasks[0];
  storage.setItem("eam.uploads.v1:owner-a", JSON.stringify([{ ...legacy, cancelRequested: true }]));
  const callsBeforeReload = [...server.calls];
  first.unmount();
  assert.equal(signal.aborted, true, "page teardown aborts the request carrying file bytes");
  assert.equal(first.listeners.size, 0, "page teardown removes the unload guard");
  assert.equal(first.timers.size, 0);
  const reloaded = await mount(t, server.api, { storage });
  assert.deepEqual(reloaded.render().tasks, []);
  assert.deepEqual((await reloaded.tick()).tasks, []);
  assert.equal(storage.getItem("eam.uploads.v1:owner-a"), null);
  assertGuard(reloaded, false);
  assert.deepEqual(server.calls, callsBeforeReload, "reload never replays old status, create, abort or cancel requests");
  gate.resolve();
  await first.settle();
  assert.deepEqual((await reloaded.tick()).tasks, []);
  assert.equal(callsOf(server, "create").length, 1);
  assert.equal(callsOf(server, "status").length, 0);
  assert.equal(callsOf(server, "abort").length, 0);
  assert.equal(callsOf(server, "cancelOperation").length, 0);
  assert.equal(callsOf(server, "commit").length, 0, "late acknowledgement from the old request cannot publish");
  assert.equal(first.completed.length, 0);
  assert.equal(reloaded.completed.length, 0);
});

test("hung delete checks time out and allow other exact Operations to progress", async (t) => {
  const server = uploadServer();
  let stuckSignal;
  server.api.createUpload = async (input) => { throw conflict(input.path === "stuck" ? "delete-stuck" : "delete-other"); };
  // The second delete can finish independently; no upload body is needed to prove polling fairness.
  server.api.getOperation = (id, signal) => {
    server.calls.push(["operation", id]);
    if (id === "delete-other") return Promise.resolve(operation(id, "failed"));
    stuckSignal = signal;
    return new Promise((resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("timeout", "AbortError")), { once: true }));
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("stuck"), entry("other")]);
  await harness.until((queue) => queue.tasks.every((task) => task.status === "waiting"));
  await harness.tick();
  await harness.tick(60000);
  assert.equal(stuckSignal.aborted, true);
  assert.ok(callsOf(server, "operation").some(([, id]) => id === "delete-other"));
  // Rows are looked up by path, not position: the queue publishes in row order, which
  // this selection does not keep.
  const waiting = (path) => harness.render().tasks.find((task) => task.path === path).waitMessage;
  assert.equal(waiting("stuck"), "drive.operationStatusUnavailable");
  assert.equal(waiting("other"), "drive.deletionDidNotFinish");
  assert.equal(callsOf(server, "part").length, 0);
});

test("a deletion that ended without deleting waits for a decision and continues on one", async (t) => {
  const server = uploadServer();
  server.api.getOperation = async (id) => operation(id, "failed");
  const harness = await mount(t, server.api);
  await harness.settle();
  // The page records an accepted delete so no upload tries that path while it purges.
  harness.persistence.trackFileOperation(operation("delete-failed"), "Health/file");
  enqueue(harness, [entry("file", "ab")], "Health");
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  const id = queue.tasks[0].id;
  assert.equal(queue.tasks[0].waitMessage, "drive.waitingForDeletion");
  queue = await harness.tick();
  // A terminal deletion has released its guard, so nothing on the server still blocks;
  // the row says what happened and offers the only decision, instead of promising a
  // start that no later moment can produce.
  assert.equal(queue.tasks[0].waitMessage, "drive.deletionDidNotFinish");
  assert.deepEqual(queue.tasks[0].waitingFor.state, "failed");
  assert.deepEqual(queue.tasks[0].waitingFor.operationId, "delete-failed");
  assert.equal(callsOf(server, "create").length, 0, "no allocation is attempted while a deletion is recorded");
  queue.resume(id);
  queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].id, id);
  assert.equal(harness.persistence.pendingFileOperation("Health/file"), undefined, "Continue drops the finished deletion");
  assert.equal(callsOf(server, "create").length, 1);
  assert.equal(callsOf(server, "abort").length, 0, "a finished deletion is never aborted");
  assert.equal(callsOf(server, "commit").length, 1);
});

test("a wait ends when its record is settled, without touching the row", async (t) => {
  const server = uploadServer();
  server.api.getOperation = async (id) => operation(id, "running");
  const harness = await mount(t, server.api);
  await harness.settle();
  harness.persistence.trackFileOperation(operation("delete-running"), "Health/file");
  enqueue(harness, [entry("file", "ab")], "Health");
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  assert.equal(callsOf(server, "create").length, 0);
  // The page drops a record as soon as it reads the Operation behind it. The row must
  // follow the record rather than the id it once latched: nothing else would ever move it.
  harness.persistence.settleFileOperation("delete-running");
  await harness.tick();
  const queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].status, "completed");
});

test("a deletion record the server no longer knows continues without asking", async (t) => {
  const server = uploadServer();
  server.api.getOperation = async () => { throw new TestApiError("operation not found", 404, { code: "not_found" }); };
  const harness = await mount(t, server.api);
  await harness.settle();
  harness.persistence.trackFileOperation(operation("delete-pruned"), "Health/file");
  enqueue(harness, [entry("file", "ab")], "Health");
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  await harness.tick();
  const queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].status, "completed");
  assert.equal(harness.persistence.pendingFileOperation("Health/file"), undefined, "a pruned record is dropped");
  assert.equal(callsOf(server, "create").length, 1);
});
