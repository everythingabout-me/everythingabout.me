import assert from "node:assert/strict";
import test from "node:test";
import { setup, uploadServer } from "./helpers/uploadHarness.mjs";

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
