import assert from "node:assert/strict";
import { createHash, webcrypto } from "node:crypto";
import { readFile } from "node:fs/promises";
import { transformWithOxc } from "vite";

const queueSource = await readFile(new URL("../../src/pages/useUploadQueue.ts", import.meta.url), "utf8");
const persistenceSource = await readFile(new URL("../../src/pages/uploadPersistence.ts", import.meta.url), "utf8");
const moduleURL = (code) => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
let identity = 0;
export class TestApiError extends Error {
  constructor(message, status = 0, metadata = {}) { super(message); this.status = status; Object.assign(this, metadata); }
}
export function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
export function memoryStorage() {
  const values = new Map();
  return {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, String(value)),
    removeItem: (key) => values.delete(key),
  };
}
export const digest = (bytes) => createHash("sha256").update(bytes).digest("hex");
export const item = (path, kind = "file", size = 1) => ({ path: `/${path.replace(/^\/+/, "")}`, kind, size, contentRevision: "content-v1", revision: "entry-v1" });
export const operation = (id, state = "running", result = null, kind = "delete") => ({ id, kind, state, progress: { completed: 0, total: null, unit: "entries" }, result, error: null, cancelRequested: false });
export const conflict = (id = "delete-1") => new TestApiError("operation in progress", 409, { code: "operation_in_progress", operationId: id });

/** A durable fake server: ACK loss can change server state before throwing. */
export function uploadServer(partSize = 2) {
  const calls = [];
  const sessions = new Map();
  const keys = new Map();
  const receipts = new Map();
  const api = {
    me: async () => ({ id: "owner-a" }),
    capabilities: async () => ({ sourceModifiedAt: false }),
    mkdir: async (path) => { calls.push(["mkdir", path]); return item(path, "folder", null); },
    metadata: async (path) => { calls.push(["metadata", path]); return item(path); },
    createUpload: async (input, key) => {
      calls.push(["create", structuredClone(input), key]);
      if (keys.has(key)) return { ...sessions.get(keys.get(key)) };
      const id = `session-${sessions.size + 1}`;
      const session = { id, path: input.path, state: "open", size: input.size, partSize, partCount: Math.ceil(input.size / partSize), maxParallelism: 2, ordering: "any", operationId: `commit-${id}`, result: null };
      sessions.set(id, session); keys.set(key, id); receipts.set(id, new Map());
      return { ...session };
    },
    uploadStatus: async (id) => { calls.push(["status", id]); assert.ok(sessions.has(id), "query an allocated session"); return { ...sessions.get(id) }; },
    uploadParts: async (id, cursor) => { calls.push(["receipts", id, cursor]); return { items: [...receipts.get(id).values()], watermark: receipts.get(id).size, nextCursor: null }; },
    uploadPart: async (id, number, blob, signal, progress) => {
      calls.push(["part", id, number, blob]);
      const receipt = { number, size: blob.size, sha256: digest(Buffer.from(await blob.arrayBuffer())) };
      receipts.get(id).set(number, receipt);
      progress?.({ loaded: blob.size, total: blob.size });
      return receipt;
    },
    commitUpload: async (id) => {
      calls.push(["commit", id]);
      const session = sessions.get(id);
      assert.equal(receipts.get(id).size, session.partCount, "publication requires all durable receipts");
      assert.equal([...receipts.get(id).values()].reduce((size, receipt) => size + receipt.size, 0), session.size, "durable receipts must cover exact file bytes");
      session.state = "committed"; session.result = item(session.path, "file", session.size); return session.result;
    },
    abortUpload: async (id) => {
      calls.push(["abort", id]);
      const session = sessions.get(id);
      if (session.state === "committed") throw new TestApiError("already committed", 409, { code: "already_committed" });
      if (session.state === "committing" || session.state === "outcome_unknown") throw new TestApiError("publication active", 409, { code: "commit_in_progress" });
      if (session.state === "expired" || session.state === "failed") throw new TestApiError("upload gone", 410, { code: "upload_gone" });
      session.state = "aborted";
    },
    getOperation: async (id) => { calls.push(["operation", id]); return operation(id); },
    cancelOperation: async (id) => { calls.push(["cancelOperation", id]); return operation(id, "cancelled"); },
  };
  return { api, calls, sessions, keys, receipts };
}

