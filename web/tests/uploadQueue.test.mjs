import assert from "node:assert/strict";
import test from "node:test";
import { conflict, deferred, digest, item, memoryStorage, operation, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const entry = (path, bytes = "abc") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json", lastModified: 1443268800000 }) });
const allComplete = (queue) => queue.tasks.length > 0 && queue.tasks.every((task) => task.status === "completed");
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }
function enqueue(harness, entries, dir = "") { harness.render().enqueue(entries, dir, () => false); }
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);

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
    let queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    const before = JSON.parse(harness.storage.getItem("eam.uploads.v1:owner-a"))[0];
    assert.ok(before.intent, "intent persists before sending create");
    assert.equal(before.file, undefined, "never serialize browser File handles");
    const id = queue.tasks[0].id;
    queue.retry(id);
    queue = await harness.until(allComplete);
    assert.equal(queue.tasks[0].id, id);
    assert.equal(queue.tasks[0].file, null);
    assert.equal(server.sessions.size, 1);
    assert.equal(callsOf(server, "part").length, 1, "a durable receipt suppresses resend after lost ACK");
    assert.equal(callsOf(server, "commit").length, 1);
    assert.equal(harness.completed.length, 1);
    const creates = callsOf(server, "create");
    assert.equal(creates.length, lostAck === "create" ? 2 : 1);
    if (lostAck === "create") {
      assert.equal(creates[0][2], creates[1][2]);
      assert.deepEqual(creates[0][1], creates[1][1]);
    } else assert.ok(callsOf(server, "status").some(([, id]) => id === "session-1"));
    assert.deepEqual(JSON.parse(harness.storage.getItem("eam.uploads.v1:owner-a")), []);
  });
}

test("known rejected allocation stays failed on reload, cancels locally and retries only on explicit action", async (t) => {
  const server = uploadServer();
  const storage = memoryStorage();
  const attempts = [];
  server.api.createUpload = async (intent, key) => {
    attempts.push([structuredClone(intent), key]);
    throw new TestApiError("quota exceeded", 507, { code: "quota_exceeded" });
  };
  const first = await mount(t, server.api, { storage });
  enqueue(first, [entry("file", "ab")]);
  const failed = await first.until((queue) => queue.tasks[0]?.status === "failed");
  const id = failed.tasks[0].id;
  assert.equal(failed.tasks[0].uploadId, undefined);
  const saved = storage.getItem("eam.uploads.v1:owner-a");
  first.unmount();

  const reloaded = await mount(t, server.api, { storage });
  let queue = await reloaded.until((queue) => queue.tasks[0]?.status === "failed");
  await reloaded.tick();
  assert.equal(attempts.length, 1, "reload must not allocate after an explicit server rejection");
  assert.equal(queue.tasks[0].id, id);
  assert.equal(queue.tasks[0].error, "quota exceeded");
  queue.cancel(id);
  await reloaded.until((queue) => queue.tasks.length === 0);
  assert.equal(attempts.length, 1, "Cancel must not replay a known rejected create");
  assert.equal(callsOf(server, "abort").length, 0);
  reloaded.unmount();

  // Restore the same rejected draft to exercise Retry independently of Cancel.
  storage.setItem("eam.uploads.v1:owner-a", saved);
  const retrying = await mount(t, server.api, { storage });
  queue = await retrying.until((queue) => queue.tasks[0]?.status === "failed");
  queue.retry(id);
  await retrying.until((queue) => queue.tasks[0]?.status === "failed" && attempts.length === 2);
  assert.equal(attempts[0][1], attempts[1][1], "explicit Retry may replay the rejected allocation key");
  assert.deepEqual(attempts[0][0], attempts[1][0]);
  assert.equal(server.sessions.size, 0);
  assert.equal(callsOf(server, "part").length, 0);
});

