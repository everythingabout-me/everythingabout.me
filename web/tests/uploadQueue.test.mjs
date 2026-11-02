import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = (await readFile(new URL("../src/pages/useUploadQueue.ts", import.meta.url), "utf8"))
  .replace('import { useCallback, useEffect, useRef, useState } from "react";', "const { useCallback, useEffect, useRef, useState, setTimeout, clearTimeout } = globalThis.__uploadHook;")
  .replace('import { ApiError, api, type Item } from "../api/client";', "const { ApiError, api } = globalThis.__uploadHook;")
  .replace('import { t } from "../i18n";', 'const t = (key) => key;');
const { code } = await transformWithOxc(source, "useUploadQueue.ts");

class TestApiError extends Error {
  constructor(message, status, metadata = {}) { super(message); this.status = status; Object.assign(this, metadata); }
}

async function setup(api) {
  const slots = [];
  const timers = new Map();
  const cleanups = [];
  let timerId = 0;
  let cursor = 0;
  globalThis.__uploadHook = {
    api,
    setTimeout: (callback, delay) => {
      assert.ok(delay === 3000 || delay === 15000);
      timers.set(++timerId, { callback, delay });
      return timerId;
    },
    clearTimeout: (id) => timers.delete(id),
    useEffect: (effect) => {
      const index = cursor++;
      if (!(index in slots)) { slots[index] = true; cleanups.push(effect()); }
    },
    ApiError: TestApiError,
    useCallback: (callback) => callback,
    useRef: (initial) => {
      const index = cursor++;
      return slots[index] ??= { current: initial };
    },
    useState: (initial) => {
      const index = cursor++;
      if (!(index in slots)) slots[index] = initial;
      return [slots[index], (value) => { slots[index] = typeof value === "function" ? value(slots[index]) : value; }];
    },
  };
  const { useUploadQueue } = await import(`data:text/javascript;base64,${Buffer.from(code + `\n// ${Math.random()}`).toString("base64")}`);
  const completed = [];
  const render = () => { cursor = 0; return useUploadQueue((task, item) => completed.push({ task, item })); };
  const settle = async () => {
    for (let i = 0; i < 20; i++) await new Promise((resolve) => setImmediate(resolve));
    return render();
  };
  const tick = (delay = 3000) => {
    const pending = [...timers].filter(([, timer]) => timer.delay === delay);
    for (const [id, { callback }] of pending) { timers.delete(id); void callback(); }
    return settle();
  };
  return { render, settle, completed, tick, timers, unmount: () => cleanups.forEach((cleanup) => cleanup?.()) };
}

test("retry checks an uncertain receipt and never sends the same body twice", async () => {
  const calls = [];
  let result = "unknown";
  const node = { path: "Health/file.json", size: 3 };
  const api = {
    signUpload: async (...args) => { calls.push(["sign", ...args]); return { uploadId: "receipt-1" }; },
    uploadFile: async (id, file, onProgress, onSent) => {
      calls.push(["body", id, file]);
      onProgress({ loaded: file.size, total: file.size });
      onSent();
      throw new TestApiError("response lost", 0);
    },
    confirmUpload: async (id) => {
      calls.push(["confirm", id]);
      if (result === "unknown") throw new TestApiError("publication in progress", 409);
      return node;
    },
  };
  const harness = await setup(api);
  const file = new File(["abc"], "file.json", { type: "application/json", lastModified: 1443268800000 });
  harness.render().enqueue([{ path: "file.json", file }], "Health", () => false);
  let queue = await harness.settle();
  const taskId = queue.tasks[0].id;
  assert.equal(queue.tasks[0].status, "failed");
  assert.equal(queue.tasks[0].uploadId, "receipt-1");
  assert.equal(queue.tasks[0].uploaded, true);
  queue.retry(taskId);
  queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "failed");
  assert.equal(queue.tasks[0].uploadId, "receipt-1");
  assert.equal(calls.filter(([kind]) => kind === "body").length, 1);
  result = "complete";
  queue.retry(taskId);
  queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "completed");
  assert.equal(queue.tasks[0].file, null);
  assert.equal(queue.tasks[0].isDir, false);
  assert.equal(harness.completed.length, 1);
  assert.equal(calls.filter(([kind]) => kind === "sign").length, 1);
  assert.equal(calls.filter(([kind]) => kind === "body").length, 1);
  assert.deepEqual(calls[0].slice(1), ["Health/file.json", 3, false, "application/json", 1443268800000]);
});

