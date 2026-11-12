import assert from "node:assert/strict";
import test from "node:test";
import { conflict, deferred, digest, item, memoryLocks, memoryStorage, operation, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const entry = () => ({ path: "file", file: new File(["ab"], "file", { type: "application/json" }) });
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
const complete = (queue) => queue.tasks[0]?.status === "completed";
async function mount(t, api, options) {
  const harness = await setup(api, options);
  t.after(harness.unmount);
  return harness;
}
function enqueue(harness) { harness.render().enqueue([entry()], "folder", () => false); }

// A creation is refused by an upload only while that upload is committing; an open session
// for the same path is ended by the server when the new one is admitted (supersedingServer).
async function blockedUpload(state = "committing") {
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
      server.calls.push(["blocked-create", session.id]); throw conflict(session.id, "upload_commit");
    }
    return create(...args);
  };
  return { server, blocker, attempts };
}

// The server's side of "a new session for a path replaces the open one": the older session
// is ended inside the creation, with no request from the client.
function supersedingServer(...args) {
  const server = uploadServer(...args);
  const create = server.api.createUpload;
  server.api.createUpload = async (input, key, ...rest) => {
    if (!server.keys.has(key)) {
      for (const session of server.sessions.values()) if (session.path === input.path && session.state === "open") session.state = "aborted";
    }
    return { ...await create(input, key, ...rest) };
  };
  return server;
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

// A page that was discarded mid-publish leaves the path's session committing. A new
// selection must adopt that publication, not allocate a second session over it and then
// describe the wait as a deletion of the file being uploaded.
test("a path left committing is adopted and finished from its own Operation", async (t) => {
  const { server } = await blockedUpload("committing");
  server.api.getOperation = async (id) => operation(id, "succeeded", item("folder/file"));
  const harness = await mount(t, server.api);
  enqueue(harness);
  const adopted = await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
  assert.equal(adopted.hasActiveUploads, true);
  await harness.tick();
  const queue = await harness.until(complete);
  assert.equal(server.sessions.size, 1, "the committing session was adopted, not duplicated");
  assert.equal(callsOf(server, "part").length, 0);
  assert.equal(callsOf(server, "commit").length, 0, "the adopted session owns the publication");
  assert.equal(queue.tasks[0].status, "completed");
  assert.equal(harness.completed.length, 1);
});

test("fresh mount ignores an old open upload; explicit selection allocates over it without asking about it", async (t) => {
  const server = supersedingServer();
  const old = await server.api.createUpload({ path: "folder/file", size: 2, mediaType: "application/json", condition: { mode: "create" } }, "old-page-key");
  const blocker = server.sessions.get(old.id);
  server.receipts.get(blocker.id).set(1, { number: 1, size: 2, sha256: digest("xx") });
  server.calls.length = 0;
  const storage = memoryStorage();
  storage.setItem("eam.uploads.v1:owner-a", JSON.stringify([{ id: "old-task", path: "folder/file", status: "uploading", uploadId: blocker.id, cancelRequested: true }]));
  const harness = await mount(t, server.api, { storage });
  assert.deepEqual((await harness.tick()).tasks, []);
  assert.equal(storage.getItem("eam.uploads.v1:owner-a"), null);
  assert.deepEqual(server.calls, [], "mount does not query, cancel or replay discarded uploads");
  enqueue(harness);
  const queue = await harness.until(complete);
  assert.equal(blocker.state, "aborted", "the server ended the abandoned session when it admitted the new one");
  assert.equal(server.calls[0][0], "create", "the first request is the allocation itself");
  assert.equal(callsOf(server, "status").filter(([, id]) => id === blocker.id).length, 0, "the abandoned session is never asked about");
  assert.equal(callsOf(server, "abort").length, 0, "and never aborted by the client");
  assert.equal(callsOf(server, "create").length, 1);
  assert.notEqual(callsOf(server, "create")[0][2], "old-page-key", "never reuse the discarded page's allocation identity");
  assert.equal(server.sessions.size, 2);
  assert.deepEqual(callsOf(server, "part").map(([, id, number]) => [id, number]), [["session-2", 1]]);
  assert.deepEqual(callsOf(server, "commit"), [["commit", "session-2"]]);
  assert.equal(await callsOf(server, "part")[0][3].text(), "ab", "the fresh session sends the newly selected bytes");
  assert.equal(queue.tasks[0].file, null);
  assert.equal(harness.completed.length, 1);
  assert.equal(queue.hasActiveUploads, false);
});

for (const release of ["unmount", "cancel", "complete"]) {
  test(`another page cannot touch an owned path until ${release}; explicit Retry then claims it automatically`, async (t) => {
    const server = supersedingServer(1);
    const locks = memoryLocks(), gate = deferred();
    const send = server.api.uploadPart;
    let firstSignal;
    server.api.uploadStatus = async (id) => {
      server.calls.push(["status", id]);
      return { ...server.sessions.get(id) };
    };
    server.api.uploadPart = async (...args) => {
      if (args[0] === "session-1" && args[1] === 2) { firstSignal = args[3]; await gate.promise; }
      return send(...args);
    };
    const first = await mount(t, server.api, { locks });
    enqueue(first);
    const uploading = await first.until(() => Boolean(firstSignal));
    const original = uploading.tasks[0];
    const secondCalls = [];
    const observedAPI = Object.fromEntries(Object.entries(server.api).map(([name, method]) => [name, (...args) => {
      if (name !== "me") secondCalls.push(name);
      return method(...args);
    }]));
    const second = await mount(t, observedAPI, { locks: first.locks });
    enqueue(second);
    const failed = await second.until((queue) => queue.tasks[0]?.status === "failed");
    const id = failed.tasks[0].id;
    assert.equal(failed.tasks[0].error, "drive.uploadAlreadyInProgress");
    assert.ok(failed.tasks[0].file instanceof File);
    assert.equal(failed.hasActiveUploads, false);
    assert.equal(second.listeners.get("beforeunload")?.size ?? 0, 0);
    assert.deepEqual(secondCalls, [], "held ownership blocks capabilities, status, abort and allocation requests");
    assert.equal(first.render().tasks[0].id, original.id);
    assert.equal(first.render().tasks[0].file, original.file);
    assert.equal(firstSignal.aborted, false);
    assert.equal(server.sessions.get("session-1").state, "open");
    assert.equal(callsOf(server, "abort").length, 0);
    await second.tick();
    assert.deepEqual(secondCalls, [], "idle polling never retries a busy path");
    if (release === "unmount") first.unmount();
    else if (release === "cancel") {
      first.render().cancel(original.id);
      await first.until((queue) => queue.tasks.length === 0);
    } else {
      gate.resolve();
      await first.until(complete);
    }
    await second.tick();
    assert.deepEqual(secondCalls, [], "ownership release still requires explicit Retry");
    failed.retry(id);
    const queue = await second.until(complete);
    assert.equal(queue.tasks[0].id, id);
    assert.equal(server.sessions.size, 2);
    assert.equal(second.completed.length, 1);
    // Only a page's own cancel aborts a session. A session its page merely left behind is
    // ended by the server when the retry is admitted, so the claiming page asks nothing
    // about it and aborts nothing.
    assert.deepEqual(callsOf(server, "abort"), release === "cancel" ? [["abort", "session-1"]] : []);
    assert.ok(!secondCalls.includes("abortUpload"), "the claiming page never aborts another page's session");
    if (release === "unmount") {
      assert.deepEqual(secondCalls.slice(0, 2), ["capabilities", "createUpload"]);
      assert.equal(server.sessions.get("session-1").state, "aborted");
    }
    gate.resolve();
    await first.settle();
    assert.equal(first.completed.length, release === "complete" ? 1 : 0, "late old-page bytes never publish after release");
  });
}

test("failed and pending cancellation retain ownership until confirmed removal", async (t) => {
  const server = uploadServer(), locks = memoryLocks(), cancellation = deferred();
  const abort = server.api.abortUpload;
  let attempts = 0;
  server.api.uploadPart = async () => { throw new TestApiError("part rejected", 400); };
  server.api.abortUpload = async (...args) => {
    if (++attempts === 1) { await cancellation.promise; throw new TestApiError("abort unavailable"); }
    return abort(...args);
  };
  const first = await mount(t, server.api, { locks });
  enqueue(first);
  const failed = await first.until((queue) => queue.tasks[0]?.status === "failed");
  const original = failed.tasks[0];
  const otherServer = uploadServer();
  const second = await mount(t, otherServer.api, { locks });
  enqueue(second);
  const busy = await second.until((queue) => queue.tasks[0]?.status === "failed");
  const id = busy.tasks[0].id;
  assert.equal(busy.tasks[0].error, "drive.uploadAlreadyInProgress", "a retryable upload failure still owns its path");
  failed.cancel(original.id);
  await first.until((queue) => queue.tasks[0]?.status === "finalizing");
  busy.retry(id);
  await second.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(second.render().tasks[0].error, "drive.uploadAlreadyInProgress", "cancelling the initial batch must not release ownership early");
  assert.deepEqual(otherServer.calls, []);
  cancellation.resolve();
  const cancelFailed = await first.until((queue) => queue.tasks[0]?.error === "abort unavailable");
  assert.equal(cancelFailed.tasks[0].cancelRequested, true);
  busy.retry(id);
  await second.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(second.render().tasks[0].error, "drive.uploadAlreadyInProgress", "failed cancellation still owns its path");
  assert.deepEqual(otherServer.calls, []);
  cancelFailed.retry(original.id);
  await first.until((queue) => queue.tasks.length === 0);
  assert.equal(attempts, 2);
  busy.retry(id);
  await second.until(complete);
  assert.equal(otherServer.sessions.size, 1);
});

for (const scenario of ["missing", "missing request", "throws", "rejects"]) {
  test(`${scenario} Web Locks fails inline without API effects and remains explicitly retryable`, async (t) => {
    const server = uploadServer(), manager = memoryLocks();
    let attempts = 0, unavailable = true;
    const locks = scenario === "missing" ? undefined : scenario === "missing request" ? {} : {
      request(...args) {
        attempts++;
        if (!unavailable) return manager.request(...args);
        if (scenario === "throws") throw new Error("Web Locks denied");
        return Promise.reject(new Error("Web Locks denied"));
      },
    };
    const calls = [];
    const api = Object.fromEntries(Object.entries(server.api).map(([name, method]) => [name, (...args) => {
      if (name !== "me") calls.push(name);
      return method(...args);
    }]));
    const harness = await mount(t, api, { locks });
    enqueue(harness);
    const failed = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    const original = failed.tasks[0];
    assert.equal(original.error, "drive.uploadCoordinationUnavailable");
    assert.ok(original.file instanceof File);
    assert.equal(original.intent, undefined);
    assert.equal(original.uploadId, undefined);
    assert.equal(failed.hasActiveUploads, false);
    assert.equal(harness.listeners.get("beforeunload")?.size ?? 0, 0);
    assert.deepEqual(calls, [], "unavailable ownership cannot query, allocate, abort or send bytes");
    await harness.tick();
    assert.equal(attempts, locks?.request ? 1 : 0, "idle polling never retries acquisition");
    failed.retry(original.id);
    const retried = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    assert.equal(retried.tasks[0].error, "drive.uploadCoordinationUnavailable");
    assert.equal(retried.tasks[0].id, original.id);
    assert.equal(retried.tasks[0].file, original.file);
    assert.deepEqual(calls, []);
    if (locks?.request) {
      assert.equal(attempts, 2);
      unavailable = false;
      retried.retry(original.id);
      await harness.until(complete);
      assert.equal(attempts, 3, "an explicit retry can recover ownership when the browser becomes available");
      assert.equal(server.sessions.size, 1);
    }
  });
}

test("shared ownership uses the authenticated owner and normalized exact target path", async (t) => {
  const locks = memoryLocks(), firstServer = uploadServer(), gate = deferred();
  let sending = false;
  firstServer.api.uploadPart = async () => { sending = true; return gate.promise; };
  const first = await mount(t, firstServer.api, { locks });
  first.render().enqueue([{ ...entry(), path: "cafe\u0301" }], "/folder", () => false);
  await first.until(() => sending);
  const secondServer = uploadServer();
  const second = await mount(t, secondServer.api, { locks });
  second.render().enqueue([{ ...entry(), path: "caf\u00e9" }], "folder", () => false);
  const busy = await second.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(busy.tasks[0].error, "drive.uploadAlreadyInProgress");
  assert.deepEqual(secondServer.calls, []);
  const otherOwner = await mount(t, { ...secondServer.api, me: async () => ({ id: "owner-b" }) }, { locks });
  otherOwner.render().enqueue([{ ...entry(), path: "caf\u00e9" }], "folder", () => false);
  await otherOwner.until(complete);
  const otherPath = await mount(t, secondServer.api, { locks });
  otherPath.render().enqueue([{ ...entry(), path: "caf\u00e9-child" }], "folder", () => false);
  await otherPath.until(complete);
  assert.equal(secondServer.sessions.size, 2, "other owners and sibling paths claim independently");
  first.unmount();
  gate.resolve({ number: 1, size: 2, sha256: digest("ab") });
  await first.settle();
});

test("the shared fake lock manager holds callback promises and aborts pending requests", async () => {
  const locks = memoryLocks(), gate = deferred(), entered = deferred();
  const held = locks.request("path", { mode: "exclusive", ifAvailable: true }, async (lock) => {
    assert.equal(lock.name, "path");
    entered.resolve();
    await gate.promise;
    return "released";
  });
  await entered.promise;
  assert.equal(await locks.request("path", { mode: "exclusive", ifAvailable: true }, (lock) => lock), null);
  const controller = new AbortController();
  let delivered = false;
  const pending = locks.request("path", { mode: "exclusive", signal: controller.signal }, () => { delivered = true; });
  const cancelled = assert.rejects(pending, { name: "AbortError" });
  controller.abort();
  await cancelled;
  assert.equal(delivered, false);
  assert.equal((await locks.query()).held.length, 1);
  gate.resolve();
  assert.equal(await held, "released");
  assert.equal(await locks.request("path", { mode: "exclusive", ifAvailable: true }, (lock) => lock.name), "path");
  assert.deepEqual(await locks.query(), { held: [], pending: [] });
});

for (const [label, changes] of [
  ["a different session ID", { id: "unrelated-session" }],
  ["a different path", { path: "private/file" }],
  ["a descendant path", { path: "folder/file/child" }],
]) {
  test(`a blocker reporting ${label} never authorizes abort or allocation`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    const status = server.api.uploadStatus;
    server.api.uploadStatus = async (...args) => ({ ...await status(...args), ...changes });
    const harness = await mount(t, server.api);
    enqueue(harness);
    const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
    assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
    assert.equal(queue.hasActiveUploads, true);
    assert.equal(attempts.length, 1);
    assert.deepEqual(callsOf(server, "status"), [["status", blocker.id]]);
    assert.equal(callsOf(server, "abort").length, 0);
    assertNoPublication(server);
  });
}