export async function setup(overrides = {}, { storage = memoryStorage(), owner = "owner-a", confirm = () => true } = {}) {
  const slots = [], effects = [], timers = new Map(), completed = [];
  let timerId = 0, cursor = 0, mounted = true;
  const same = (a, b) => a && b && a.length === b.length && a.every((value, i) => Object.is(value, b[i]));
  const schedule = (callback, delay, repeat) => { assert.ok(delay === 2000 || delay === 15000, `unexpected timer ${delay}`); timers.set(++timerId, { callback, delay, repeat }); return timerId; };
  const runtime = {
    api: { me: async () => ({ id: owner }), ...overrides }, ApiError: TestApiError,
    localStorage: storage, crypto: webcrypto, window: { confirm },
    setTimeout: (fn, delay) => schedule(fn, delay, false), clearTimeout: (id) => timers.delete(id),
    setInterval: (fn, delay) => schedule(fn, delay, true), clearInterval: (id) => timers.delete(id),
    useRef: (value) => slots[cursor++] ??= { current: value },
    useState: (initial) => {
      const at = cursor++;
      if (!(at in slots)) slots[at] = initial;
      return [slots[at], (value) => { slots[at] = typeof value === "function" ? value(slots[at]) : value; }];
    },
    useCallback: (callback, deps) => {
      const at = cursor++;
      if (!slots[at] || !same(slots[at].deps, deps)) slots[at] = { value: callback, deps };
      return slots[at].value;
    },
    useEffect: (callback, deps) => {
      const at = cursor++;
      if (!slots[at] || !same(slots[at].deps, deps)) effects.push(() => {
        slots[at]?.cleanup?.(); slots[at] = { deps, cleanup: callback() };
      });
    },
  };
  globalThis.__uploadHarness = runtime;
  const unique = ++identity;
  const persistenceCode = (await transformWithOxc(`const { localStorage } = globalThis.__uploadHarness;\n${persistenceSource}\n// harness ${unique}`, "uploadPersistence.ts")).code;
  const persistenceURL = moduleURL(persistenceCode);
  const persistence = await import(persistenceURL);
  const source = queueSource
    .replace(/import \{ useCallback, useEffect, useRef, useState \} from "react";/, "const { useCallback, useEffect, useRef, useState, setTimeout, clearTimeout, setInterval, clearInterval, localStorage, crypto, window } = globalThis.__uploadHarness;")
    .replace(/import \{ ApiError, api,[^\n]+from "\.\.\/api\/client";/, "const { ApiError, api } = globalThis.__uploadHarness;")
    .replace('import { t } from "../i18n";', 'const t = (key) => key;')
    .replace('from "./uploadPersistence"', `from "${persistenceURL}"`);
  const { code } = await transformWithOxc(`${source}\n// harness ${unique}`, "useUploadQueue.ts");
  const { useUploadQueue } = await import(moduleURL(code));
  const render = () => {
    cursor = 0;
    const queue = useUploadQueue((task, result) => completed.push({ task, item: result }));
    if (mounted) while (effects.length) effects.shift()();
    return queue;
  };
  const settle = async () => {
    for (let i = 0; i < 25; i++) await new Promise((resolve) => setImmediate(resolve));
    return render();
  };
  const until = async (predicate, label = "expected queue state") => {
    for (let i = 0; i < 100; i++) { const queue = await settle(); if (predicate(queue)) return queue; }
    assert.fail(`${label}: ${JSON.stringify(render().tasks.map(({ path, status, error }) => ({ path, status, error })))}`);
  };
  const tick = async (delay = 2000) => {
    for (const [id, timer] of [...timers]) if (timer.delay === delay) {
      if (!timer.repeat) timers.delete(id);
      void timer.callback();
    }
    return settle();
  };
  const unmount = () => {
    mounted = false;
    for (const slot of slots) slot?.cleanup?.();
  };
  render();
  return { render, settle, until, tick, timers, storage, completed, persistence, unmount };
}
