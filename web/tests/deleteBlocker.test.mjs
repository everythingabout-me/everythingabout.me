import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/pages/deleteBlocker.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "deleteBlocker.ts");
const { resolveDeleteBlocker } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

const failure = (status) => Object.assign(new Error(`HTTP ${status}`), { status });
const client = ({ operation, session } = {}) => ({
  getOperation: async () => {
    if (operation instanceof Error) throw operation;
    return operation;
  },
  uploadStatus: async () => {
    if (session instanceof Error) throw session;
    return session;
  },
});

// An open session holds no guard, so the only upload a delete can be refused by is one
// that is publishing. Nothing here may answer with a cancellation: the path, the session
// state and where the session sits relative to the deleted folder change nothing.
test("a committing upload is named and never cancelled", async () => {
  for (const path of ["/Photos/a.jpg", "/健康/手表数据/apple-health/a.jpg"]) {
    for (const state of ["committing", "outcome_unknown"]) {
      const result = await resolveDeleteBlocker(client({
        operation: { kind: "upload_commit", state: "running" },
        session: { id: "u1", path, state },
      }), "op-1");
      assert.deepEqual(result, { resolution: "blocked", reason: "upload", name: "a.jpg" });
    }
  }
});

test("the resolution never asks for a cancellation", async () => {
  const calls = [];
  const result = await resolveDeleteBlocker({
    getOperation: async () => { calls.push("getOperation"); return { kind: "upload_commit", state: "queued" }; },
    uploadStatus: async () => { calls.push("uploadStatus"); return { id: "u1", path: "/Photos/a.jpg", state: "open" }; },
    abortUpload: async () => { calls.push("abortUpload"); },
  }, "op-1");
  assert.deepEqual(result, { resolution: "blocked", reason: "upload", name: "a.jpg" });
  assert.deepEqual(calls, ["getOperation", "uploadStatus"]);
});

test("a settled blocker lets the delete retry", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "cancelled" },
  }), "op-1");
  assert.deepEqual(result, { resolution: "retry" });
});

test("a blocker that disappeared between the refusal and the read is retried", async () => {
  for (const status of [404, 410]) {
    assert.deepEqual(await resolveDeleteBlocker(client({ operation: failure(status) }), "op-1"), { resolution: "retry" });
    assert.deepEqual(await resolveDeleteBlocker(client({
      operation: { kind: "upload_commit", state: "queued" },
      session: failure(status),
    }), "op-1"), { resolution: "retry" });
  }
});

test("a non-upload blocker is reported generically", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "delete", state: "running" },
  }), "op-1");
  assert.deepEqual(result, { resolution: "blocked", reason: "other" });
});

test("a blocker this browser may not read is reported generically", async () => {
  assert.deepEqual(
    await resolveDeleteBlocker(client({ operation: failure(403) }), "op-1"),
    { resolution: "blocked", reason: "other" },
  );
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "queued" },
    session: failure(403),
  }), "op-1");
  assert.deepEqual(result, { resolution: "blocked", reason: "other" });
});

test("an unexpected read failure is not swallowed", async () => {
  await assert.rejects(resolveDeleteBlocker(client({ operation: failure(500) }), "op-1"), { status: 500 });
});