// A blocker that is this path's own earlier session, already publishing, must be adopted:
// the row follows its Operation to the result instead of allocating over it, aborting it,
// or describing it as a deletion of the file being uploaded.
for (const [label, changes] of [
  ["a committing upload", { state: "committing" }],
  ["an upload with unknown publication outcome", { state: "outcome_unknown" }],
  ["an already committed upload", { state: "committed" }],
]) {
  test(`a blocker reporting ${label} is adopted, never aborted or reallocated`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    const status = server.api.uploadStatus;
    server.api.uploadStatus = async (...args) => ({ ...await status(...args), ...changes });
    server.api.getOperation = async (id) => operation(id, "succeeded", item("folder/file"));
    const harness = await mount(t, server.api);
    enqueue(harness);
    const adopted = await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
    assert.equal(adopted.hasActiveUploads, true);
    assert.equal(attempts.length, 1);
    assert.equal(callsOf(server, "status").at(-1)[1], blocker.id);
    assert.equal(callsOf(server, "abort").length, 0);
    assertNoPublication(server);
    await harness.tick();
    const queue = await harness.until(complete);
    assert.equal(queue.tasks[0].status, "completed");
    assert.equal(server.sessions.size, 1, "the blocker's publication is the one that finished");
  });
}

test("definitive non-upload blocker status retains exact Operation waiting without confirmation or abort", async (t) => {
  const { server, blocker, attempts } = await blockedUpload();
  server.api.uploadStatus = async (id) => { server.calls.push(["status", id]); throw new TestApiError("not an upload", 404); };
  const harness = await mount(t, server.api);
  enqueue(harness);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
  assert.equal(attempts.length, 1);
  assert.equal(callsOf(server, "abort").length, 0);
  assertNoPublication(server);
});

