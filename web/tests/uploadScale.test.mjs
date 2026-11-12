import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";
import { memoryLocks, setup, uploadServer } from "./helpers/uploadHarness.mjs";

const rowSource = await readFile(new URL("../src/pages/uploadRows.ts", import.meta.url), "utf8");
const rowModule = await transformWithOxc(rowSource, "uploadRows.ts");
const { buildUploadRows } = await import(
  `data:text/javascript;base64,${Buffer.from(rowModule.code).toString("base64")}`
);

const directory = (count) => [...Array.from({ length: count }, (_, index) => ({
  path: `手表数据/day-${String(index % 365).padStart(3, "0")}/record-${index}.json`,
  file: new File(["{}"], `record-${index}.json`, { type: "application/json" }),
}))];
const allComplete = (queue) => queue.tasks.length > 0 && queue.tasks.every((task) => task.status === "completed");

// Selecting a folder happens on the main thread, before any request exists: the page must stay
// on screen while a 20k-entry selection is read, not paint a growing list 40k times. The previous
// shape looked each entry up in the whole task array and appended one task per state update, which
// held a copy of the growing array per update — the renderer ran out of memory and died with
// "Aw, Snap!" a few seconds into the selection.
test("a folder selection publishes once and stays linear in its entry count", async (t) => {
  const measure = async (count) => {
    const server = uploadServer();
    const harness = await setup(server.api);
    t.after(harness.unmount);
    const queue = harness.render();
    const before = harness.updates();
    const started = performance.now();
    queue.enqueue(directory(count), "", () => false);
    const elapsed = performance.now() - started;
    assert.equal(harness.render().tasks.length, count);
    assert.ok(harness.updates() - before <= 2, `a ${count}-entry selection published ${harness.updates() - before} list states`);
    harness.unmount();
    return elapsed;
  };
  const small = await measure(2000);
  const large = await measure(20000);
  // Linear work grows tenfold with the entries; the quadratic shape grew a hundredfold.
  assert.ok(large < Math.max(small, 1) * 25, `20000 entries took ${large.toFixed(0)} ms, 2000 took ${small.toFixed(0)} ms`);
});

test("a running folder upload paints on the queue's cadence, not once per file", async (t) => {
  const server = uploadServer();
  const harness = await setup(server.api);
  t.after(harness.unmount);
  const queue = harness.render();
  const enqueued = harness.updates();
  queue.enqueue(directory(300), "", () => false);
  await harness.until(allComplete);
  assert.equal(harness.completed.length, 300);
  // Every task changes state several times (queued, uploading, finalizing, completed). Each
  // change may schedule a paint, but a paint is one published list, and there are never more
  // publishes than paints plus the selection itself.
  assert.ok(harness.updates() - enqueued <= harness.paints() + 3,
    `${harness.updates() - enqueued} published states over ${harness.paints()} paints`);
});

test("a large selection holds a bounded page of upload sessions at once", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  let active = 0;
  let peak = 0;
  const first = [];
  server.api.createUpload = async (...args) => {
    active += 1;
    peak = Math.max(peak, active);
    if (peak === 1) first.push(args[0]);
    try {
      // The first wave stays open so the bound is measured against real concurrency,
      // not against how fast the fake server answers.
      if (active <= 8) await new Promise((resolve) => setImmediate(resolve));
      return await create(...args);
    } finally { active -= 1; }
  };
  const harness = await setup(server.api);
  t.after(harness.unmount);
  harness.render().enqueue(directory(200), "", () => false);
  await harness.until(allComplete, "every queued file eventually starts");
  // Admission runs two pages ahead of the transfer window: 64 sessions may be open, while
  // bytes stay bounded by the part pool. A selection of 200 must never open 200.
  assert.ok(peak <= 64, `${peak} upload sessions were open at once`);
  assert.ok(peak > 1, "a queue that serializes everything would be useless");
  assert.equal(first.length, 1);
  assert.equal(server.calls.filter(([kind]) => kind === "create").length, 200, "each file allocates exactly once");
});

