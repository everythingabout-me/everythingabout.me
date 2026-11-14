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
  harness.render().enqueue([entry("abcdefghijklm")], "");
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
    harness.render().enqueue([entry("abcdefghij")], "");
    await harness.until(() => active === expected);
    assert.equal(peak, expected);
    gate.resolve();
    await harness.until(complete);
    assert.equal(peak, expected);
    assert.deepEqual(parts(server).map(([, , number]) => number), [1, 2, 3, 4, 5]);
  });
}

test("class A sessions share the global part pool without application parts", async (t) => {
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
  harness.render().enqueue(["a", "b", "c", "d"].map((path) => ({ path, file: new File(["abcdefgh"], path) })), "");
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
  harness.render().enqueue([entry("ab")], "");
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
  harness.render().enqueue([entry("ab")], "");
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 250));
  assert.equal(attempts.length, 1);
  assert.ok(server.calls.some(([kind]) => kind === "status"));
  // The session was created empty in this attempt, so the only receipt read is the one
  // after the failure that proves the part never landed.
  assert.equal(server.calls.filter(([kind]) => kind === "receipts").length, 1);
  await harness.tick(250);
  await harness.until(complete);
  assert.equal(attempts.length, 2);
  assert.equal(attempts[0][0], attempts[1][0]);
  assert.equal(attempts[0][1], attempts[1][1]);
  assert.equal(attempts[0][2], attempts[1][2], "retry retains the exact immutable Blob");
  assert.equal(server.sessions.size, 1);
});

test("automatic part retries stop after five attempts and preserve the original session", async (t) => {
  const server = uploadServer();
  let attempts = 0;
  server.api.uploadPart = async () => { attempts++; throw new TestApiError("server error", 500); };
  const harness = await mount(t, server.api);
  harness.render().enqueue([entry("ab")], "");
  // Four waits for five attempts, each twice the last: a phone that lost its link for a
  // quarter of a minute still finishes the part.
  for (const delay of [250, 500, 1000, 2000]) {
    await harness.until(() => [...harness.timers.values()].some(({ delay: pending }) => pending === delay));
    await harness.tick(delay);
  }
  const queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(attempts, 5);
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
  harness.render().enqueue([entry("ab")], "");
  await harness.until(() => [...harness.timers.values()].some(({ delay }) => delay === 500));
  assert.equal(server.calls.filter(([kind]) => kind === "part").length, 0);
  // Backpressure is not an ambiguous ACK: no status or receipt query is needed.
  assert.equal(server.calls.filter(([kind]) => kind === "status").length, 0);
  assert.equal(server.calls.filter(([kind]) => kind === "receipts").length, 0, "a fresh session has nothing to read");
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
  harness.render().enqueue([entry("ab")], "");
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
  harness.render().enqueue([entry("abcdefgh")], "");
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
  harness.render().enqueue(["a", "b", "c"].map((path) => ({ path, file: new File(["x".repeat(16)], path) })), "");
  // Three files of eight parts each over one sixteen-slot pool: the first two take every
  // slot, and the third holds none although it already has an open session. That file is
  // queued for the user, whatever its task status says.
  const queue = await harness.until((queue) => queue.tasks.length === 3 && active === 16);
  assert.deepEqual(queue.tasks.map((task) => task.activeParts), [8, 8, 0]);
  assert.deepEqual(queue.tasks.map((task) => task.status), ["uploading", "uploading", "uploading"]);
  gate.resolve();
  await harness.until((queue) => queue.tasks.every((task) => task.status === "completed"));
});

test("a selection queues in the order the list shows, not the order the drop delivered", async (t) => {
  const server = uploadServer(2);
  const gate = deferred();
  const sent = [];
  const pathOf = (id) => [...server.sessions.values()].find((session) => session.id === id).path;
  const filesBehind = new Set(Array.from({ length: 8 }, (_, i) => `c${String(32 + i)}`));
  const send = server.api.uploadPart;
  server.api.uploadPart = async (id, number, blob, signal, progress) => {
    sent.push(pathOf(id));
    await gate.promise;
    return send(id, number, blob, signal, progress);
  };
  const harness = await mount(t, server.api);
  // Sixteen parts each, so one file can fill the whole six-slot pool and the order the
  // files are served in is visible without waiting for anything to finish.
  harness.render().enqueue(["b", "a"].map((path) => ({ path, file: new File(["x".repeat(32)], path) })), "");
  const queue = await harness.until((queue) => queue.tasks.some((task) => task.activeParts === 16));
  // The drop delivered b first; the list shows a first, so a holds the pool and finishes
  // first. The other order would make a row switch from uploading to waiting for a reason
  // a person cannot see.
  assert.deepEqual(queue.tasks.map((task) => task.path), ["a", "b"]);
  assert.equal(queue.tasks[0].activeParts, 16);
  assert.equal(queue.tasks[1].activeParts, 0);
  assert.deepEqual([...new Set(sent)], ["a"]);
  gate.resolve();
  await harness.until((queue) => queue.tasks.every((task) => task.status === "completed"));
});

