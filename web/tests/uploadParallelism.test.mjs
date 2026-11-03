import assert from "node:assert/strict";
import test from "node:test";
import { deferred, digest, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const entry = (bytes) => ({ path: "file", file: new File([bytes], "file") });
const complete = (queue) => queue.tasks[0]?.status === "completed";
async function mount(t, api) { const harness = await setup(api); t.after(harness.unmount); return harness; }
const parts = (server) => server.calls.filter(([kind]) => kind === "part");

test("combined progress includes every active part exactly once as ACKs arrive out of order", async (t) => {
  const server = uploadServer(4);
  const create = server.api.createUpload;
  server.api.createUpload = async (...args) => ({ ...await create(...args), maxParallelism: 4 });
  const send = server.api.uploadPart;
  const pending = new Map();
  server.api.uploadPart = async (...args) => {
    const gate = deferred();
    pending.set(args[1], { gate, progress: args[4] });
    await gate.promise;
    return send(...args);
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("abcdefghijklm")], "", () => false);
  await harness.until(() => pending.size === 4);
  pending.get(1).progress({ loaded: 2 });
  pending.get(2).progress({ loaded: 1 });
  pending.get(3).progress({ loaded: 3 });
  pending.get(4).progress({ loaded: 1 });
  let queue = await harness.settle();
  assert.equal(queue.tasks[0].loaded, 7);
  assert.ok(queue.tasks[0].speed > 0);
  pending.get(2).gate.resolve();
  await harness.until((queue) => queue.tasks[0].loaded === 10);
  pending.get(2).progress({ loaded: 100 });
  // A transport must not emit progress after its ACK; remaining requests are
  // clamped individually, so oversized in-flight samples cannot exceed the file.
  pending.get(1).progress({ loaded: 100 });
  pending.get(3).progress({ loaded: 100 });
  queue = await harness.settle();
  assert.ok(queue.tasks[0].loaded <= 13);
  for (const { gate } of pending.values()) gate.resolve();
  await harness.until(complete);
  assert.equal(harness.render().tasks[0].loaded, 13);
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 1);
});

for (const [ordering, maxParallelism, expected] of [["any", 2, 2], ["any", 1, 1], ["ascending", 4, 1]]) {
  test(`${ordering} ordering honors the session part limit ${maxParallelism}`, async (t) => {
    const server = uploadServer();
    const configure = (session) => ({ ...session, ordering, maxParallelism,
      nextPart: ordering === "ascending" ? server.receipts.get(session.id).size + 1 : null });
    const create = server.api.createUpload;
    server.api.createUpload = async (...args) => configure(await create(...args));
    server.api.uploadStatus = async (id) => configure(server.sessions.get(id));
    const send = server.api.uploadPart;
    const gate = deferred();
    let active = 0, peak = 0;
    server.api.uploadPart = async (...args) => {
      active++; peak = Math.max(peak, active);
      try { await gate.promise; return await send(...args); } finally { active--; }
    };
    const harness = await mount(t, server.api);
    harness.render().enqueue([entry("abcdefghij")], "", () => false);
    await harness.until(() => active === expected);
    assert.equal(peak, expected);
    gate.resolve();
    await harness.until(complete);
    assert.equal(peak, expected);
    assert.deepEqual(parts(server).map(([, , number]) => number), [1, 2, 3, 4, 5]);
  });
}

test("signed sessions retain four-file by four-part concurrency and pass transport on every request", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  server.api.createUpload = async (...args) => {
    const session = await create(...args);
    Object.assign(server.sessions.get(session.id), { transport: "cos-signed", maxParallelism: 4 });
    return { ...server.sessions.get(session.id) };
  };
  const send = server.api.uploadPart;
  const gate = deferred();
  let active = 0, peak = 0;
  const modes = [];
  server.api.uploadPart = async (...args) => {
    modes.push(args[5]);
    active++; peak = Math.max(peak, active);
    try { await gate.promise; return await send(...args); } finally { active--; }
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue(["a", "b", "c", "d"].map((path) => ({ path, file: new File(["abcdefgh"], path) })), "", () => false);
  await harness.until(() => active === 16);
  assert.equal(peak, 16);
  assert.deepEqual(modes, Array(16).fill("cos-signed"));
  gate.resolve();
  await harness.until((queue) => queue.tasks.length === 4 && queue.tasks.every((task) => task.status === "completed"));
});

