import assert from "node:assert/strict";
import test from "node:test";
import { conflict, digest, memoryStorage, operation, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const entry = () => ({ path: "file", file: new File(["ab"], "file", { type: "application/json" }) });
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
const complete = (queue) => queue.tasks[0]?.status === "completed";
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }
function enqueue(harness) { harness.render().enqueue([entry()], "folder", () => false); }

async function blockedUpload(state = "open") {
  const server = uploadServer();
  const create = server.api.createUpload;
  const session = await create({ path: "folder/file", size: 2, mediaType: "application/json", condition: { mode: "create" } }, "old-page-key");
  const blocker = server.sessions.get(session.id);
  blocker.state = state;
  server.calls.length = 0;
  const attempts = [];
  server.api.createUpload = async (...args) => {
    attempts.push([structuredClone(args[0]), args[1]]);
    if (attempts.length === 1 || !["aborted", "expired", "failed"].includes(blocker.state)) {
      server.calls.push(["blocked-create", session.id]); throw conflict(session.id);
    }
    return create(...args);
  };
  return { server, blocker, attempts };
}

function assertNoPublication(server) {
  assert.equal(callsOf(server, "create").length, 0, "a blocked allocation must not create another session");
  assert.equal(callsOf(server, "part").length, 0, "a blocked upload must not send file bytes");
  assert.equal(callsOf(server, "commit").length, 0, "a blocked upload must not publish");
  assert.equal(server.sessions.size, 1);
}

function assertFreshPublication(server, attempts) {
  assert.equal(attempts.length, 2);
  assert.equal(attempts[0][1], attempts[1][1], "retry keeps this page's exact allocation key");
  assert.notEqual(attempts[1][1], "old-page-key", "never reuse the discarded page's allocation identity");
  assert.deepEqual(attempts[0][0], attempts[1][0]);
  assert.equal(server.sessions.size, 2);
  assert.deepEqual(callsOf(server, "part").map(([, id, number]) => [id, number]), [["session-2", 1]]);
  assert.deepEqual(callsOf(server, "commit"), [["commit", "session-2"]]);
}

test("fresh mount ignores an old open upload; explicit selection and confirmation abort it before a fresh allocation", async (t) => {
  const { server, blocker, attempts } = await blockedUpload();
  blocker.path = "/folder/file";
  server.receipts.get(blocker.id).set(1, { number: 1, size: 2, sha256: digest("xx") });
  const prompts = [], storage = memoryStorage();
  storage.setItem("eam.uploads.v1:owner-a", JSON.stringify([{ id: "old-task", path: "folder/file", status: "uploading", uploadId: blocker.id, cancelRequested: true }]));
  const harness = await mount(t, server.api, { storage, confirm: (message) => { prompts.push(message); return true; } });
  assert.deepEqual((await harness.tick()).tasks, []);
  assert.equal(storage.getItem("eam.uploads.v1:owner-a"), null);
  assert.deepEqual(server.calls, [], "mount does not query, cancel or replay discarded uploads");
  assert.deepEqual(prompts, []);
  enqueue(harness);
  const queue = await harness.until(complete);
  assert.deepEqual(prompts, ["drive.restartUploadConfirm"]);
  assert.equal(blocker.state, "aborted");
  assert.deepEqual(server.calls.slice(0, 4).map(([kind, id]) => [kind, typeof id === "string" ? id : id.path]), [
    ["blocked-create", "session-1"], ["status", "session-1"], ["abort", "session-1"], ["create", "folder/file"],
  ]);
  assertFreshPublication(server, attempts);
  assert.equal(await callsOf(server, "part")[0][3].text(), "ab", "the fresh session sends the newly selected bytes");
  assert.equal(queue.tasks[0].file, null);
  assert.equal(harness.completed.length, 1);
  assert.equal(queue.hasActiveUploads, false);
});