// Cancelling a folder row names every descendant, because the row is a projection rather
// than a task of its own. Handing those ids over one at a time made the gesture quadratic:
// each id walked the whole queue to resolve its subtree and published a list state of its
// own, so stopping a large import froze the tab for as long as the import had been running.
test("cancelling a folder row is one gesture, linear in the tree it stops", async (t) => {
  const measure = async (count) => {
    const server = uploadServer();
    const harness = await setup(server.api);
    t.after(harness.unmount);
    const queue = harness.render();
    queue.enqueue(directory(count), "", () => false);
    // Exactly what the row's Cancel button hands the queue.
    const [row] = buildUploadRows([], harness.render().tasks, "");
    assert.ok(row.upload, "the selection is one folder row");
    assert.ok(row.upload.cancelIds.length >= count, "a folder row cancels every descendant");
    const published = harness.updates();
    const started = performance.now();
    queue.cancel(row.upload.cancelIds);
    const elapsed = performance.now() - started;
    assert.ok(harness.updates() - published <= 2,
      `cancelling ${count} files published ${harness.updates() - published} list states`);
    const drained = await harness.until((current) => current.tasks.length === 0, "every cancelled row is dropped");
    assert.equal(drained.tasks.length, 0);
    harness.unmount();
    return elapsed;
  };
  const small = await measure(500);
  const large = await measure(5000);
  // Ten times the tree must cost about ten times the gesture. The per-id shape cost a
  // hundred times as much (and a published state per file).
  assert.ok(large < Math.max(small, 1) * 25, `cancelling 5000 files took ${large.toFixed(0)} ms, 500 took ${small.toFixed(0)} ms`);
});

// A pump is a decision, not a notification. Cancelling a subtree changes thousands of tasks
// in one loop, and the part of the selection the loop has not reached is still `queued`:
// running a pump per cancelled task admitted a fresh page of sessions from exactly that
// remainder, so stopping an import allocated uploads it was about to discard — one page per
// cancelled file — and the server paid for every one of them.
test("cancelling a folder does not allocate sessions for the files it is cancelling", async (t) => {
  const server = uploadServer();
  const harness = await setup(server.api);
  t.after(harness.unmount);
  harness.render().enqueue(directory(1200), "", () => false);
  await harness.settle();
  const [row] = buildUploadRows([], harness.render().tasks, "");
  const before = server.calls.filter(([kind]) => kind === "create").length;
  assert.ok(before > 0, "the import started before it was cancelled");
  harness.render().cancel(row.upload.cancelIds);
  await harness.settle();
  await harness.settle();
  const after = server.calls.filter(([kind]) => kind === "create").length;
  // At most the page already in flight may land; the cancelled remainder must allocate none.
  assert.ok(after - before <= 64, `${after - before} sessions were allocated for a cancelled folder`);
});

// A bulk cancel changes thousands of tasks in one loop, and the part of the selection the
// loop has not reached is still `queued`. Running a scheduling pass after each cancelled
// task claimed the path of the next queued file and admitted it — only for the same loop to
// abort that claim a moment later. That is one Web Lock claim per cancelled file: 1,000
// requests for a 1,000-file folder, all thrown away, competing with the aborts that are
// actually finishing the gesture.
test("cancelling a folder claims no path for the files still queued behind it", async (t) => {
  const server = uploadServer();
  const locks = memoryLocks();
  const harness = await setup(server.api, { locks });
  t.after(harness.unmount);
  harness.render().enqueue(directory(1200), "", () => false);
  await harness.settle();
  const [row] = buildUploadRows([], harness.render().tasks, "");
  const claimed = locks.requests.length;
  assert.ok(claimed > 0, "the import had already claimed paths");
  harness.render().cancel(row.upload.cancelIds);
  await harness.settle();
  await harness.settle();
  assert.ok(locks.requests.length - claimed <= 64,
    `${locks.requests.length - claimed} paths were claimed while cancelling ${row.upload.cancelIds.length} files`);
});