test("reload requires reselection, verifies stored receipt hashes and sends only missing numbered parts", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  server.api.uploadPart = async (...args) => { await send(...args); throw new TestApiError("ACK dropped"); };
  const storage = memoryStorage();
  const first = await mount(t, server.api, { storage });
  enqueue(first, [entry("folder/file", "abcd")]);
  const initial = await first.until((queue) => queue.tasks[0]?.status === "failed");
  const id = initial.tasks[0].id;
  first.unmount();
  server.api.uploadPart = send;
  const reloaded = await mount(t, server.api, { storage });
  let queue = await reloaded.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(queue.tasks[0].file, null);
  assert.equal(queue.tasks[0].uploadId, "session-1");
  assert.equal(queue.tasks[0].error, "drive.reselectUpload");
  assert.equal(callsOf(server, "part").length, 1);
  enqueue(reloaded, [entry("folder/file", "axcd")]);
  queue = await reloaded.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(queue.tasks[0].error, "drive.reselectedFileChanged");
  assert.equal(callsOf(server, "part").length, 1, "same size with changed bytes cannot resume");
  // Correcting a rejected selection should keep the original session without another reload.
  enqueue(reloaded, [entry("folder/file", "abcd")]);
  queue = await reloaded.until(allComplete, "correct original must replace the rejected File selection");
  assert.equal(queue.tasks[0].id, id);
  assert.deepEqual(callsOf(server, "part").map(([, id, number]) => [id, number]), [["session-1", 1], ["session-1", 2]]);
  assert.equal(server.sessions.size, 1);
});

test("persisted sessions and pending deletes are scoped to the authenticated owner", async (t) => {
  const server = uploadServer();
  server.api.createUpload = async () => { throw new TestApiError("offline"); };
  const storage = memoryStorage();
  const first = await mount(t, server.api, { storage });
  enqueue(first, [entry("private")]);
  await first.until((queue) => queue.tasks[0]?.status === "failed");
  first.persistence.trackFileOperation(operation("private-delete"), "private");
  first.unmount();
  const other = await mount(t, { ...server.api, me: async () => ({ id: "owner-b" }) }, { storage, owner: "owner-b" });
  assert.deepEqual((await other.settle()).tasks, []);
  assert.deepEqual(other.persistence.pendingFileOperations(), []);
  assert.ok(storage.getItem("eam.uploads.v1:owner-a"));
});

test("receipt pagination must have one watermark and validate every resumed part before sending bytes", async (t) => {
  for (const scenario of ["changed watermark", "duplicate receipt", "wrong hash", "wrong size"]) {
    const server = uploadServer();
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

test("zero-byte upload commits an empty session without a part request", async (t) => {
  const server = uploadServer();
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("empty", "")]);
  const queue = await harness.until(allComplete);
  assert.equal(queue.tasks[0].size, 0);
  assert.equal(callsOf(server, "part").length, 0);
  assert.equal(callsOf(server, "commit").length, 1);
});

test("part turns let a small file finish while two larger files are partially uploaded", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  const gates = [deferred(), deferred()];
  let active = 0, peak = 0;
  server.api.uploadPart = async (...args) => {
    active++; peak = Math.max(peak, active);
    try {
      if (args[1] === 1 && args[0] !== "session-3") await gates[Number(args[0].slice(-1)) - 1].promise;
      return await send(...args);
    } finally { active--; }
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("large-a", "abcdefgh"), entry("large-b", "abcdefgh"), entry("small", "x")]);
  await harness.settle();
  assert.equal(server.sessions.size, 2, "only two active files may allocate/send");
  gates.forEach((gate) => gate.resolve());
  await harness.until(allComplete);
  assert.ok(peak <= 2);
  const parts = callsOf(server, "part");
  const small = parts.findIndex(([, id]) => id === "session-3");
  assert.ok(small >= 0 && small < parts.findIndex(([, id, number]) => id === "session-1" && number === 4), "small file receives a turn before large-a drains");
  assert.ok(small < parts.findIndex(([, id, number]) => id === "session-2" && number === 4), "small file receives a turn before large-b drains");
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

test("failed, cancelled, unknown, unavailable and wrong delete identities never authorize bytes", async (t) => {
  const server = uploadServer();
  server.api.createUpload = async () => { server.calls.push(["create"]); throw conflict(); };
  let response = operation("delete-1", "running");
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); if (response instanceof Error) throw response; return response; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("folder/file")]);
  await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  for (const value of [operation("delete-1", "failed"), operation("delete-1", "cancelled"), operation("delete-1", "outcome_unknown"), new TestApiError("missing", 404), new Error("offline"), operation("other", "succeeded"), {}]) {
    response = value;
    const queue = await harness.tick();
    assert.equal(queue.tasks[0].status, "waiting");
    assert.equal(queue.tasks[0].speed, 0);
    assert.equal(callsOf(server, "create").length, 1);
    assert.equal(callsOf(server, "part").length, 0);
  }
});

