import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "client.ts");
const { api, ApiError } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

class FakeXHR {
  static instances = [];
  upload = {};
  headers = {};
  status = 0;
  responseText = "";
  constructor() { FakeXHR.instances.push(this); }
  open(method, url) { this.method = method; this.url = url; }
  setRequestHeader(name, value) { this.headers[name] = value; }
  send(body) { this.body = body; }
  abort() { this.aborted = true; this.onabort?.(); }
  respond(status, payload) {
    this.status = status;
    this.responseText = typeof payload === "string" ? payload : JSON.stringify(payload);
    this.onload();
  }
}

test("streaming upload keeps bytes on the API origin and waits for the indexed response", async () => {
  const original = globalThis.XMLHttpRequest;
  globalThis.XMLHttpRequest = FakeXHR;
  try {
    const file = new File(["abc"], "sample.json", { type: "application/json" });
    const progress = [];
    let sent = 0;
    let completed = false;
    const controller = new AbortController();
    const pending = api.uploadFile("receipt +?", file, (value) => progress.push(value), () => sent++, controller.signal);
    pending.then(() => { completed = true; });
    const xhr = FakeXHR.instances.at(-1);
    assert.equal(xhr.method, "POST");
    assert.equal(xhr.url, "/api/fs/upload/confirm?uploadId=receipt+%2B%3F");
    assert.equal(xhr.withCredentials, true);
    assert.equal(xhr.body, file);
    assert.equal(xhr.headers["Content-Type"], "application/octet-stream");
    xhr.upload.onprogress({ loaded: 2 });
    xhr.upload.onload();
    await Promise.resolve();
    assert.deepEqual(progress, [{ loaded: 2, total: 3 }]);
    assert.equal(sent, 1);
    assert.equal(completed, false, "body transmission alone must not mark the file indexed");
    const node = { path: "sample.json", size: 3 };
    xhr.respond(200, { ok: true, data: { node } });
    assert.deepEqual(await pending, node);
    controller.abort();
    assert.equal(xhr.aborted, undefined, "completed uploads must remove their abort listener");
  } finally { globalThis.XMLHttpRequest = original; }
});

test("zero-byte upload sends its empty File and resolves from server finalization", async () => {
  const original = globalThis.XMLHttpRequest;
  globalThis.XMLHttpRequest = FakeXHR;
  try {
    const file = new File([], "empty");
    const pending = api.uploadFile("empty-id", file, () => {}, () => {}, new AbortController().signal);
    const xhr = FakeXHR.instances.at(-1);
    assert.equal(xhr.body, file);
    xhr.upload.onload();
    xhr.respond(200, { ok: true, data: { node: { path: "empty", size: 0 } } });
    assert.equal((await pending).size, 0);
  } finally { globalThis.XMLHttpRequest = original; }
});

test("aborting receive rejects promptly and suppresses late progress", async () => {
  const original = globalThis.XMLHttpRequest;
  globalThis.XMLHttpRequest = FakeXHR;
  try {
    const controller = new AbortController();
    let progress = 0;
    const pending = api.uploadFile("cancel-id", new File(["body"], "file"), () => progress++, () => progress++, controller.signal);
    const rejected = assert.rejects(pending, { name: "AbortError" });
    const xhr = FakeXHR.instances.at(-1);
    controller.abort();
    await rejected;
    assert.equal(xhr.aborted, true);
    xhr.upload.onprogress({ loaded: 3 });
    xhr.upload.onload();
    assert.equal(progress, 0);
  } finally { globalThis.XMLHttpRequest = original; }
});

test("stream errors preserve retry classification instead of implying publication failed", async () => {
  const original = globalThis.XMLHttpRequest;
  globalThis.XMLHttpRequest = FakeXHR;
  try {
    for (const status of [409, 410, 404, 503]) {
      const pending = api.uploadFile("receipt", new File(["a"], "file"), () => {}, () => {}, new AbortController().signal);
      const rejected = assert.rejects(pending, (error) => error instanceof ApiError && error.status === status && error.message === "server outcome");
      FakeXHR.instances.at(-1).respond(status, { ok: false, error: "server outcome" });
      await rejected;
    }
    const pending = api.uploadFile("receipt", new File(["a"], "file"), () => {}, () => {}, new AbortController().signal);
    const rejected = assert.rejects(pending, (error) => error instanceof ApiError && error.status === 0);
    FakeXHR.instances.at(-1).onerror();
    await rejected;
  } finally { globalThis.XMLHttpRequest = original; }
});

test("receipt allocation captures metadata and finalization retries send no file body", async () => {
  const original = globalThis.fetch;
  const requests = [];
  globalThis.fetch = async (path, init) => {
    requests.push({ path, init });
    return { status: 200, json: async () => ({ ok: true, data: requests.length === 1 ? { uploadId: "id" } : { node: { path: "Health/steps.json" } } }) };
  };
  try {
    assert.deepEqual(await api.signUpload("Health/steps.json", 0, false, "application/json", 1443268800000), { uploadId: "id" });
    assert.equal((await api.confirmUpload("id")).path, "Health/steps.json");
    assert.deepEqual(JSON.parse(requests[0].init.body), { path: "Health/steps.json", size: 0, overwrite: false, mime: "application/json", modifiedAt: 1443268800000 });
    assert.equal(requests[1].path, "/api/fs/upload/confirm");
    assert.deepEqual(JSON.parse(requests[1].init.body), { uploadId: "id" });
  } finally { globalThis.fetch = original; }
});