// Admission and settlement are stages with their own bounds: the transfer window must be
// filled with files that already hold a session, and a file whose bytes are placed must
// stop occupying a slot while it settles. These three assert the boundary itself, because
// blurring it is what made the queue's throughput follow its slowest control request.
const named = (path, bytes = "ab") => ({ path, file: new File([bytes], path) });

test("the transfer stage claims its window while the look-ahead prepares sessions behind it", async (t) => {
  const server = uploadServer(2);
  const gate = deferred();
  const send = server.api.uploadPart;
  server.api.uploadPart = async (...args) => { await gate.promise; return send(...args); };
  const harness = await mount(t, server.api);
  const files = Array.from({ length: 100 }, (_, i) => named(`f${String(i).padStart(3, "0")}`));
  harness.render().enqueue(files, "");
  // Sixty-four files belong to the transfer stage: a claim is what gives a queued row an
  // owner, and it deliberately does not wait for its session — that wait used to consume a
  // slot and left the part pool with nothing to hand out. The sessions are prepared behind
  // them, which is what a freed slot is filled from.
  await harness.until((queue) => queue.tasks.filter((task) => task.status === "uploading").length === 64 && server.sessions.size > 32);
  assert.equal(new Set(server.calls.filter(([kind]) => kind === "create").map(([, , key]) => key)).size, server.sessions.size, "each prepared file is admitted exactly once");
  gate.resolve();
  await harness.until((queue) => queue.tasks.every((task) => task.status === "completed"));
});

test("a settling file holds no transfer slot, so the window keeps moving while it publishes", async (t) => {
  const server = uploadServer(2);
  const commitGate = deferred();
  const partGate = deferred();
  const sendCommit = server.api.commitUpload;
  server.api.commitUpload = async (id) => { await commitGate.promise; return sendCommit(id); };
  const pathOf = (id) => [...server.sessions.values()].find((session) => session.id === id).path;
  const filesBehind = new Set(Array.from({ length: 8 }, (_, i) => `c${String(32 + i)}`));
  // The two files behind the window keep their parts open, so the window's rotation is
  // visible: the twelve ahead of them are waiting for publication, not for a slot.
  const behind = new Set(filesBehind);
  const sendPart = server.api.uploadPart;
  server.api.uploadPart = async (id, number, blob, signal, progress) => {
    if (behind.has(pathOf(id))) await partGate.promise;
    return sendPart(id, number, blob, signal, progress);
  };
  const harness = await mount(t, server.api);
  const files = Array.from({ length: 40 }, (_, i) => named(`c${String(i).padStart(2, "0")}`));
  harness.render().enqueue(files, "");
  const queue = await harness.until((queue) =>
    queue.tasks.filter((task) => task.status === "finalizing").length === 32
    && queue.tasks.filter((task) => task.status === "uploading").length === 8);
  assert.deepEqual(queue.tasks.filter((task) => task.status === "finalizing").map((task) => task.path),
    files.slice(0, 32).map((file) => file.path));
  assert.deepEqual(queue.tasks.filter((task) => task.status === "uploading").map((task) => task.path),
    files.slice(32).map((file) => file.path));
  commitGate.resolve();
  partGate.resolve();
  await harness.until((queue) => queue.tasks.every((task) => task.status === "completed"));
});