test("declining restart fails locally and idle polling never retries, while explicit Retry can request confirmation again", async (t) => {
  const { server, blocker, attempts } = await blockedUpload();
  let accepted = false;
  const prompts = [];
  const harness = await mount(t, server.api, { confirm: (message) => { prompts.push(message); return accepted; } });
  enqueue(harness);
  const failed = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  const id = failed.tasks[0].id;
  assert.equal(failed.tasks[0].error, "drive.uploadAlreadyInProgress");
  assert.ok(failed.tasks[0].file instanceof File);
  assert.equal(failed.hasActiveUploads, false);
  assert.equal(harness.listeners.get("beforeunload")?.size ?? 0, 0);
  assert.equal(blocker.state, "open");
  assertNoPublication(server);
  assert.equal(callsOf(server, "abort").length, 0);
  await harness.tick();
  assert.equal(attempts.length, 1);
  assert.deepEqual(prompts, ["drive.restartUploadConfirm"]);
  accepted = true;
  failed.retry(id);
  const queue = await harness.until(complete);
  assert.equal(queue.tasks[0].id, id);
  assert.deepEqual(prompts, ["drive.restartUploadConfirm", "drive.restartUploadConfirm"]);
  assert.deepEqual(callsOf(server, "abort"), [["abort", blocker.id]]);
  assert.equal(attempts.length, 3, "the explicit retry encounters the retained reservation before allocating again");
  assert.ok(attempts.every(([, key]) => key === attempts[0][1]));
  assert.equal(server.sessions.size, 2);
  assert.deepEqual(callsOf(server, "part").map(([, id]) => id), ["session-2"]);
  assert.deepEqual(callsOf(server, "commit"), [["commit", "session-2"]]);
});

for (const [label, changes] of [
  ["a different session ID", { id: "unrelated-session" }],
  ["a different path", { path: "private/file" }],
  ["a descendant path", { path: "folder/file/child" }],
  ["a committing upload", { state: "committing" }],
  ["an upload with unknown publication outcome", { state: "outcome_unknown" }],
  ["an already committed upload", { state: "committed" }],
]) {
  test(`a blocker reporting ${label} never authorizes abort or allocation`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    const status = server.api.uploadStatus;
    server.api.uploadStatus = async (...args) => ({ ...await status(...args), ...changes });
    let prompts = 0;
    const harness = await mount(t, server.api, { confirm: () => { prompts++; return true; } });
    enqueue(harness);
    const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
    assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
    assert.equal(queue.hasActiveUploads, true);
    assert.equal(prompts, 0);
    assert.equal(attempts.length, 1);
    assert.deepEqual(callsOf(server, "status"), [["status", blocker.id]]);
    assert.equal(callsOf(server, "abort").length, 0);
    assertNoPublication(server);
  });
}

test("definitive non-upload blocker status retains exact Operation waiting without confirmation or abort", async (t) => {
  const { server, blocker, attempts } = await blockedUpload();
  server.api.uploadStatus = async (id) => { server.calls.push(["status", id]); throw new TestApiError("not an upload", 404); };
  let prompts = 0;
  const harness = await mount(t, server.api, { confirm: () => { prompts++; return true; } });
  enqueue(harness);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
  assert.equal(attempts.length, 1);
  assert.equal(prompts, 0);
  assert.equal(callsOf(server, "abort").length, 0);
  assertNoPublication(server);
});

for (const error of [new TestApiError("status unavailable", 503), new Error("offline")]) {
  test(`transient blocker status failure (${error.message}) remains retryable and explicit Retry can confirm a restart`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    const status = server.api.uploadStatus;
    let failure = error;
    server.api.uploadStatus = async (...args) => {
      if (failure) { server.calls.push(["status", args[0]]); throw failure; }
      return status(...args);
    };
    const prompts = [];
    const harness = await mount(t, server.api, { confirm: (message) => { prompts.push(message); return true; } });
    enqueue(harness);
    const failed = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    const id = failed.tasks[0].id, originalFile = failed.tasks[0].file;
    assert.equal(failed.tasks[0].error, error.message);
    assert.equal(failed.tasks[0].waitingFor, undefined, "a transient lookup failure cannot trap the task in Operation waiting");
    assert.ok(originalFile instanceof File);
    assert.equal(failed.hasActiveUploads, false);
    assert.equal(harness.listeners.get("beforeunload")?.size ?? 0, 0);
    assert.deepEqual(prompts, []);
    assert.equal(callsOf(server, "abort").length, 0);
    assertNoPublication(server);
    await harness.tick();
    assert.equal(attempts.length, 1, "idle polling never silently retries a failed lookup");
    assert.equal(callsOf(server, "operation").length, 0);
    failure = undefined;
    failed.retry(id);
    const queue = await harness.until(complete);
    assert.equal(queue.tasks[0].id, id);
    assert.deepEqual(prompts, ["drive.restartUploadConfirm"]);
    assert.deepEqual(callsOf(server, "status"), [["status", blocker.id], ["status", blocker.id]]);
    assert.deepEqual(callsOf(server, "abort"), [["abort", blocker.id]]);
    assert.equal(blocker.state, "aborted");
    assert.equal(attempts.length, 3, "Retry inspects the same retained blocker before a fresh allocation");
    assert.ok(attempts.every(([, key]) => key === attempts[0][1]), "Retry retains the original allocation key");
    for (const [intent] of attempts) assert.deepEqual(intent, attempts[0][0]);
    assert.equal(server.sessions.size, 2);
    assert.deepEqual(callsOf(server, "part").map(([, id]) => id), ["session-2"]);
    assert.deepEqual(callsOf(server, "commit"), [["commit", "session-2"]]);
    assert.equal(harness.completed[0].task.file, originalFile);
    assert.equal(queue.hasActiveUploads, false);
  });
}

