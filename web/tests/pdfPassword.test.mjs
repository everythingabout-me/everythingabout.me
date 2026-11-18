import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

// The password hook is plain TypeScript over a loading task, so it runs in node unchanged; only
// the module syntax has to be transformed first.
const source = await readFile(new URL("../src/components/preview/pdfPassword.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "pdfPassword.ts");
const { attachPasswordPrompt, pdfWithPasswordPrompt } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

function fakeTask() {
  const task = {
    onPassword: null,
    destroyed: false,
    destroy() {
      task.destroyed = true;
      return Promise.resolve();
    },
  };
  return task;
}

test("a handler is attached to the task the viewer created", () => {
  const task = fakeTask();
  assert.equal(attachPasswordPrompt(task, async () => "pw"), task);
  assert.equal(typeof task.onPassword, "function");
});

test("the typed password reaches pdf.js", async () => {
  const task = fakeTask();
  const asked = [];
  attachPasswordPrompt(task, async (request) => {
    asked.push(request);
    return "1004501016051992";
  });
  let supplied = null;
  task.onPassword((password) => { supplied = password; }, 1);
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(asked, ["required"]);
  assert.equal(supplied, "1004501016051992");
});

test("a second request reports the password as wrong", async () => {
  const task = fakeTask();
  const asked = [];
  attachPasswordPrompt(task, async (request) => {
    asked.push(request);
    return "again";
  });
  task.onPassword(() => {}, 2);
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(asked, ["incorrect"]);
});

test("closing the dialog ends the task instead of leaving it pending", async () => {
  const task = fakeTask();
  attachPasswordPrompt(task, async () => null);
  let supplied = false;
  task.onPassword(() => { supplied = true; }, 1);
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(supplied, false);
  assert.equal(task.destroyed, true);
});

test("the module the viewer receives is the same module with one wrapped method", async () => {
  const seen = [];
  const issued = [];
  const module = {
    version: "4.10.38",
    GlobalWorkerOptions: { workerSrc: "" },
    getDocument(...args) {
      seen.push(args);
      const task = fakeTask();
      issued.push(task);
      return task;
    },
  };
  const wrapped = pdfWithPasswordPrompt(module, async () => "pw");
  assert.equal(wrapped.version, "4.10.38");
  assert.equal(wrapped.GlobalWorkerOptions, module.GlobalWorkerOptions);
  const task = wrapped.getDocument({ url: "/x.pdf" }, "4.10.38");
  assert.equal(task, issued[0]);
  assert.deepEqual(seen, [[{ url: "/x.pdf" }, "4.10.38"]]);
  assert.equal(typeof task.onPassword, "function");
  // The module the viewer keeps is untouched: only the wrapped copy attaches the handler.
  const plain = module.getDocument({ url: "/plain.pdf" });
  assert.equal(plain.onPassword, null);
});
