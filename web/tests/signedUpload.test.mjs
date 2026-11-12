import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const moduleURL = (code) => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
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
  respond(status) { this.status = status; this.onload(); }
}
const grant = (overrides = {}) => ({
  number: 2, size: 3, method: "PUT",
  url: "/api/files/transfer/drive/owner/file?partNumber=2&uploadId=native&q-signature=synthetic",
  headers: { "Content-Type": "application/octet-stream", "Content-Length": "3" },
  expiresAt: new Date(Date.now() + 300000).toISOString(),
  ...overrides,
});
const response = (status, payload) => ({ status, json: async () => payload });
async function setup(t, options = {}) {
  const original = { fetch: globalThis.fetch, XMLHttpRequest: globalThis.XMLHttpRequest, window: globalThis.window };
  const calls = [];
  globalThis.window = { location: { origin: "https://app.example" } };
  FakeXHR.instances = [];
  globalThis.XMLHttpRequest = FakeXHR;
  globalThis.fetch = async (url, init) => {
    calls.push({ url, init });
    if (url === "/api/files/me") return response(200, { csrfToken: "application-csrf" });
    if (url.endsWith("/parts/sign")) return response(200, { parts: options.parts ?? [grant()] });
    assert.fail(`unexpected API request ${url}`);
  };
  t.after(() => { globalThis.fetch = original.fetch; globalThis.XMLHttpRequest = original.XMLHttpRequest; globalThis.window = original.window; });
  const client = await import(moduleURL(`${code}\n// presigned instance ${++sequence}`));
  const xhr = async () => {
    for (let i = 0; i < 100 && !FakeXHR.instances.length; i++) await new Promise((resolve) => setImmediate(resolve));
    assert.equal(FakeXHR.instances.length, 1, "one presigned byte request starts");
    return FakeXHR.instances[0];
  };
  return { ...client, calls, xhr };
}

test("presigned part sends exactly the signed URL and content type without confirming", async (t) => {
  const { api, calls, xhr } = await setup(t);
  const progress = [];
  let completed = false;
  const blob = new Blob(["abc"]);
  const request = api.uploadPresignedPart(grant(), blob, undefined, (value) => progress.push(value));
  request.then(() => { completed = true; });
  const data = await xhr();
  assert.equal(data.url, "https://app.example/api/files/transfer/drive/owner/file?partNumber=2&uploadId=native&q-signature=synthetic");
  assert.equal(data.method, "PUT");
  assert.equal(data.withCredentials, false);
  assert.deepEqual(data.headers, { "content-type": "application/octet-stream" });
  assert.equal(data.body, blob);
  assert.equal(calls.filter(({ url }) => url.includes("/parts/2/")).length, 0, "class A has no per-part confirm request");
  data.upload.onprogress({ loaded: 2 });
  assert.deepEqual(progress, [{ loaded: 2, total: 3 }]);
  assert.equal(completed, false);
  data.respond(204);
  await request;
});

test("a direct-publication part sends the conditional headers it was signed for", async (t) => {
  // The one-conditional-PUT publication binds If-None-Match and X-Cos-Forbid-Overwrite
  // into the signature. Dropping them fails every file at the provider, and refusing the
  // grant outright fails it in the browser before a byte moves — which is what a page of
  // 23,373 files did: sessions opened, no part ever sent.
  const { api, xhr } = await setup(t);
  const request = api.uploadPresignedPart(grant({
    headers: {
      "Content-Length": "3",
      "Content-Type": "application/octet-stream",
      "If-None-Match": "*",
      "X-Cos-Forbid-Overwrite": "true",
    },
  }), new Blob(["abc"]));
  const settled = request.then(() => "sent", (error) => error.message);
  const data = await xhr();
  assert.deepEqual(data.headers, {
    "content-type": "application/octet-stream",
    "if-none-match": "*",
    "x-cos-forbid-overwrite": "true",
  });
  data.respond(200);
  assert.equal(await settled, "sent");
});

test("a content-replacement part carries the If-Match it was signed for", async (t) => {
  const { api, xhr } = await setup(t);
  const request = api.uploadPresignedPart(grant({
    headers: { "Content-Length": "3", "Content-Type": "text/plain", "If-Match": '"etag"' },
  }), new Blob(["abc"]));
  const settled = request.then(() => "sent", (error) => error.message);
  const data = await xhr();
  assert.deepEqual(data.headers, { "content-type": "text/plain", "if-match": '"etag"' });
  data.respond(200);
  assert.equal(await settled, "sent");
});

test("signing requests a part window and returns the grants", async (t) => {
  const { api, calls } = await setup(t);
  const parts = await api.signUploadParts("session +?", [1, 2]);
  assert.equal(parts.length, 1);
  const sign = calls.find(({ url }) => url.includes("/parts/sign"));
  assert.equal(sign.url, "/api/files/uploads/session%20%2B%3F/parts/sign");
  assert.deepEqual(JSON.parse(sign.init.body), { numbers: [1, 2] });
  assert.equal(sign.init.headers.get("X-CSRF-Token"), "application-csrf");
});

