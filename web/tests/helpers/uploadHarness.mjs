import assert from "node:assert/strict";
import { createHash, webcrypto } from "node:crypto";
import { readFile } from "node:fs/promises";
import { transformWithOxc } from "vite";

const queueSource = await readFile(new URL("../../src/pages/useUploadQueue.ts", import.meta.url), "utf8");
const persistenceSource = await readFile(new URL("../../src/pages/uploadPersistence.ts", import.meta.url), "utf8");
const ownershipSource = await readFile(new URL("../../src/pages/uploadOwnership.ts", import.meta.url), "utf8");
const selectionSource = await readFile(new URL("../../src/pages/uploadSelection.ts", import.meta.url), "utf8");
const moduleURL = (code) => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
let identity = 0;
export class TestApiError extends Error {
  constructor(message, status = 0, metadata = {}) { super(message); this.status = status; Object.assign(this, metadata); }
}
/** Mirrors FileApiError: a Problem Details failure carrying the server problem. */
export class TestFileApiError extends TestApiError {
  constructor(problem) { super(problem.detail, problem.status, problem); this.problem = problem; }
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
/** One same-origin Web Locks manager; share it between harnesses to model pages. */
export function memoryLocks() {
  const held = new Map(), pending = [], requests = [];
  const drain = () => {
    for (let i = 0; i < pending.length;) {
      const request = pending[i];
      if (held.has(request.name) && !request.options.ifAvailable) { i++; continue; }
      pending.splice(i, 1);
      request.options.signal?.removeEventListener("abort", request.abort);
      const lock = held.has(request.name) ? null : { name: request.name, mode: "exclusive" };
      if (lock) held.set(request.name, lock);
      Promise.resolve().then(() => request.callback(lock)).then(
        (value) => { if (lock) held.delete(request.name); request.resolve(value); drain(); },
        (error) => { if (lock) held.delete(request.name); request.reject(error); drain(); },
      );
    }
  };
  return {
    requests,
    request(name, options, callback) {
      requests.push(name);
      assert.equal(options.mode, "exclusive", "the fake supports exclusive Web Locks");
      if (options.ifAvailable && options.signal) return Promise.reject(new DOMException("signal and ifAvailable cannot be used together", "NotSupportedError"));
      return new Promise((resolve, reject) => {
        const request = { name, options, callback, resolve, reject };
        request.abort = () => {
          const at = pending.indexOf(request);
          if (at < 0) return;
          pending.splice(at, 1);
          reject(options.signal.reason ?? new DOMException("Lock request aborted", "AbortError"));
          queueMicrotask(drain);
        };
        if (options.signal?.aborted) { reject(options.signal.reason ?? new DOMException("Lock request aborted", "AbortError")); return; }
        pending.push(request);
        options.signal?.addEventListener("abort", request.abort, { once: true });
        queueMicrotask(drain);
      });
    },
    async query() {
      return { held: [...held.values()], pending: pending.map(({ name }) => ({ name, mode: "exclusive" })) };
    },
  };
}
export const digest = (bytes) => createHash("sha256").update(bytes).digest("hex");
export const item = (path, kind = "file", size = 1) => ({ path: `/${path.replace(/^\/+/, "")}`, kind, size, contentRevision: "content-v1", revision: "entry-v1" });
export const operation = (id, state = "running", result = null, kind = "delete") => ({ id, kind, state, progress: { completed: 0, total: null, unit: "entries" }, result, error: null, cancelRequested: false });
// The default blocker is a deletion, because that is what the page's own records and the
// docs describe; a test that simulates a folder-creation race overrides the kind.
export const conflict = (id = "delete-1", kind = "delete") => new TestApiError("operation in progress", 409, { code: "operation_in_progress", operationId: id, operationKind: kind });

/** A durable fake server: ACK loss can change server state before throwing. */
export function uploadServer(partSize = 2, options = {}) {
  const classA = options.class === "A";
  const calls = [];
  const probes = [];
  const sessions = new Map();
  const keys = new Map();
  const receipts = new Map();
  // The real server reports how much a session already holds, authoritative for both
  // classes (class A from the provider's listing). The queue uses it to skip a listing that
  // can only be empty on a session it just created.
  const progress = (id) => {
    const session = sessions.get(id);
    const parts = [...receipts.get(id).values()];
    return { ...session, receivedParts: parts.length, receivedBytes: parts.reduce((sum, part) => sum + part.size, 0) };
  };
  const api = {
    me: async () => ({ id: "owner-a" }),
    capabilities: async (selector) => {
      // Kept out of `calls`: the probe is not part of the upload flow that tests assert on
      // call by call, but how often and with which selector it is asked is its own question.
      probes.push(selector);
      return { sourceModifiedAt: false, transferClass: "C", transferKind: "relay" };
    },
    mkdir: async (path) => { calls.push(["mkdir", path]); return item(path, "folder", null); },
    metadata: async (path) => { calls.push(["metadata", path]); return item(path); },
    createUpload: async (input, key) => {
      calls.push(["create", structuredClone(input), key]);
      if (keys.has(key)) return { ...sessions.get(keys.get(key)) };
      const id = `session-${sessions.size + 1}`;
      const transport = classA
        ? { kind: "presigned-parts", parts: [], signUrl: `/api/files/uploads/${id}/parts/sign`, expiresAt: new Date(Date.now() + 3600_000).toISOString() }
        : { kind: "relay", url: `/api/files/uploads/${id}/parts` };
      const session = { id, class: classA ? "A" : "C", transport, path: input.path, state: "open", size: input.size, partSize, partCount: Math.ceil(input.size / partSize), ordering: "any", operationId: `commit-${id}`, result: null };
      sessions.set(id, session); keys.set(key, id); receipts.set(id, new Map());
      // A resumed session can already hold parts; declare them so the client is expected to
      // list and validate receipts rather than assume the session is empty.
      for (let number = 1; number <= (options.resumeParts ?? 0); number++) {
        const size = number === session.partCount ? input.size - (number - 1) * partSize : partSize;
        receipts.get(id).set(number, { number, size, sha256: digest(String.fromCharCode(96 + number)) });
      }
      return progress(id);
    },
    uploadStatus: async (id) => {
      calls.push(["status", id]);
      if (!sessions.has(id)) throw new TestApiError("upload not found", 404, { code: "not_found" });
      return progress(id);
    },
    uploadParts: async (id, cursor) => {
      calls.push(classA ? ["providerParts", id, cursor] : ["receipts", id, cursor]);
      if (classA) {
        const items = [...receipts.get(id).values()].map((part) => ({ number: part.number, size: part.size, etag: `etag-${part.number}` }));
        return { items, nextCursor: null };
      }
      return { items: [...receipts.get(id).values()], watermark: receipts.get(id).size, nextCursor: null };
    },
    signUploadParts: async (id, numbers) => {
      calls.push(["sign", id, numbers]);
      const session = sessions.get(id);
      return numbers.map((number) => ({ number, size: number === session.partCount ? session.size - (number - 1) * partSize : partSize,
        method: "PUT", url: `/api/files/transfer/drive/owner/file?partNumber=${number}&uploadId=${id}`,
        headers: { "Content-Type": "application/octet-stream", "Content-Length": String(number === session.partCount ? session.size - (number - 1) * partSize : partSize) },
        expiresAt: new Date(Date.now() + 3600_000).toISOString() }));
    },
    uploadPart: async (id, number, blob, signal, progress) => {
      calls.push(["part", id, number, blob]);
      const receipt = { number, size: blob.size, sha256: digest(Buffer.from(await blob.arrayBuffer())) };
      receipts.get(id).set(number, receipt);
      progress?.({ loaded: blob.size, total: blob.size });
      return receipt;
    },
    uploadPresignedPart: async (grant, blob, signal, progress) => {
      calls.push(["presignedPart", grant.number, blob]);
      const id = new URL(grant.url, "https://upload.invalid").searchParams.get("uploadId");
      receipts.get(id).set(grant.number, { number: grant.number, size: blob.size });
      progress?.({ loaded: blob.size, total: blob.size });
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
  return { api, calls, probes, sessions, keys, receipts };
}

export async function setup(overrides = {}, options = {}) {
  const { storage = memoryStorage(), owner = "owner-a" } = options;
  const locks = Object.hasOwn(options, "locks") ? options.locks : memoryLocks();
  const navigator = { locks };
  const slots = [], effects = [], timers = new Map(), completed = [], listeners = new Map();
  let timerId = 0, cursor = 0, mounted = true;
  // Every published task-list state is one render of the whole page. Counting them makes
  // "selecting a folder must not publish once per file" an assertion instead of a hope.
  let stateUpdates = 0;
  const window = {
    // The queue asks in the page, not with a native dialog: one refusal is a decision about
    // a whole import, and `window.confirm` cannot carry that answer. Reaching this is a
    // regression, so it fails the test instead of silently answering.
    confirm: () => assert.fail("the upload queue must ask in the page, not with window.confirm"),
    addEventListener(type, listener) {
      if (!listeners.has(type)) listeners.set(type, new Set());
      listeners.get(type).add(listener);
    },
    removeEventListener(type, listener) {
      const registered = listeners.get(type);
      registered?.delete(listener);
      if (registered?.size === 0) listeners.delete(type);
    },
  };
  const dispatchBeforeUnload = () => {
    const event = { type: "beforeunload", cancelable: true, defaultPrevented: false, returnValue: undefined,
      preventDefault() { this.defaultPrevented = true; } };
    for (const listener of [...(listeners.get(event.type) ?? [])]) listener.call(window, event);
    return event;
  };
  const same = (a, b) => a && b && a.length === b.length && a.every((value, i) => Object.is(value, b[i]));
  // Only the queue's documented waits may be scheduled: the page publish cadence at 100, part retries
  // 250/500/1000/2000/4000/8000 (capped), receiver-busy backoff, the operation poll at 2000 and the 60 s
  // status deadline that must outlast a request-budget wait.
  const allowed = [100, 250, 500, 1000, 2000, 4000, 8000, 60000];
  const schedule = (callback, delay, repeat) => { assert.ok(allowed.includes(delay), `unexpected timer ${delay}`); timers.set(++timerId, { callback, delay, repeat }); return timerId; };
  const runtime = {
    api: { me: async () => ({ id: owner }), ...overrides }, ApiError: TestApiError, FileApiError: TestFileApiError,
    localStorage: storage, crypto: webcrypto, window, navigator,
    setTimeout: (fn, delay) => schedule(fn, delay, false), clearTimeout: (id) => timers.delete(id),
    setInterval: (fn, delay) => schedule(fn, delay, true), clearInterval: (id) => timers.delete(id),
    useRef: (value) => slots[cursor++] ??= { current: value },
    useState: (initial) => {
      const at = cursor++;
      if (!(at in slots)) slots[at] = initial;
      return [slots[at], (value) => { stateUpdates++; slots[at] = typeof value === "function" ? value(slots[at]) : value; }];
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
  const ownershipCode = (await transformWithOxc(`const { navigator } = globalThis.__uploadHarness;\n${ownershipSource}\n// harness ${unique}`, "uploadOwnership.ts")).code;
  const ownershipURL = moduleURL(ownershipCode);
  const ownership = await import(ownershipURL);
  const selectionCode = (await transformWithOxc(`${selectionSource}\n// harness ${unique}`, "uploadSelection.ts")).code;
  const selectionURL = moduleURL(selectionCode);
  const source = queueSource
    .replace(/import \{ useCallback, useEffect, useRef, useState \} from "react";/, "const { useCallback, useEffect, useRef, useState, setTimeout, clearTimeout, setInterval, clearInterval, localStorage, crypto, window } = globalThis.__uploadHarness;")
    .replace(/import \{ ApiError, FileApiError, api,[^\n]+from "\.\.\/api\/client";/, "const { ApiError, FileApiError, api } = globalThis.__uploadHarness;")
    .replace('import { t } from "../i18n";', 'const t = (key) => key;')
    .replace('from "./uploadPersistence"', `from "${persistenceURL}"`)
    .replace('from "./uploadOwnership"', `from "${ownershipURL}"`)
    .replace('from "./uploadSelection"', `from "${selectionURL}"`);
  const { code } = await transformWithOxc(`${source}\n// harness ${unique}`, "useUploadQueue.ts");
  const { useUploadQueue } = await import(moduleURL(code));
  const render = () => {
    cursor = 0;
    const queue = useUploadQueue((task, result) => completed.push({ task, item: result }));
    if (mounted) while (effects.length) effects.shift()();
    return queue;
  };
  // A published state is the page's next paint. One settle is one paint: it drains the
  // queue's pending work and then lets the coalesced publication fire exactly once, so a
  // test counts paints the way the browser does instead of one per task change.
  let paints = 0;
  const paint = () => {
    for (const [id, timer] of [...timers]) {
      if (timer.delay !== 100 || timer.repeat) continue;
      timers.delete(id);
      paints += 1;
      timer.callback();
    }
  };
  const settle = async () => {
    for (let i = 0; i < 25; i++) await new Promise((resolve) => setImmediate(resolve));
    paint();
    await new Promise((resolve) => setImmediate(resolve));
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
    if (!mounted) return;
    mounted = false;
    for (const slot of slots) slot?.cleanup?.();
  };
  render();
  return { render, settle, until, tick, timers, storage, locks, ownership, completed, persistence, listeners, dispatchBeforeUnload, unmount, updates: () => stateUpdates, paints: () => paints };
}