test("signed part_pending confirmation retries identical bytes within the original session", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  server.api.createUpload = async (...args) => {
    const session = await create(...args);
    server.sessions.get(session.id).transport = "cos-signed";
    return { ...server.sessions.get(session.id) };
  };
  const send = server.api.uploadPart;
  const attempts = [];
  server.api.uploadPart = async (...args) => {
    attempts.push(args);
    if (attempts.length === 1) throw new TestApiError("not ready", 409, { code: "part_pending" });
    return send(...args);
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "", () => false);
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 250));
  await harness.tick(250);
  await harness.until(complete);
  assert.equal(attempts.length, 2);
  assert.equal(attempts[0][2], attempts[1][2]);
  assert.equal(attempts[1][5], "cos-signed");
  assert.equal(server.sessions.size, 1);
});

test("a transient failure checks receipts then retries the same part with bounded backoff", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  const attempts = [];
  server.api.uploadPart = async (...args) => {
    attempts.push(args);
    if (attempts.length === 1) throw new TestApiError("temporarily unavailable", 503);
    return send(...args);
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "", () => false);
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 250));
  assert.equal(attempts.length, 1);
  assert.ok(server.calls.some(([kind]) => kind === "status"));
  assert.equal(server.calls.filter(([kind]) => kind === "receipts").length, 2);
  await harness.tick(250);
  await harness.until(complete);
  assert.equal(attempts.length, 2);
  assert.equal(attempts[0][0], attempts[1][0]);
  assert.equal(attempts[0][1], attempts[1][1]);
  assert.equal(attempts[0][2], attempts[1][2], "retry retains the exact immutable Blob");
  assert.equal(server.sessions.size, 1);
});

test("automatic part retries stop after three attempts and preserve the original session", async (t) => {
  const server = uploadServer();
  let attempts = 0;
  server.api.uploadPart = async () => { attempts++; throw new TestApiError("busy", 429); };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "", () => false);
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 250));
  await harness.tick(250);
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 500));
  await harness.tick(500);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(attempts, 3);
  assert.equal(queue.tasks[0].uploadId, "session-1");
  assert.equal(server.sessions.size, 1);
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 0);
  assert.equal([...harness.timers.values()].filter(({ delay }) => delay < 1000).length, 0);
});

test("a conflicting receipt after an ambiguous ACK never resends or commits", async (t) => {
  const server = uploadServer();
  let attempts = 0;
  server.api.uploadPart = async (id, number) => {
    attempts++;
    server.receipts.get(id).set(number, { number, size: 2, sha256: digest("xx") });
    throw new TestApiError("response lost");
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "", () => false);
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(queue.tasks[0].error, "drive.reselectedFileChanged");
  assert.equal(attempts, 1);
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 0);
});

test("cancel aborts every parallel request and a pending retry delay", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  server.api.createUpload = async (...args) => ({ ...await create(...args), maxParallelism: 4 });
  const signals = [];
  let attempts = 0;
  server.api.uploadPart = (id, number, blob, signal) => {
    attempts++; signals.push(signal);
    if (number === 1) return Promise.reject(new TestApiError("offline"));
    return new Promise((resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true }));
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("abcdefgh")], "", () => false);
  await harness.until(() => signals.length === 4 && [...harness.timers.values()].some(({ delay }) => delay === 250));
  harness.render().cancel(harness.render().tasks[0].id);
  await harness.until((queue) => queue.tasks.length === 0);
  assert.ok(signals.every((signal) => signal.aborted));
  await harness.tick(250);
  assert.equal(attempts, 4);
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 0);
  assert.equal([...harness.timers.values()].filter(({ delay }) => delay < 1000).length, 0);
});
