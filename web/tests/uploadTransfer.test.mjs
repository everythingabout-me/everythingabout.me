import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "client.ts");
let sequence = 0;
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
  respond(status, payload) { this.status = status; this.responseText = typeof payload === "string" ? payload : JSON.stringify(payload); this.onload(); }
}
const response = (status, payload) => ({ status, json: async () => payload });
const problem = (status, code = "operation_in_progress") => ({ type: `https://everythingabout.me/problems/${code}`, title: "Request failed", status, code, detail: "server outcome", requestId: "request-1", operationId: "operation-1" });
async function setup(t, responder = () => response(200, { csrfToken: "csrf-1" })) {
  const original = { fetch: globalThis.fetch, XMLHttpRequest: globalThis.XMLHttpRequest };
  const requests = [];
  FakeXHR.instances = [];
  globalThis.XMLHttpRequest = FakeXHR;
  globalThis.fetch = async (url, init) => { requests.push({ url, init }); return responder(url, init); };
  t.after(() => Object.assign(globalThis, original));
  const client = await import(`data:text/javascript;base64,${Buffer.from(`${code}\n// instance ${++sequence}`).toString("base64")}`);
  const xhr = async () => {
    for (let i = 0; i < 10 && !FakeXHR.instances.length; i++) await new Promise((resolve) => setImmediate(resolve));
    assert.ok(FakeXHR.instances.length, "part request starts after obtaining CSRF");
    return FakeXHR.instances.at(-1);
  };
  return { ...client, requests, xhr };
}

test("part PUT keeps bytes on the API origin, carries CSRF and resolves only a durable receipt", async (t) => {
  const { api, requests, xhr } = await setup(t);
  const chunk = new Blob(["abc"]);
  const progress = [];
  let completed = false;
  const controller = new AbortController();
  const pending = api.uploadPart("session +?", 2, chunk, controller.signal, (value) => progress.push(value));
  pending.then(() => { completed = true; });
  const request = await xhr();
  assert.equal(request.method, "PUT");
  assert.equal(request.url, "/api/files/uploads/session%20%2B%3F/parts/2");
  assert.equal(request.withCredentials, true);
  assert.equal(request.headers["Content-Type"], "application/octet-stream");
  assert.equal(request.headers["X-CSRF-Token"], "csrf-1");
  assert.equal(request.body, chunk);
  assert.equal(requests[0].url, "/api/files/me");
  assert.equal(requests[0].init.credentials, "same-origin");
  request.upload.onprogress({ loaded: 2 });
  request.upload.onload?.();
  await Promise.resolve();
  assert.deepEqual(progress, [{ loaded: 2, total: 3 }]);
  assert.equal(completed, false, "transmission is not a durable part ACK");
  const receipt = { number: 2, size: 3, sha256: "verified-hash" };
  request.respond(200, receipt);
  assert.deepEqual(await pending, receipt);
  controller.abort();
  assert.equal(request.aborted, undefined, "remove abort listener after ACK");
});

test("aborting an active part rejects promptly and suppresses late progress", async (t) => {
  const { api, xhr } = await setup(t);
  const controller = new AbortController();
  let progress = 0;
  const pending = api.uploadPart("session", 1, new Blob(["body"]), controller.signal, () => progress++);
  const rejection = assert.rejects(pending, { name: "AbortError" });
  const request = await xhr();
  controller.abort();
  await rejection;
  request.upload.onprogress({ loaded: 3 });
  request.respond(200, { number: 1 });
  assert.equal(request.aborted, true);
  assert.equal(progress, 0);
});

test("already aborted part obtains no byte transport", async (t) => {
  const { api } = await setup(t);
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(api.uploadPart("session", 1, new Blob(["x"]), controller.signal), { name: "AbortError" });
  assert.equal(FakeXHR.instances.length, 0);
});

test("part failures preserve Problem Details and transport uncertainty", async (t) => {
  const { api, ApiError, FileApiError } = await setup(t);
  for (const status of [409, 410, 404, 503]) {
    const pending = api.uploadPart("session", 1, new Blob(["a"]));
    const rejection = assert.rejects(pending, (error) => error instanceof FileApiError && error instanceof ApiError && error.status === status && error.code === "operation_in_progress" && error.operationId === "operation-1" && error.problem.requestId === "request-1");
    await new Promise((resolve) => setImmediate(resolve));
    FakeXHR.instances.at(-1).respond(status, problem(status));
    await rejection;
  }
  for (const event of ["onerror", "ontimeout"]) {
    const pending = api.uploadPart("session", 1, new Blob(["a"]));
    const rejection = assert.rejects(pending, (error) => error instanceof ApiError && error.status === 0);
    await new Promise((resolve) => setImmediate(resolve));
    FakeXHR.instances.at(-1)[event]();
    await rejection;
  }
});

test("invalid JSON and malformed errors remain API errors rather than fake receipts", async (t) => {
  const { api, ApiError } = await setup(t);
  for (const [status, body] of [[502, "<html>gateway unavailable</html>"], [409, { status: 409, detail: "missing code" }]]) {
    const pending = api.uploadPart("session", 1, new Blob(["x"]));
    const rejection = assert.rejects(pending, (error) => error instanceof ApiError && error.status === status);
    await new Promise((resolve) => setImmediate(resolve));
    FakeXHR.instances.at(-1).respond(status, body);
    await rejection;
  }
});

test("a rejected CSRF token is refreshed before the next part mutation", async (t) => {
  let tokens = 0;
  const { api } = await setup(t, () => response(200, { csrfToken: `csrf-${++tokens}` }));
  const first = api.uploadPart("session", 1, new Blob(["a"]));
  const rejection = assert.rejects(first, { status: 403 });
  await new Promise((resolve) => setImmediate(resolve));
  FakeXHR.instances.at(-1).respond(403, problem(403, "csrf_failed"));
  await rejection;
  const second = api.uploadPart("session", 1, new Blob(["a"]));
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(FakeXHR.instances.at(-1).headers["X-CSRF-Token"], "csrf-2");
  FakeXHR.instances.at(-1).respond(200, { number: 1, size: 1, sha256: "hash" });
  await second;
});

test("create persists public intent and idempotency key; status, receipt, commit and abort use identity URLs", async (t) => {
  const { api, requests } = await setup(t, (url) => {
    if (url === "/api/files/me") return response(200, { csrfToken: "csrf-1" });
    if (url.endsWith("/commit")) return response(202, { id: "commit-1", state: "running", progress: {} });
    if (url.includes("/parts")) return response(200, { items: [], nextCursor: null, watermark: 0 });
    return response(200, { id: "session", state: "open", size: 0, partCount: 0 });
  });
  const intent = { path: "Health/empty +?.json", size: 0, mediaType: "application/json", condition: { mode: "replace", contentRevision: "content-r1" }, sourceModifiedAt: "2015-09-26T12:00:00Z" };
  await api.createUpload(intent, "stable-key");
  await api.uploadStatus("session +?");
  await api.uploadParts("session +?", "cursor +?");
  await api.commitUpload("session +?");
  await api.abortUpload("session +?");
  const mutations = requests.filter(({ init }) => init?.method);
  assert.deepEqual(JSON.parse(mutations[0].init.body), { ...intent, path: "/Health/empty +?.json" });
  assert.equal(mutations[0].init.headers.get("Idempotency-Key"), "stable-key");
  assert.ok(mutations.every(({ init }) => init.headers.get("X-CSRF-Token") === "csrf-1"));
  assert.ok(requests.every(({ init }) => init.credentials === "same-origin"));
  assert.equal(requests[2].url, "/api/files/uploads/session%20%2B%3F");
  assert.equal(requests[3].url, "/api/files/uploads/session%20%2B%3F/parts?cursor=cursor+%2B%3F");
  assert.equal(mutations[1].url, "/api/files/uploads/session%20%2B%3F/commit");
  assert.equal(mutations[1].init.body, undefined, "commit retries never send file bytes");
  assert.equal(mutations[2].init.method, "DELETE");
  assert.equal(FakeXHR.instances.length, 0, "zero-size control flow uses no part transport");
});