test("only a definitive expired receipt permits a fresh transfer", async () => {
  const calls = [];
  let allocated = 0;
  const api = {
    signUpload: async () => ({ uploadId: `receipt-${++allocated}` }),
    uploadFile: async (id, file, onProgress, onSent) => {
      calls.push(["body", id]);
      if (id === "receipt-1") throw new TestApiError("receive interrupted", 0);
      onSent();
      return { path: "file", size: file.size };
    },
    confirmUpload: async (id) => { calls.push(["confirm", id]); throw new TestApiError("aborted", 410); },
  };
  const harness = await setup(api);
  harness.render().enqueue([{ path: "file", file: new File(["abc"], "file") }], "", () => false);
  let queue = await harness.settle();
  assert.equal(queue.tasks[0].uploaded, false);
  queue.retry(queue.tasks[0].id);
  queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "completed");
  assert.deepEqual(calls, [["body", "receipt-1"], ["confirm", "receipt-1"], ["body", "receipt-2"]]);
});

function deletionConflict(operationId = "delete-1", path = "folder", deletionStatus = "running") {
  return new TestApiError("deletion in progress", 409, { code: "deletion_in_progress", operationId, path, deletionStatus });
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

const rowsSource = await readFile(new URL("../src/pages/uploadRows.ts", import.meta.url), "utf8");
const rowsCode = (await transformWithOxc(rowsSource, "uploadRows.ts")).code;
const { buildUploadRows } = await import(`data:text/javascript;base64,${Buffer.from(rowsCode).toString("base64")}`);

test("shared exact job polling releases slots and only done automatically resumes retained Files", async () => {
  const calls = [];
  let status = "pending";
  const api = {
    signUpload: async (path) => {
      calls.push(["sign", path]);
      if (path !== "unrelated" && status !== "done") throw deletionConflict();
      return { uploadId: path };
    },
    uploadFile: async (id, file) => { calls.push(["body", id, file]); return { path: id }; },
    deletionStatus: async (id) => { calls.push(["status", id]); return { job: { id, status } }; },
    list: async () => assert.fail("deletion conflicts must not enter overwrite lookup"),
  };
  const harness = await setup(api);
  const first = new File(["a"], "a");
  const second = new File(["b"], "b");
  harness.render().enqueue([
    { path: "folder/a", file: first }, { path: "folder/b", file: second },
    { path: "unrelated", file: new File(["c"], "c") },
  ], "", () => false);
  let queue = await harness.settle();
  assert.deepEqual(queue.tasks.map((task) => task.status), ["waiting", "waiting", "completed"]);
  assert.equal(queue.tasks[0].file, first);
  assert.equal(queue.tasks[0].speed, 0);
  assert.equal(harness.timers.size, 1);
  assert.equal(buildUploadRows([], queue.tasks, "")[0].upload.status, "waiting");
  assert.equal(buildUploadRows([], queue.tasks, "")[0].upload.waitMessage, "drive.waitingForDeletion");
  queue = await harness.tick();
  assert.deepEqual(calls.filter(([kind]) => kind === "status"), [["status", "delete-1"]]);
  assert.equal(calls.filter(([kind]) => kind === "body").length, 1);
  status = "running";
  await harness.tick();
  assert.equal(calls.filter(([kind]) => kind === "body").length, 1);
  status = "done";
  queue = await harness.tick();
  assert.ok(queue.tasks.every((task) => task.status === "completed"));
  assert.deepEqual(calls.filter(([kind]) => kind === "body").map(([, id]) => id), ["unrelated", "folder/a", "folder/b"]);
  assert.equal(calls.find(([kind, id]) => kind === "body" && id === "folder/a")[2], first);
  assert.equal(harness.timers.size, 0);
});

test("failed, cancelled, missing, wrong and unreachable status responses never permit file bytes", async () => {
  let response = { job: { id: "delete-1", status: "failed" } };
  let bodies = 0;
  let signs = 0;
  const harness = await setup({
    signUpload: async () => { signs++; throw deletionConflict("delete-1", "folder", "failed"); },
    uploadFile: async () => { bodies++; },
    deletionStatus: async () => { if (response instanceof Error) throw response; return response; },
  });
  harness.render().enqueue([{ path: "folder/file", file: new File(["a"], "file") }], "", () => false);
  let queue = await harness.settle();
  assert.equal(queue.tasks[0].waitMessage, "drive.deletionNeedsAttention");
  for (const [value, message] of [
    [response, "drive.deletionNeedsAttention"],
    [{ job: { id: "delete-1", status: "cancelled" } }, "drive.deletionNeedsAttention"],
    [new TestApiError("not found", 404), "drive.deletionStatusUnavailable"],
    [new Error("offline"), "drive.deletionStatusUnavailable"],
    [{}, "drive.deletionStatusUnavailable"],
    [{ job: { id: "other", status: "done" } }, "drive.deletionStatusUnavailable"],
  ]) {
    response = value;
    queue = await harness.tick();
    assert.equal(queue.tasks[0].status, "waiting");
    assert.equal(queue.tasks[0].waitMessage, message);
    assert.equal(queue.tasks[0].speed, 0);
    assert.equal(harness.timers.size, 1);
    assert.equal(bodies, 0);
    assert.equal(signs, 1);
  }
  queue.cancel(queue.tasks[0].id);
  assert.equal(harness.timers.size, 0);
});

test("directory deletion is detected before merge lookup and children wait until mkdir finishes", async () => {
  let done = false;
  const mkdirGate = deferred();
  const calls = [];
  const harness = await setup({
    mkdir: async (path) => {
      calls.push(["mkdir", path]);
      if (!done) throw deletionConflict("delete-folder", "root");
      if (path === "root") await mkdirGate.promise;
    },
    list: async (path) => { calls.push(["list", path]); return { node: { path, is_dir: true } }; },
    signUpload: async (path) => { calls.push(["sign", path]); return { uploadId: path }; },
    uploadFile: async (id) => { calls.push(["body", id]); return { path: id }; },
    deletionStatus: async (id) => ({ job: { id, status: done ? "done" : "running" } }),
  });
  harness.render().enqueue([
    { path: "root", file: null }, { path: "root/nested", file: null },
    { path: "root/nested/file", file: new File(["a"], "file") },
    { path: "rootish/file", file: new File(["b"], "file") },
  ], "", () => false);
  let queue = await harness.settle();
  assert.deepEqual(queue.tasks.map((task) => task.status), ["waiting", "waiting", "waiting", "completed"]);
  assert.equal(queue.tasks[2].waitingFor.ancestorId, queue.tasks[0].id);
  assert.equal(buildUploadRows([], queue.tasks, "root/nested")[0].upload.status, "waiting");
  assert.equal(buildUploadRows([], queue.tasks, "root")[0].upload.waitMessage, "drive.waitingForDeletion");
  assert.ok(!calls.some(([kind]) => kind === "list"));
  done = true;
  queue = await harness.tick();
  assert.deepEqual(queue.tasks.map((task) => task.status), ["uploading", "queued", "queued", "completed"]);
  assert.ok(!calls.some(([kind, path]) => kind === "sign" && path.startsWith("root/")));
  mkdirGate.resolve();
  queue = await harness.settle();
  assert.ok(queue.tasks.every((task) => task.status === "completed"));
});

test("cancel waiting folder removes descendants and late poll cannot revive a replacement at the same path", async () => {
  const status = deferred();
  let polls = 0;
  let bodies = 0;
  const harness = await setup({
    mkdir: async () => { throw deletionConflict(); },
    signUpload: async () => assert.fail("descendants must remain dependency blocked"),
    uploadFile: async () => { bodies++; },
    deletionStatus: async () => { polls++; return status.promise; },
  });
  const entries = [{ path: "folder", file: null }, { path: "folder/file", file: new File(["a"], "file") }];
  harness.render().enqueue(entries, "", () => false);
  let queue = await harness.settle();
  const oldId = queue.tasks[0].id;
  await harness.tick();
  assert.equal(polls, 1);
  queue.cancel(oldId);
  assert.deepEqual(harness.render().tasks, []);
  harness.render().enqueue(entries, "", () => false);
  queue = await harness.settle();
  assert.notEqual(queue.tasks[0].id, oldId);
  status.resolve({ job: { id: "delete-1", status: "done" } });
  queue = await harness.settle();
  assert.ok(queue.tasks.every((task) => task.status === "waiting"));
  assert.equal(bodies, 0);
  assert.equal(harness.timers.size, 1);
  queue.cancel(queue.tasks[0].id);
  assert.equal(harness.timers.size, 0);
});

test("unmount clears scheduled polling and ignores status requests already in flight", async () => {
  for (const inFlight of [false, true]) {
    const status = deferred();
    let signs = 0;
    const harness = await setup({
      signUpload: async () => { signs++; throw deletionConflict(); },
      deletionStatus: async () => status.promise,
    });
    harness.render().enqueue([{ path: "file", file: new File(["a"], "file") }], "", () => false);
    await harness.settle();
    if (inFlight) await harness.tick();
    harness.unmount();
    assert.equal(harness.timers.size, 0);
    status.resolve({ job: { id: "delete-1", status: "done" } });
    await harness.settle();
    assert.equal(signs, 1);
    assert.equal(harness.timers.size, 0);
    assert.equal(harness.completed.length, 0);
  }
});

test("deletion waiting preserves uncertain receipt and resumes confirmation without resending bytes", async () => {
  let bodies = 0;
  let confirmations = 0;
  const file = new File(["abc"], "file");
  const harness = await setup({
    signUpload: async () => ({ uploadId: "saved-receipt" }),
    uploadFile: async (id, body, onProgress, onSent) => {
      bodies++;
      onSent();
      throw deletionConflict();
    },
    deletionStatus: async (id) => ({ job: { id, status: "done" } }),
    confirmUpload: async (id) => { confirmations++; assert.equal(id, "saved-receipt"); return { path: "file" }; },
  });
  harness.render().enqueue([{ path: "file", file }], "", () => false);
  let queue = await harness.settle();
  assert.equal(queue.tasks[0].status, "waiting");
  assert.equal(queue.tasks[0].file, file);
  assert.equal(queue.tasks[0].uploadId, "saved-receipt");
  assert.equal(queue.tasks[0].uploaded, true);
  queue = await harness.tick();
  assert.equal(queue.tasks[0].status, "completed");
  assert.equal(bodies, 1);
  assert.equal(confirmations, 1);
});

test("ordinary directory and file conflicts retain existing merge and overwrite behavior", async () => {
  const previousWindow = globalThis.window;
  let prompts = 0;
  globalThis.window = { confirm: () => { prompts++; return true; } };
  const signed = [];
  try {
    const harness = await setup({
      mkdir: async () => { throw new TestApiError("already exists", 409); },
      list: async (path) => ({ node: { path, is_dir: path === "folder" } }),
      signUpload: async (path, size, overwrite) => {
        signed.push(overwrite);
        if (!overwrite) throw new TestApiError("already exists", 409);
        return { uploadId: "id" };
      },
      uploadFile: async () => ({ path: "file" }),
    });
    harness.render().enqueue([{ path: "folder", file: null }, { path: "file", file: new File(["a"], "file") }], "", () => false);
    const queue = await harness.settle();
    assert.ok(queue.tasks.every((task) => task.status === "completed"));
    assert.deepEqual(signed, [false, true]);
    assert.equal(prompts, 1);
    assert.equal(harness.timers.size, 0);
  } finally { globalThis.window = previousWindow; }
});

test("hung deletion checks time out, let new waits progress, and retire immediately on cancellation", async () => {
  const signals = [];
  const bodies = [];
  let otherDone = false;
  const stuckFile = new File(["a"], "stuck");
  const harness = await setup({
    signUpload: async (path) => {
      if (path === "stuck") throw deletionConflict("delete-stuck", "stuck");
      if (!otherDone) throw deletionConflict("delete-other", "other");
      return { uploadId: path };
    },
    uploadFile: async (id) => { bodies.push(id); return { path: id }; },
    deletionStatus: (id, signal) => {
      if (id === "delete-stuck") { signals.push(signal); return new Promise(() => {}); }
      return Promise.resolve({ job: { id, status: "done" } });
    },
  });
  harness.render().enqueue([{ path: "stuck", file: stuckFile }], "", () => false);
  await harness.settle();
  await harness.tick();
  harness.render().enqueue([{ path: "other", file: new File(["b"], "other") }], "", () => false);
  let queue = await harness.settle();
  assert.ok(queue.tasks.every((task) => task.status === "waiting"));
  queue = await harness.tick(15000);
  assert.equal(signals[0].aborted, true);
  assert.equal(queue.tasks[0].status, "waiting");
  assert.equal(queue.tasks[0].waitMessage, "drive.deletionStatusUnavailable");
  assert.equal(queue.tasks[0].file, stuckFile);
  assert.deepEqual(bodies, []);
  otherDone = true;
  queue = await harness.tick();
  assert.equal(queue.tasks[1].status, "completed");
  assert.deepEqual(bodies, ["other"]);
  assert.equal(signals.length, 2);
  queue.cancel(queue.tasks[0].id);
  assert.equal(signals[1].aborted, true);
  queue = await harness.settle();
  assert.equal(harness.timers.size, 0);
  assert.deepEqual(queue.tasks.map((task) => task.path), ["other"]);
});