for (const error of [new TestApiError("status unavailable", 503), new Error("offline")]) {
  test(`transient blocker status failure (${error.message}) remains retryable and explicit Retry adopts the publication`, async (t) => {
    const { server, blocker, attempts } = await blockedUpload();
    const status = server.api.uploadStatus;
    let failure = error;
    server.api.uploadStatus = async (...args) => {
      if (failure) { server.calls.push(["status", args[0]]); throw failure; }
      return status(...args);
    };
    server.api.getOperation = async (id) => operation(id, "succeeded", item("folder/file"));
    const harness = await mount(t, server.api);
    enqueue(harness);
    const failed = await harness.until((queue) => queue.tasks[0]?.status === "failed");
    const id = failed.tasks[0].id, originalFile = failed.tasks[0].file;
    assert.equal(failed.tasks[0].error, error.message);
    assert.equal(failed.tasks[0].waitingFor, undefined, "a transient lookup failure cannot trap the task in Operation waiting");
    assert.ok(originalFile instanceof File);
    assert.equal(failed.hasActiveUploads, false);
    assert.equal(harness.listeners.get("beforeunload")?.size ?? 0, 0);
    assert.equal(callsOf(server, "abort").length, 0);
    assertNoPublication(server);
    await harness.tick();
    assert.equal(attempts.length, 1, "idle polling never silently retries a failed lookup");
    failure = undefined;
    failed.retry(id);
    await harness.until((queue) => queue.tasks[0]?.status === "finalizing");
    await harness.tick();
    const queue = await harness.until(complete);
    assert.equal(queue.tasks[0].id, id);
    assert.equal(attempts.length, 2, "Retry asks for the allocation again and learns who holds the path");
    assert.ok(attempts.every(([, key]) => key === attempts[0][1]), "Retry retains the original allocation key");
    for (const [intent] of attempts) assert.deepEqual(intent, attempts[0][0]);
    assert.equal(callsOf(server, "status").at(-1)[1], blocker.id);
    assert.equal(callsOf(server, "abort").length, 0, "a publication under way is adopted, never aborted");
    assertNoPublication(server);
    assert.equal(queue.hasActiveUploads, false);
  });
}