for (const terminal of ["aborted", "expired", "failed"]) {
  test(`lost abort ACK with exact ${terminal} session and cancelled Operation safely allocates a fresh session`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    server.api.abortUpload = async (id) => { server.calls.push(["abort", id]); blocker.state = terminal; throw new TestApiError("abort ACK lost"); };
    server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, "cancelled", null, "upload_commit"); };
    let prompts = 0;
    const harness = await mount(t, server.api, { confirm: () => { prompts++; return true; } });
    enqueue(harness);
    await harness.until(complete);
    assert.equal(prompts, 1);
    assert.deepEqual(callsOf(server, "status"), [["status", blocker.id], ["status", blocker.id]]);
    assert.deepEqual(callsOf(server, "abort"), [["abort", blocker.id]]);
    assert.deepEqual(callsOf(server, "operation"), [["operation", blocker.id]]);
    assertFreshPublication(server, attempts);
  });
}

for (const [label, settled, opState, opId] of [
  ["an open session", { state: "open" }, "cancelled"],
  ["a committing session", { state: "committing" }, "cancelled"],
  ["an unknown publication outcome", { state: "outcome_unknown" }, "cancelled"],
  ["a different session ID", { state: "aborted", id: "unrelated-session" }, "cancelled"],
  ["a different path", { state: "aborted", path: "private/file" }, "cancelled"],
  ["a running Operation", { state: "aborted" }, "running"],
  ["a failed Operation", { state: "aborted" }, "failed"],
  ["an unknown Operation outcome", { state: "aborted" }, "outcome_unknown"],
  ["a different Operation ID", { state: "aborted" }, "cancelled", "unrelated-operation"],
]) {
  test(`lost abort ACK followed by ${label} cannot authorize another allocation`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    const status = server.api.uploadStatus;
    let aborted = false;
    server.api.abortUpload = async (id) => { server.calls.push(["abort", id]); aborted = true; throw new TestApiError("abort ACK lost"); };
    server.api.uploadStatus = async (...args) => ({ ...await status(...args), ...(aborted ? settled : {}) });
    server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(opId ?? id, opState, null, "upload_commit"); };
    const harness = await mount(t, server.api, { confirm: () => true });
    enqueue(harness);
    const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    assert.equal(queue.tasks[0].error, "abort ACK lost");
    assert.equal(queue.hasActiveUploads, false);
    await harness.tick();
    assert.equal(attempts.length, 1);
    assert.deepEqual(callsOf(server, "abort"), [["abort", blocker.id]]);
    assertNoPublication(server);
  });
}

for (const terminal of ["aborted", "expired", "failed"]) {
  for (const opState of ["cancelled", "failed", "running", "outcome_unknown"]) {
    test(`an initially ${terminal} blocker with ${opState} Operation ${opState === "cancelled" ? "permits a fresh allocation" : "keeps exact Operation waiting"}`, async (t) => {
      const { server, blocker, attempts } = await blockedUpload(terminal);
      server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, opState, null, "upload_commit"); };
      let prompts = 0;
      const harness = await mount(t, server.api, { confirm: () => { prompts++; return true; } });
      enqueue(harness);
      const queue = await harness.until(opState === "cancelled" ? complete : (queue) => queue.tasks[0]?.status === "waiting");
      assert.deepEqual(callsOf(server, "operation"), [["operation", blocker.id]]);
      assert.equal(callsOf(server, "abort").length, 0);
      assert.equal(prompts, 0, "a terminal session is never aborted or presented as open");
      if (opState === "cancelled") assertFreshPublication(server, attempts);
      else {
        assert.equal(attempts.length, 1);
        assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
        assertNoPublication(server);
      }
    });
  }
}

test("an initially terminal blocker cannot use another Operation's cancellation as proof", async (t) => {
  const { server, blocker, attempts } = await blockedUpload("aborted");
  server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation("different-operation", "cancelled"); };
  const harness = await mount(t, server.api, { confirm: () => assert.fail("terminal blocker cannot prompt for abort") });
  enqueue(harness);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
  assert.equal(attempts.length, 1);
  assert.equal(callsOf(server, "abort").length, 0);
  assertNoPublication(server);
});