// The old window counted every started row, so sixty-four rows waiting for publication
// stopped the queue from starting anything: a wave of parts, a wait for a whole settle page,
// then the next wave, with the part pool idle in between. A settlement is a commitment, not a
// transfer, and the depth of the one must not bound the other.
test("a full settle window still lets the files behind it move their bytes", async (t) => {
  const server = uploadServer(2);
  const commitGate = deferred();
  const sendCommit = server.api.commitUpload;
  server.api.commitUpload = async (id) => { await commitGate.promise; return sendCommit(id); };
  const pathOf = (id) => [...server.sessions.values()].find((session) => session.id === id).path;
  const harness = await mount(t, server.api);
  const files = Array.from({ length: 200 }, (_, i) => named(`s${String(i).padStart(3, "0")}`));
  harness.render().enqueue(files, "");
  const finalizing = (queue) => queue.tasks.filter((task) => task.status === "finalizing");
  // Sixty-four publications are outstanding — the settle window in full — and the queue must
  // not read that as "the transfer stage is busy".
  let queue = await harness.until((queue) => finalizing(queue).length >= 64);
  const published = new Set(finalizing(queue).map((task) => task.path));
  const started = queue.tasks.filter((task) => task.status !== "queued");
  assert.ok(started.length > 64, `only ${started.length} files started while 64 publications were waiting`);
  // And the part pool keeps working: bytes arrive for a file that is not one of the rows
  // already waiting to publish.
  const partsBefore = server.calls.filter(([kind]) => kind === "part").length;
  await harness.until(() => server.calls.filter(([kind]) => kind === "part").length > partsBefore);
  const sending = server.calls.filter(([kind]) => kind === "part").slice(partsBefore).map(([, id]) => pathOf(id));
  assert.ok(sending.some((path) => !published.has(path)), "a file behind the settle window sent its bytes");
  commitGate.resolve();
  await harness.until((queue) => queue.tasks.every((task) => task.status === "completed"));
  assert.equal(server.sessions.size, files.length);
});

test("cancelling a prepared row aborts its session instead of leaving it open", async (t) => {
  const server = uploadServer(2);
  const gate = deferred();
  const send = server.api.uploadPart;
  server.api.uploadPart = async (...args) => { await gate.promise; return send(...args); };
  const harness = await mount(t, server.api);
  const files = Array.from({ length: 100 }, (_, i) => named(`p${String(i).padStart(3, "0")}`));
  harness.render().enqueue(files, "");
  const queue = await harness.until((queue) => server.sessions.size > 32
    && queue.tasks.some((task) => task.status === "queued" && task.uploadId));
  const prepared = queue.tasks.find((task) => task.status === "queued" && task.uploadId);
  assert.ok(prepared && prepared.uploadId, "a row behind the window holds a prepared session");
  queue.cancel(prepared.id);
  await harness.until((queue) => !queue.tasks.some((task) => task.id === prepared.id));
  assert.deepEqual(server.calls.filter(([kind]) => kind === "abort"), [["abort", prepared.uploadId]], "the prepared session is aborted");
  assert.equal(server.calls.filter(([kind, input]) => kind === "create" && input.path === prepared.path).length, 1, "the prepared session is never reallocated");
  gate.resolve();
});

test("the look-ahead never reserves more than a gigabyte of quota", async (t) => {
  // A prepared session reserves its bytes against the owner's quota the moment it is
  // admitted, so depth is bounded by bytes as well as by count. With 100 MB files the byte
  // bound holds ten of them (1 GB); forty files would reserve 4 GB for a minute before their
  // turn. What the window is actually moving is not part of that reserve: those sessions are
  // consumed the moment their file starts, so the bound is on speculation, not on work.
  const server = uploadServer(16 << 20);
  const gate = deferred();
  const send = server.api.uploadPart;
  server.api.uploadPart = async (...args) => { await gate.promise; return send(...args); };
  const harness = await mount(t, server.api);
  // More files than the transfer stage claims, so some of them stay behind it and are the
  // look-ahead this bound is about. A claimed file takes its session at once and is then the
  // work in progress, not speculation; what must stay under a gigabyte is what is admitted
  // for files nobody has started yet.
  const files = Array.from({ length: 200 }, (_, i) => named(`big-${String(i).padStart(2, "0")}`, "x".repeat(100_000_000)));
  harness.render().enqueue(files, "");
  const queue = await harness.until((queue) => queue.tasks.some((task) => task.status === "queued" && task.uploadId));
  const held = queue.tasks.filter((task) => task.status === "queued" && task.uploadId);
  const reserved = held.reduce((sum, task) => sum + task.size, 0);
  assert.ok(reserved > 0, "sessions are admitted for the files that are waiting");
  assert.ok(reserved <= 1_000_000_000, `the look-ahead reserved ${reserved} bytes of quota`);
  gate.resolve();
});
