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

test("an open upload inside the delete target is cancellable", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "queued" },
    session: { id: "u1", path: "/健康/手表数据/apple-health/profile.json", state: "open" },
  }), "op-1", "/健康/手表数据/apple-health");
  assert.deepEqual(result, { resolution: "cancel", id: "u1", name: "profile.json" });
});

test("an open upload at the deleted file itself is cancellable", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "queued" },
    session: { id: "u1", path: "/notes/report.pdf", state: "open" },
  }), "op-1", "/notes/report.pdf");
  assert.deepEqual(result, { resolution: "cancel", id: "u1", name: "report.pdf" });
});

test("path containment respects a directory boundary", async () => {
  const [sibling, nested] = await Promise.all([
    resolveDeleteBlocker(client({
      operation: { kind: "upload_commit", state: "queued" },
      session: { id: "u1", path: "/Photos2/a.jpg", state: "open" },
    }), "op-1", "/Photos"),
    resolveDeleteBlocker(client({
      operation: { kind: "upload_commit", state: "queued" },
      session: { id: "u2", path: "/Photos/2024/a.jpg", state: "open" },
    }), "op-1", "/Photos"),
  ]);
  assert.deepEqual(sibling, { resolution: "blocked", reason: "upload", name: "a.jpg" });
  assert.deepEqual(nested, { resolution: "cancel", id: "u2", name: "a.jpg" });
});

test("a commit that already started is named but not cancelled", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "running" },
    session: { id: "u1", path: "/Photos/a.jpg", state: "committing" },
  }), "op-1", "/Photos");
  assert.deepEqual(result, { resolution: "blocked", reason: "upload", name: "a.jpg" });
});

test("a settled blocker lets the delete retry", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "cancelled" },
  }), "op-1", "/Photos");
  assert.deepEqual(result, { resolution: "retry" });
});

test("a blocker that disappeared between the refusal and the read is retried", async () => {
  for (const status of [404, 410]) {
    assert.deepEqual(await resolveDeleteBlocker(client({ operation: failure(status) }), "op-1", "/Photos"), { resolution: "retry" });
    assert.deepEqual(await resolveDeleteBlocker(client({
      operation: { kind: "upload_commit", state: "queued" },
      session: failure(status),
    }), "op-1", "/Photos"), { resolution: "retry" });
  }
});

test("a non-upload blocker is reported generically", async () => {
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "delete", state: "running" },
  }), "op-1", "/Photos");
  assert.deepEqual(result, { resolution: "blocked", reason: "other" });
});

test("a blocker this browser may not read is reported generically", async () => {
  assert.deepEqual(
    await resolveDeleteBlocker(client({ operation: failure(403) }), "op-1", "/Photos"),
    { resolution: "blocked", reason: "other" },
  );
  const result = await resolveDeleteBlocker(client({
    operation: { kind: "upload_commit", state: "queued" },
    session: failure(403),
  }), "op-1", "/Photos");
  assert.deepEqual(result, { resolution: "blocked", reason: "other" });
});

test("an unexpected read failure is not swallowed", async () => {
  await assert.rejects(resolveDeleteBlocker(client({ operation: failure(500) }), "op-1", "/Photos"), { status: 500 });
});
