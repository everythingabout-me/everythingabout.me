import assert from "node:assert/strict";
import { createHash, webcrypto } from "node:crypto";
import { createRequire } from "node:module";
import { readFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";
import test from "node:test";
import { transformWithOxc } from "vite";

const require = createRequire(import.meta.url);
const moduleURL = (code) => `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`;
const digestSource = (await readFile(new URL("../src/api/uploadDigests.ts", import.meta.url), "utf8"))
  .replace('import("spark-md5")', `import(${JSON.stringify(pathToFileURL(require.resolve("spark-md5")).href)})`);
const digestCode = (await transformWithOxc(digestSource, "uploadDigests.ts")).code;
const source = (await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8"))
  .replace('import("./uploadDigests")', `import(${JSON.stringify(moduleURL(digestCode))})`);
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
  respond(status, xml = "") { this.status = status; this.responseText = xml; this.onload(); }
}
const sha256 = createHash("sha256").update("abc").digest("hex");
const md5 = createHash("md5").update("abc").digest("base64");
const receipt = { number: 2, size: 3, sha256, checksumSource: "client-sha256-cos-md5" };
const grant = () => ({ method: "PUT", url: "/api/files/transfer/drive/owner/file?partNumber=2&uploadId=native&q-signature=synthetic", headers: { "Content-MD5": md5, "Content-Type": "application/octet-stream" }, expiresAt: new Date(Date.now() + 300000).toISOString() });
const response = (status, payload) => ({ status, json: async () => payload });
const pendingPart = () => response(409, { status: 409, code: "part_pending", detail: "The native part is not ready." });
async function setup(t, options = {}) {
  const original = { fetch: globalThis.fetch, XMLHttpRequest: globalThis.XMLHttpRequest, crypto: globalThis.crypto };
  const calls = [];
  let confirms = 0;
  FakeXHR.instances = [];
  globalThis.XMLHttpRequest = FakeXHR;
  if (!globalThis.crypto) globalThis.crypto = webcrypto;
  globalThis.fetch = async (url, init) => {
    calls.push({ url, init });
    if (url === "/api/files/me") return response(200, { csrfToken: "application-csrf" });
    if (url.endsWith("/authorize")) return response(200, options.grant ?? grant());
    if (url.endsWith("/confirm")) {
      confirms++;
      return options.confirm ? options.confirm(confirms) : confirms === 1 ? pendingPart() : response(201, receipt);
    }
    assert.fail(`unexpected API request ${url}`);
  };
  t.after(() => { globalThis.fetch = original.fetch; globalThis.XMLHttpRequest = original.XMLHttpRequest; });
  const client = await import(moduleURL(`${code}\n// signed instance ${++sequence}`));
  const xhr = async () => {
    for (let i = 0; i < 100 && !FakeXHR.instances.length; i++) await new Promise((resolve) => setImmediate(resolve));
    assert.equal(FakeXHR.instances.length, 1, "one signed byte request starts");
    return FakeXHR.instances[0];
  };
  return { ...client, calls, xhr };
}

test("signed part sends exact digests, only approved headers, and waits for application confirmation after COS XML", async (t) => {
  const { api, calls, xhr } = await setup(t);
  const progress = [];
  let completed = false;
  const blob = new Blob(["abc"]);
  const request = api.uploadPart("session +?", 2, blob, undefined, (value) => progress.push(value), "cos-signed");
  request.then(() => { completed = true; });
  const data = await xhr();
  assert.equal(data.url, grant().url);
  assert.equal(data.method, "PUT");
  assert.equal(data.withCredentials, false);
  assert.deepEqual(data.headers, { "content-md5": md5, "content-type": "application/octet-stream" });
  assert.equal(data.body, blob);
  const authorize = calls.find(({ url }) => url.endsWith("/authorize"));
  assert.equal(authorize.url, "/api/files/uploads/session%20%2B%3F/parts/2/authorize");
  assert.deepEqual(JSON.parse(authorize.init.body), { size: 3, sha256, md5 });
  assert.equal(authorize.init.headers.get("X-CSRF-Token"), "application-csrf");
  data.upload.onprogress({ loaded: 2 });
  assert.deepEqual(progress, [{ loaded: 2, total: 3 }]);
  assert.equal(completed, false);
  data.respond(200, '<UploadPartResult><ETag>"untrusted"</ETag></UploadPartResult>');
  assert.deepEqual(await request, receipt);
  assert.equal(calls.filter(({ url }) => url.endsWith("/confirm")).length, 2);
  assert.equal(calls.at(-1).init.body, undefined, "browser ETag is never a confirmation assertion");
});

test("lost COS ACK confirms native success without resending any body", async (t) => {
  const { api, xhr, calls } = await setup(t);
  const request = api.uploadPart("session", 2, new Blob(["abc"]), undefined, undefined, "cos-signed");
  const data = await xhr();
  data.onerror();
  assert.deepEqual(await request, receipt);
  assert.equal(FakeXHR.instances.length, 1);
  assert.equal(calls.filter(({ url }) => url.endsWith("/confirm")).length, 2);
});

test("a prior unacknowledged COS success is confirmed before any new byte request", async (t) => {
  const { api, calls } = await setup(t, { confirm: () => response(200, receipt) });
  assert.deepEqual(await api.uploadPart("session", 2, new Blob(["abc"]), undefined, undefined, "cos-signed"), receipt);
  assert.equal(FakeXHR.instances.length, 0);
  assert.equal(calls.filter(({ url }) => url.endsWith("/confirm")).length, 1);
});

test("COS failure is never treated as application JSON and pending confirmation remains retryable", async (t) => {
  const { api, xhr } = await setup(t, { confirm: pendingPart });
  const request = api.uploadPart("session", 2, new Blob(["abc"]), undefined, undefined, "cos-signed");
  const rejected = assert.rejects(request, { status: 409, code: "part_pending" });
  (await xhr()).respond(403, "<Error><Code>SignatureDoesNotMatch</Code></Error>");
  await rejected;
});

for (const url of [
  "https://other.example/api/files/transfer/key?sig=x", "https://same.example/api/files/transfer/key?sig=x",
  "//other.example/api/files/transfer/key?sig=x", "/api/files/transfer/key?sig=x#fragment",
  "/api/files/transfer/../content?sig=x", "/api/files/transfer/%2e%2e/content?sig=x",
  "/api/files/transfer/\\evil?sig=x", "/api/files/content?sig=x", "/api/files/transfer/?sig=x",
  "/api/files/transfer/key", "/api/files/transfer/key?sig=bad\nvalue",
  "/api/files/transfer/key%00?sig=x", "/api/files/transfer/key%0d?sig=x", "/api/files/transfer/key%5cpath?sig=x",
]) {
  test(`reject signed data target outside strict transfer path (${JSON.stringify(url)})`, async (t) => {
    const { api } = await setup(t, { grant: { ...grant(), url } });
    await assert.rejects(api.uploadPart("session", 2, new Blob(["abc"]), undefined, undefined, "cos-signed"), /Invalid signed upload target/);
    assert.equal(FakeXHR.instances.length, 0);
  });
}

for (const changes of [
  { method: "POST" }, { expiresAt: "invalid" }, { expiresAt: "2000-01-01T00:00:00Z" },
  { headers: { ...grant().headers, Authorization: "app secret" } },
  { headers: { ...grant().headers, "X-CSRF-Token": "csrf" } },
  { headers: { ...grant().headers, "x-cos-security-token": "provider credential" } },
  { headers: { ...grant().headers, "Content-MD5": "other" } },
  { headers: { ...grant().headers, "Content-Type": "text/plain" } },
  { headers: { ...grant().headers, "content-md5": md5 } },
]) {
  test(`reject malformed signed authorization ${JSON.stringify(changes)}`, async (t) => {
    const { api } = await setup(t, { grant: { ...grant(), ...changes } });
    await assert.rejects(api.uploadPart("session", 2, new Blob(["abc"]), undefined, undefined, "cos-signed"));
    assert.equal(FakeXHR.instances.length, 0);
  });
}

test("confirmation must bind number, size, digest and checksum provenance", async (t) => {
  for (const change of [{ number: 1 }, { size: 4 }, { sha256: "bad" }, { checksumSource: undefined }]) {
    const harness = await setup(t, { confirm: () => response(200, { ...receipt, ...change }) });
    await assert.rejects(harness.api.uploadPart("session", 2, new Blob(["abc"]), undefined, undefined, "cos-signed"), /identity changed/);
    assert.equal(FakeXHR.instances.length, 0);
  }
});

test("signed cancellation aborts bytes and does not confirm late COS responses", async (t) => {
  const { api, xhr, calls } = await setup(t);
  const signal = new AbortController();
  const request = api.uploadPart("session", 2, new Blob(["abc"]), signal.signal, undefined, "cos-signed");
  const rejected = assert.rejects(request, { name: "AbortError" });
  const data = await xhr();
  signal.abort();
  await rejected;
  data.respond(200);
  assert.equal(data.aborted, true);
  assert.equal(calls.filter(({ url }) => url.endsWith("/confirm")).length, 1);
});