test("pending ancestor delete blocks mkdir and nested children while prefix siblings continue", async (t) => {
  const server = uploadServer();
  const mkdir = server.api.mkdir;
  const gate = deferred();
  let done = false;
  server.api.mkdir = async (path) => { if (path === "root") await gate.promise; return mkdir(path); };
  server.api.getOperation = async (id) => operation(id, done ? "succeeded" : "running");
  const harness = await mount(t, server.api);
  await harness.settle();
  harness.persistence.trackFileOperation(operation("delete-root"), "root");
  enqueue(harness, [{ path: "root", file: null }, { path: "root/nested", file: null }, entry("root/nested/file"), entry("rootish/file")]);
  let queue = await harness.until((queue) => queue.tasks[3]?.status === "completed");
  assert.equal(queue.tasks[0].status, "waiting");
  assert.equal(callsOf(server, "mkdir").length, 0);
  assert.equal(callsOf(server, "create").length, 1);
  done = true;
  await harness.tick();
  await harness.settle();
  assert.equal(callsOf(server, "create").length, 1, "children stay blocked while parent mkdir is unresolved");
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
  enqueue(harness, [{ path: "folder", file: null }, entry("folder/file")]);
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
  const gate = deferred();
  let signal;
  server.api.uploadPart = async (id, number, blob, receivedSignal) => { server.calls.push(["part", id, number]); signal = receivedSignal; await gate.promise; return { number, size: blob.size, sha256: digest("ab") }; };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file", "ab")]);
  await harness.until(() => Boolean(signal));
  harness.render().cancel(harness.render().tasks[0].id);
  await harness.settle();
  assert.equal(signal.aborted, true);
  assert.equal(callsOf(server, "abort").length, 1);
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
  assert.deepEqual(callsOf(server, "cancelOperation"), [["cancelOperation", "publish-1"]]);
  await harness.tick();
  assert.deepEqual(harness.render().tasks, []);
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

test("cancel waiting folder removes descendants and a late poll cannot revive replacement tasks", async (t) => {
  const server = uploadServer();
  const gate = deferred();
  server.api.mkdir = async () => { throw conflict(); };
  server.api.getOperation = async () => gate.promise;
  const harness = await mount(t, server.api);
  const entries = [{ path: "folder", file: null }, entry("folder/file")];
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

test("reload of a committing session follows its original Operation without file reselection", async (t) => {
  const server = uploadServer();
  server.api.commitUpload = async (id) => {
    server.calls.push(["commit", id]);
    server.sessions.get(id).state = "committing";
    throw new TestApiError("commit ACK lost");
  };
  const storage = memoryStorage();
  const first = await mount(t, server.api, { storage });
  enqueue(first, [entry("file", "ab")]);
  await first.until((queue) => queue.tasks[0]?.status === "failed");
  first.unmount();
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, "succeeded", item("file"), "upload_commit"); };
  const reloaded = await mount(t, server.api, { storage });
  let queue = await reloaded.until((queue) => queue.tasks[0]?.status === "finalizing");
  assert.equal(queue.tasks[0].file, null);
  assert.equal(queue.tasks[0].operationId, "commit-session-1");
  await reloaded.tick();
  queue = await reloaded.until(allComplete);
  assert.equal(queue.tasks[0].file, null);
  assert.equal(callsOf(server, "part").length, 1);
  assert.equal(callsOf(server, "commit").length, 1);
  assert.deepEqual(callsOf(server, "operation"), [["operation", "commit-session-1"]]);
});

test("manual retry of a waiting file must first resolve its exact delete Operation", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  let first = true;
  server.api.createUpload = async (...args) => {
    if (first) { first = false; throw conflict(); }
    return create(...args);
  };
  server.api.getOperation = async (id) => operation(id, "failed");
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("file")]);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  queue.retry(queue.tasks[0].id);
  await harness.settle();
  assert.equal(harness.render().tasks[0].status, "waiting");
  assert.equal(callsOf(server, "part").length, 0, "retry cannot bypass the retained exact delete identity");
  assert.equal(callsOf(server, "commit").length, 0);
});