for (const terminal of ["aborted", "expired", "failed"]) {
  for (const opState of ["cancelled", "failed", "running", "outcome_unknown"]) {
    // A terminal Operation released its guards, so the path may be allocated again; only a
    // still-running or unknown outcome keeps the wait, because it may still hold them.
    const released = opState === "cancelled" || opState === "failed";
    test(`an initially ${terminal} blocker with ${opState} Operation ${released ? "permits a fresh allocation" : "keeps exact Operation waiting"}`, async (t) => {
      const { server, blocker, attempts } = await blockedUpload(terminal);
      server.api.getOperation = async (id) => { server.calls.push(["operation", id]); return operation(id, opState, null, "upload_commit"); };
      const harness = await mount(t, server.api);
      enqueue(harness);
      const queue = await harness.until(released ? complete : (queue) => queue.tasks[0]?.status === "waiting");
      assert.deepEqual(callsOf(server, "operation"), [["operation", blocker.id]]);
      assert.equal(callsOf(server, "abort").length, 0);
      if (released) assertFreshPublication(server, attempts);
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
  const harness = await mount(t, server.api);
  enqueue(harness);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "waiting");
  assert.equal(queue.tasks[0].waitingFor.operationId, blocker.id);
  assert.equal(attempts.length, 1);
  assert.equal(callsOf(server, "abort").length, 0);
  assertNoPublication(server);
});
