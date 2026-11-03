import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/pages/uploadOwnership.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "uploadOwnership.ts");
const moduleURL = `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
const { claimUploadPath } = await import(`${moduleURL}#tab-a`);
const { claimUploadPath: claimFromOtherTab } = await import(`${moduleURL}#tab-b`);

// Reserve a lock before callback delivery, as a grant can race with cancellation.
class FakeLockManager {
  held = new Map();
  calls = [];
  deferred = false;
  deliveries = [];
  afterCallback;

  request(name, options, callback) {
    if (options.ifAvailable && options.signal !== undefined) {
      return Promise.reject(new DOMException("ifAvailable cannot be combined with signal", "NotSupportedError"));
    }
    const lock = this.held.has(name) ? null : { name, mode: "exclusive" };
    if (lock) this.held.set(name, lock);
    const finished = new Promise((resolve, reject) => {
      const finish = (value, error) => {
        if (lock && this.held.get(name) === lock) this.held.delete(name);
        if (error) reject(error);
        else resolve(value);
      };
      const deliver = () => {
        try {
          const pending = callback(lock);
          this.afterCallback?.();
          Promise.resolve(pending).then((value) => finish(value), (error) => finish(undefined, error));
        } catch (error) {
          finish(undefined, error);
        }
      };
      if (this.deferred) this.deliveries.push(deliver);
      else queueMicrotask(deliver);
    });
    this.calls.push({ name, options, finished });
    return finished;
  }
}

function setNavigator(t, value) {
  const original = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  Object.defineProperty(globalThis, "navigator", { configurable: true, value });
  t.after(() => {
    if (original) Object.defineProperty(globalThis, "navigator", original);
    else delete globalThis.navigator;
  });
}

function setup(t) {
  const locks = new FakeLockManager();
  setNavigator(t, { locks });
  const controllers = [];
  const signal = () => {
    const controller = new AbortController();
    controllers.push(controller);
    return controller.signal;
  };
  t.after(() => controllers.forEach((controller) => controller.abort()));
  return { locks, signal };
}

const isAbort = (error) => error instanceof Error && error.name === "AbortError";

test("separate tabs exclusively own the same account and normalized path without queuing", async (t) => {
  const { locks, signal } = setup(t);
  const firstSignal = signal();
  const release = await claimUploadPath("owner-a", "/Cafe\u0301/report.txt/", firstSignal);
  assert.equal(typeof release, "function");
  assert.equal(await claimFromOtherTab("owner-a", "Café/report.txt", signal()), null);
  assert.equal(locks.calls[0].name, 'eam.upload-path.v1:["owner-a","/Café/report.txt"]');
  assert.deepEqual(locks.calls[0].options, { mode: "exclusive", ifAvailable: true });
  assert.equal(locks.held.size, 1);

  release();
  release();
  await locks.calls[0].finished;
  assert.equal(locks.held.size, 0);
  const nextRelease = await claimFromOtherTab("owner-a", "/Café/report.txt", signal());
  assert.equal(typeof nextRelease, "function", "explicit release or unmount hands ownership to a new claim");
  release();
  assert.equal(locks.held.size, 1, "an old release cannot release a later owner's lock");
  nextRelease();
  await locks.calls.at(-1).finished;
  assert.equal(locks.held.size, 0);
});

test("different accounts and target paths can hold independent locks", async (t) => {
  const { locks, signal } = setup(t);
  const releases = await Promise.all([
    claimUploadPath("owner-a", "folder/file", signal()),
    claimFromOtherTab("owner-b", "folder/file", signal()),
    claimFromOtherTab("owner-a", "folder/other", signal()),
  ]);
  assert.ok(releases.every((release) => typeof release === "function"));
  assert.equal(locks.held.size, 3);
  for (const release of releases) release();
  await Promise.all(locks.calls.map((call) => call.finished));
  assert.equal(locks.held.size, 0);
});

test("canceling an acquired claim releases it for another tab", async (t) => {
  const { locks, signal } = setup(t);
  const controller = new AbortController();
  t.after(() => controller.abort());
  const release = await claimUploadPath("owner-a", "file", controller.signal);
  assert.equal(await claimFromOtherTab("owner-a", "file", signal()), null);
  controller.abort();
  await locks.calls[0].finished;
  const nextRelease = await claimFromOtherTab("owner-a", "file", signal());
  assert.equal(typeof nextRelease, "function");
  release();
  assert.equal(locks.held.size, 1);
  nextRelease();
  await locks.calls.at(-1).finished;
});

test("an already aborted signal rejects without requesting ownership", async (t) => {
  const { locks } = setup(t);
  const controller = new AbortController();
  controller.abort("unmounted");
  await assert.rejects(claimUploadPath("owner-a", "file", controller.signal), isAbort);
  assert.equal(locks.calls.length, 0);
});

test("abort after grant but before delivery rejects promptly and a delayed callback cannot leak a lock", async (t) => {
  const { locks, signal } = setup(t);
  locks.deferred = true;
  const controller = new AbortController();
  const pending = claimUploadPath("owner-a", "file", controller.signal);
  const rejected = assert.rejects(pending, isAbort);
  assert.equal(locks.held.size, 1, "the manager has granted the lock but has not called back");
  controller.abort();
  await rejected;
  assert.equal(locks.deliveries.length, 1, "rejection does not wait for the callback");
  locks.deliveries.shift()();
  await locks.calls[0].finished;
  assert.equal(locks.held.size, 0);
  locks.deferred = false;
  const release = await claimFromOtherTab("owner-a", "file", signal());
  assert.equal(typeof release, "function");
  release();
  await locks.calls.at(-1).finished;
});

test("abort between acquisition and returning the release handle never leaves an unreachable lock", async (t) => {
  const { locks } = setup(t);
  const controller = new AbortController();
  locks.afterCallback = () => controller.abort();
  await assert.rejects(claimUploadPath("owner-a", "file", controller.signal), isAbort);
  await locks.calls[0].finished;
  assert.equal(locks.held.size, 0);
});

test("missing Web Locks and rejected requests throw Errors rather than report a busy path", async (t) => {
  setNavigator(t, undefined);
  await assert.rejects(claimUploadPath("owner-a", "file", new AbortController().signal), Error);
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: {} });
  await assert.rejects(claimUploadPath("owner-a", "file", new AbortController().signal), Error);

  const failure = new DOMException("Locks are blocked", "SecurityError");
  Object.defineProperty(globalThis, "navigator", {
    configurable: true,
    value: { locks: { request: () => Promise.reject(failure) } },
  });
  await assert.rejects(claimUploadPath("owner-a", "file", new AbortController().signal), (error) => error === failure);
  navigator.locks.request = () => { throw failure; };
  await assert.rejects(claimUploadPath("owner-a", "file", new AbortController().signal), (error) => error === failure);
  navigator.locks.request = () => Promise.reject("unavailable");
  await assert.rejects(claimUploadPath("owner-a", "file", new AbortController().signal), (error) => {
    assert.ok(error instanceof Error);
    assert.equal(error.cause, "unavailable");
    return true;
  });
});