test("consistent receipt pages skip every saved part, including noncontiguous numbers", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  server.api.createUpload = async (...args) => {
    const session = await create(...args);
    server.receipts.get(session.id).set(1, { number: 1, size: 2, sha256: digest("ab") });
    server.receipts.get(session.id).set(3, { number: 3, size: 1, sha256: digest("e") });
    return session;
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
  user.resolve({ id: "owner-a" });
  await harness.until((queue) => queue.tasks[0]?.status === "failed");
  harness.unmount();
  const reload = await mount(t, server.api, { storage: harness.storage });
  await reload.until((queue) => queue.tasks[0]?.status === "failed");
  const creates = callsOf(server, "create").length;
  enqueue(reload, [entry("file", "longer")]);
  await reload.settle();
  assert.equal(reload.render().tasks[0].error, "drive.reselectedFileChanged");
  assert.equal(callsOf(server, "create").length, creates);
  assert.equal(callsOf(server, "part").length, 0);
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
  test(`reload resumes a persisted cancellation for a session in ${phase} state without resending bytes`, async (t) => {
    const server = uploadServer();
    const storage = memoryStorage();
    const session = await server.api.createUpload({ path: "file", size: 2, mediaType: "application/json", condition: { mode: "create" } }, "saved-key");
    server.sessions.get(session.id).state = phase;
    const task = { id: "persisted-task", createKey: "saved-key", path: "file", name: "file", dir: "", isDir: false, size: 2, overwrite: false, status: "failed", loaded: 2, speed: 0, uploaded: phase === "committing", uploadId: session.id, cancelRequested: true,
      ...(phase === "committing" ? { operationId: session.operationId } : {}) };
    storage.setItem("eam.uploads.v1:owner-a", JSON.stringify([task]));
    if (phase === "committing") {
      server.api.abortUpload = async (id) => { server.calls.push(["abort", id]); throw new TestApiError("publication active", 409, { code: "commit_in_progress" }); };
      server.api.cancelOperation = async (id) => { server.calls.push(["cancelOperation", id]); return operation(id, "cancelled", null, "upload_commit"); };
    }
    const harness = await mount(t, server.api, { storage });
    await harness.until((queue) => queue.tasks.length === 0 && callsOf(server, "abort").length > 0, "persisted cancellation must resume automatically");
    assert.deepEqual(callsOf(server, "abort"), [["abort", session.id]]);
    assert.equal(callsOf(server, "part").length, 0);
    assert.equal(callsOf(server, "commit").length, 0);
    assert.equal(server.sessions.size, 1);
    assert.equal(harness.completed.length, 0);
    if (phase === "committing") assert.deepEqual(callsOf(server, "cancelOperation"), [["cancelOperation", session.operationId]]);
    assert.deepEqual(JSON.parse(storage.getItem("eam.uploads.v1:owner-a")), []);
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
    const commit = server.api.commitUpload;
    let operationCommitted = false;
    server.api.commitUpload = async (id) => {
      if (boundary === "upload abort") { await commit(id); throw new TestApiError("commit ACK lost"); }
      server.sessions.get(id).state = "committing";
      return operation("commit-session-1", "running", null, "upload_commit");
    };
    server.api.abortUpload = async (id) => { server.calls.push(["abort", id]); throw new TestApiError("publication active", 409, { code: boundary === "upload abort" ? "already_committed" : "commit_in_progress" }); };
    server.api.cancelOperation = async (id) => { server.calls.push(["cancelOperation", id]); operationCommitted = true; throw new TestApiError("publication completed", 409, { code: "already_committed" }); };
    server.api.getOperation = async (id) => {
      server.calls.push(["operation", id]);
      return operation(id, operationCommitted || boundary === "upload abort" ? "succeeded" : "running", item("file"), "upload_commit");
    };
    const harness = await mount(t, server.api);
    enqueue(harness, [entry("file", "ab")]);
    const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed" || Boolean(queue.tasks[0]?.operationId));
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

test("reload of an unstarted queued file waits for reselection before binding media metadata", async (t) => {
  const server = uploadServer();
  const storage = memoryStorage();
  storage.setItem("eam.uploads.v1:owner-a", JSON.stringify([{ id: "unstarted", createKey: "unstarted-key", path: "file", name: "file", dir: "", isDir: false, size: 2, modifiedAt: 1443268800000, overwrite: false, status: "queued", loaded: 0, speed: 0, uploaded: false }]));
  const harness = await mount(t, server.api, { storage });
  await harness.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(harness.render().tasks[0].error, "drive.reselectUpload");
  assert.equal(callsOf(server, "create").length, 0, "an unstarted upload cannot bind guessed File metadata");
  enqueue(harness, [entry("file", "ab")]);
  await harness.until(allComplete);
  assert.equal(callsOf(server, "create")[0][1].mediaType, "application/json");
  assert.equal(harness.render().tasks[0].id, "unstarted");
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
  await harness.tick(15000);
  assert.equal(stuckSignal.aborted, true);
  assert.ok(callsOf(server, "operation").some(([, id]) => id === "delete-other"));
  assert.equal(harness.render().tasks[0].waitMessage, "drive.deletionStatusUnavailable");
  assert.equal(harness.render().tasks[1].waitMessage, "drive.deletionNeedsAttention");
  assert.equal(callsOf(server, "part").length, 0);
});
