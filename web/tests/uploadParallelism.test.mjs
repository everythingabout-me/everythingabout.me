import assert from "node:assert/strict";
import test from "node:test";
import { deferred, digest, setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

const entry = (bytes) => ({ path: "file", file: new File([bytes], "file") });
const complete = (queue) => queue.tasks[0]?.status === "completed";
async function mount(t, api) { const harness = await setup(api); t.after(harness.unmount); return harness; }
const parts = (server) => server.calls.filter(([kind]) => kind === "part");

test("combined progress includes every active part exactly once as ACKs arrive out of order", async (t) => {
  const server = uploadServer(4);
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
  // Samples land once a second rather than on every byte event, so in-flight bytes
  // are summed at flush time: each active part counts once, and the value appears on
  // the next tick.
  assert.equal(queue.tasks[0].loaded, 0);
  queue = await harness.tick(1000);
  assert.equal(queue.tasks[0].loaded, 7);
  // The first sample has nothing to measure against, so the speed appears with the
  // second one.
  assert.equal(queue.tasks[0].speed, 0);
  pending.get(2).gate.resolve();
  await harness.settle();
  // The ACK moves the part's bytes from in-flight to durable, so the total is the
  // part's full size even though its last sample was smaller.
  queue = await harness.tick(1000);
  assert.equal(queue.tasks[0].loaded, 10);
  assert.ok(queue.tasks[0].speed > 0);
  pending.get(2).progress({ loaded: 100 });
  // A transport must not emit progress after its ACK; remaining requests are
  // clamped individually, so oversized in-flight samples cannot exceed the file.
  pending.get(1).progress({ loaded: 100 });
  pending.get(3).progress({ loaded: 100 });
  queue = await harness.tick(1000);
  assert.ok(queue.tasks[0].loaded <= 13);
  for (const { gate } of pending.values()) gate.resolve();
  await harness.until(complete);
  assert.equal(harness.render().tasks[0].loaded, 13);
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 1);
});

for (const [ordering, expected] of [["any", 5], ["ascending", 1]]) {
  test(`${ordering} ordering sends ${expected === 1 ? "one part at a time" : "every missing part at once"}`, async (t) => {
    const server = uploadServer();
    const configure = (session) => ({ ...session, ordering,
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

test("class A sessions share the global eight-part pool without application parts", async (t) => {
  const server = uploadServer(4, { class: "A" });
  const gate = deferred();
  let active = 0, peak = 0;
  const sent = [];
  server.api.uploadPresignedPart = async (grant, blob, signal, progress) => {
    sent.push(grant.number);
    active++; peak = Math.max(peak, active);
    try {
      await gate.promise;
      const id = new URL(grant.url, "https://upload.invalid").searchParams.get("uploadId");
      server.receipts.get(id).set(grant.number, { number: grant.number, size: blob.size });
      progress?.({ loaded: blob.size, total: blob.size });
    } finally { active--; }
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue(["a", "b", "c", "d"].map((path) => ({ path, file: new File(["abcdefgh"], path) })), "", () => false);
  await harness.until(() => active === 8);
  await harness.tick();
  assert.equal(peak, 8, "all class A parts across four files run together");
  gate.resolve();
  await harness.until((queue) => queue.tasks.length === 4 && queue.tasks.every((task) => task.status === "completed"));
  assert.equal(peak, 8);
  assert.equal(sent.length, 8);
  assert.equal(server.calls.filter(([kind]) => kind === "part").length, 0, "class A never sends bytes to the application");
  assert.ok(server.calls.some(([kind]) => kind === "sign"), "missing grants are signed by the application");
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 4);
});

test("class A transient part failure checks provider state then retries the same part", async (t) => {
  const server = uploadServer(2, { class: "A" });
  const send = server.api.uploadPresignedPart;
  const attempts = [];
  server.api.uploadPresignedPart = async (...args) => {
    attempts.push(args);
    if (attempts.length === 1) throw new TestApiError("temporarily unavailable", 503);
    return send(...args);
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "", () => false);
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 250));
  assert.equal(attempts.length, 1);
  assert.ok(server.calls.some(([kind]) => kind === "status"));
  assert.ok(server.calls.some(([kind]) => kind === "providerParts"));
  await harness.tick(250);
  await harness.until(complete);
  assert.equal(attempts.length, 2);
  assert.equal(attempts[0][0].number, attempts[1][0].number);
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
  server.api.uploadPart = async () => { attempts++; throw new TestApiError("server error", 500); };
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

test("receiver backpressure waits without spending a part attempt or failing the file", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  let rejected = 0;
  server.api.uploadPart = async (...args) => {
    // More rejections than PART_UPLOAD_ATTEMPTS: a busy receiver must not exhaust them.
    if (++rejected <= 4) throw new TestApiError("receiver busy", 429, { code: "rate_limited" });
    return send(...args);
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "", () => false);
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 500));
  assert.equal(server.calls.filter(([kind]) => kind === "part").length, 0);
  // Backpressure is not an ambiguous ACK: no status or receipt query is needed.
  assert.equal(server.calls.filter(([kind]) => kind === "status").length, 0);
  assert.equal(server.calls.filter(([kind]) => kind === "receipts").length, 1, "only the initial receipt read");
  for (const delay of [500, 1000, 2000, 2000]) {
    await harness.until(() => [...harness.timers.values()].some(({ delay: pending }) => pending === delay));
    await harness.tick(delay);
  }
  const queue = await harness.until(complete);
  assert.equal(queue.tasks[0].status, "completed");
  assert.equal(rejected, 5);
  assert.equal(server.calls.filter(([kind]) => kind === "part").length, 1, "the part transfers exactly once");
  assert.equal(server.calls.filter(([kind]) => kind === "commit").length, 1);
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

test("a started file holds no part slot until the earlier file releases one", async (t) => {
  const server = uploadServer();
  const send = server.api.uploadPart;
  const gate = deferred();
  let active = 0;
  server.api.uploadPart = async (...args) => {
    active++;
    try { await gate.promise; return await send(...args); } finally { active--; }
  };
  const harness = await mount(t, server.api);
  harness.render().enqueue(["a", "b", "c"].map((path) => ({ path, file: new File(["abcdefgh"], path) })), "", () => false);
  // Three files of four parts each over one eight-slot pool: the first two take every
  // slot, and the third holds none although it already has an open session and its
  // receipts read. That file is queued for the user, whatever its task status says.
  const queue = await harness.until((queue) => queue.tasks.length === 3 && active === 8
    && server.calls.filter(([kind]) => kind === "receipts").length === 3);
  assert.deepEqual(queue.tasks.map((task) => task.activeParts), [4, 4, 0]);
  assert.deepEqual(queue.tasks.map((task) => task.status), ["uploading", "uploading", "uploading"]);
  gate.resolve();
  await harness.until((queue) => queue.tasks.every((task) => task.status === "completed"));
});