test("presigned cancellation aborts the provider request", async (t) => {
  const { api, xhr } = await setup(t);
  const signal = new AbortController();
  const request = api.uploadPresignedPart(grant(), new Blob(["abc"]), signal.signal);
  const rejected = assert.rejects(request, { name: "AbortError" });
  const data = await xhr();
  signal.abort();
  await rejected;
  assert.equal(data.aborted, true);
});

for (const url of [
  "https://bucket.example/key?sign=x", // absolute provider URL is allowed
]) {
  test(`accept a direct provider grant ${JSON.stringify(url)}`, async (t) => {
    const { api, xhr } = await setup(t);
    const request = api.uploadPresignedPart(grant({ url }), new Blob(["abc"]));
    const data = await xhr();
    assert.equal(data.withCredentials, false);
    data.respond(200);
    await request;
  });
}

for (const [label, changes] of [
  ["fragment", { url: "/api/files/transfer/key?sign=x#fragment" }],
  ["foreign relative path", { url: "/api/files/content?sign=x" }],
  ["non-http scheme", { url: "ftp://bucket.example/key?sign=x" }],
  ["expired", { expiresAt: "2000-01-01T00:00:00Z" }],
  ["wrong method", { method: "POST" }],
  ["wrong length", { size: 4 }],
  ["authorization header", { headers: { "Content-Type": "application/octet-stream", Authorization: "app secret" } }],
  ["digest header", { headers: { "Content-Type": "application/octet-stream", "Content-MD5": "kAFQmDzST7DWlj99KOF/cg==" } }],
]) {
  test(`reject malformed presigned grant: ${label}`, async (t) => {
    const { api } = await setup(t);
    await assert.rejects(api.uploadPresignedPart(grant(changes), new Blob(["abc"])));
    assert.equal(FakeXHR.instances.length, 0);
  });
}

// A form upload is the create path when the connection signs one. Every signed value rides
// in the body, so the request carries no custom header and the browser sends no preflight:
// one round trip per small file instead of two.
const formGrant = (overrides = {}) => ({
  number: 1, size: 3, method: "POST", url: "https://bucket.cos.ap-hongkong.myqcloud.com/",
  headers: {},
  fields: [
    { name: "key", value: "drive/owner/照片.json" },
    { name: "policy", value: "eyJjb25kaXRpb25zIjpbXX0=" },
    { name: "q-sign-algorithm", value: "sha1" },
    { name: "q-ak", value: "AKIDfixture" },
    { name: "q-key-time", value: "1700000000;1700000600" },
    { name: "q-signature", value: "0123456789abcdef" },
    { name: "x-cos-forbid-overwrite", value: "true" },
    { name: "Content-Type", value: "application/json" },
    { name: "success_action_status", value: "201" },
  ],
  fileField: "file",
  expiresAt: new Date(Date.now() + 300000).toISOString(),
  ...overrides,
});

test("a form upload posts every signed field and no request header", async (t) => {
  const { api, xhr } = await setup(t);
  const blob = new Blob(["abc"]);
  const request = api.uploadPresignedPart(formGrant(), blob);
  const settled = request.then(() => "sent", (error) => error.message);
  const data = await xhr();
  assert.equal(data.method, "POST");
  assert.equal(data.url, "https://bucket.cos.ap-hongkong.myqcloud.com/");
  assert.equal(data.withCredentials, false);
  assert.deepEqual(data.headers, {}, "a form upload is simple only while it carries no header");
  assert.ok(data.body instanceof FormData);
  const entries = [...data.body.entries()];
  assert.deepEqual(entries.slice(0, -1), formGrant().fields.map(({ name, value }) => [name, value]));
  const [fileField, file] = entries.at(-1);
  assert.equal(fileField, "file");
  assert.equal(file.size, 3, "the file goes last, after every signed field");
  data.respond(201);
  assert.equal(await settled, "sent");
});

test("a refused form upload is the same name conflict the create endpoint reports", async (t) => {
  const { api, xhr } = await setup(t);
  const request = api.uploadPresignedPart(formGrant(), new Blob(["abc"]));
  const settled = request.then(() => "sent", (error) => error);
  const data = await xhr();
  data.respond(409);
  const error = await settled;
  assert.equal(error.status, 409);
  assert.equal(error.code, "name_conflict");
});

test("a form grant that drops the overwrite switch never sends", async (t) => {
  const { api } = await setup(t);
  const withoutSwitch = formGrant({ fields: formGrant().fields.filter(({ name }) => name !== "x-cos-forbid-overwrite") });
  await assert.rejects(() => api.uploadPresignedPart(withoutSwitch, new Blob(["abc"])), /Invalid form upload fields/);
  const withHeader = formGrant({ headers: { "Content-Type": "application/json" } });
  await assert.rejects(() => api.uploadPresignedPart(withHeader, new Blob(["abc"])), /no request headers/);
  const relative = formGrant({ url: "/api/files/transfer/drive/owner/file" });
  await assert.rejects(() => api.uploadPresignedPart(relative, new Blob(["abc"])), /Invalid form upload/);
  assert.equal(FakeXHR.instances.length, 0, "a broken grant fails before a byte moves");
});
