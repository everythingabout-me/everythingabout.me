import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = (await readFile(new URL("../src/pages/useUploadQueue.ts", import.meta.url), "utf8"))
  .replace('import { useCallback, useRef, useState } from "react";', "const { useCallback, useRef, useState } = globalThis.__uploadHook;")
  .replace('import { ApiError, api, type Item } from "../api/client";', "const { ApiError, api } = globalThis.__uploadHook;")
  .replace('import { t } from "../i18n";', 'const t = (key) => key;');
const { code } = await transformWithOxc(source, "useUploadQueue.ts");

class TestApiError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}

async function setup(api) {
  const slots = [];
  let cursor = 0;
  globalThis.__uploadHook = {
    api,
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
  return { render, settle, completed };
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
