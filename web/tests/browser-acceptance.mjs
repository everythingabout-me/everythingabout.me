import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";

let input = "";
for await (const chunk of process.stdin) input += chunk;
const config = JSON.parse(input);
const { chromium } = await import(pathToFileURL(config.module).href);
const browser = await chromium.launch({ executablePath: config.chrome || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", headless: true });
try {
  const context = await browser.newContext({ locale: "en-US" });
  await context.addCookies([{ name: "eam_session", value: config.cookie, url: config.url, httpOnly: true, sameSite: "Lax" }]);
  const page = await context.newPage();
  const requests = [];
  const problems = [];
  const origin = new URL(config.url).origin;
  page.on("request", (request) => requests.push({ url: request.url(), method: request.method() }));
  page.on("pageerror", (error) => problems.push(error.message));
  page.on("response", async (response) => {
    if (response.status() >= 400 && response.request().method() !== "GET") console.error("HTTP problem", response.status(), new URL(response.url()).pathname, await response.text().catch(() => ""));
  });
  await page.goto(`${config.url}/files`);
  await page.locator('input[type="file"]').first().waitFor({ state: "attached" });
  // Normal upload uses a real File input, real CSRF/bootstrap and P5 bytes.
  await page.locator('input[type="file"]').first().setInputFiles({ name: "hello +?#%.txt", mimeType: "text/plain", buffer: Buffer.from("browser upload\n") });
  const metadata = async (path) => context.request.get(`${config.url}/api/files?${new URLSearchParams({ path })}`);
  const waitMetadata = async (path) => {
    let last;
    for (let attempt = 0; attempt < 200; attempt++) {
      last = await metadata(path);
      if (last.ok()) return last.json();
      await page.waitForTimeout(100);
    }
    throw new Error(`Metadata did not settle: ${path}: ${await last.text()}`);
  };
  assert.equal((await waitMetadata("/hello +?#%.txt")).size, 15);
  await page.goto(`${config.url}/files/${encodeURIComponent("hello +?#%.txt")}`);
  await page.getByRole("heading", { name: "hello +?#%.txt" }).waitFor();
  const raw = await context.request.get(`${config.url}/api/files/content?${new URLSearchParams({ path: "/hello +?#%.txt" })}`);
  assert.equal(await raw.text(), "browser upload\n");
  assert.equal(raw.headers()["x-content-type-options"], "nosniff");
  // One acknowledged part survives page destruction. Resume requires a local File reselection.
  await page.goto(`${config.url}/files`);
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let intercepted = false;
  await page.route("**/api/files/uploads/*/parts/2", async (route) => { intercepted = true; await held; await route.abort().catch(() => {}); });
  const large = { name: "resume.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(9 * 1024 * 1024, 65) };
  await page.locator('input[type="file"]').first().setInputFiles(large);
  await page.waitForFunction(() => Object.keys(localStorage).some((key) => key.startsWith("eam.uploads.v1:") && localStorage.getItem(key).includes("resume.bin")));
  for (let i = 0; i < 200 && !intercepted; i++) await page.waitForTimeout(50);
  assert.equal(intercepted, true, "the second part was reached after the first durable ACK");
  release();
  await page.reload();
  await page.unroute("**/api/files/uploads/*/parts/2");
  await page.getByText(/Select this file again/i).waitFor();
  const before = requests.filter((request) => /\/parts\/1$/.test(new URL(request.url).pathname)).length;
  await page.locator('input[type="file"]').first().setInputFiles(large);
  await waitMetadata("/resume.bin");
  const after = requests.filter((request) => /\/parts\/1$/.test(new URL(request.url).pathname)).length;
  assert.equal(after, before, "refresh resume must query receipts and skip acknowledged part one");
  // Safe raster preview stays at this origin and performs no provider image transform.
  const image = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l9sAAAAASUVORK5CYII=", "base64");
  await page.locator('input[type="file"]').first().setInputFiles({ name: "pixel.png", mimeType: "image/png", buffer: image });
  await waitMetadata("/pixel.png");
  await page.goto(`${config.url}/files/pixel.png`);
  await page.waitForFunction(() => [...document.images].some((image) => image.src.includes('/api/files/content?')));
  const sources = await page.locator("img,video,audio,source").evaluateAll((elements) => elements.map((element) => element.src).filter(Boolean));
  for (const source of sources) assert.equal(new URL(source).origin, origin);
  // Cancel a real allocation before its first part can reach the server.
  await page.goto(`${config.url}/files`);
  let cancelSession;
  let releaseCancel;
  const cancelHold = new Promise((resolve) => { releaseCancel = resolve; });
  await page.route("**/api/files/uploads/*/parts/1", async (route) => {
    cancelSession = new URL(route.request().url()).pathname.split("/")[4];
    await cancelHold; await route.abort().catch(() => {});
  });
  await page.locator('input[type="file"]').first().setInputFiles({ name: "cancel.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(1024) });
  for (let i = 0; i < 200 && !cancelSession; i++) await page.waitForTimeout(25);
  assert.ok(cancelSession);
  await page.getByRole("button", { name: "Cancel upload of cancel.bin", exact: true }).click();
  releaseCancel(); await page.unroute("**/api/files/uploads/*/parts/1");
  for (let i = 0; i < 100; i++) {
    const current = await (await context.request.get(`${config.url}/api/files/uploads/${cancelSession}`)).json();
    if (current.state === "aborted") break;
    await page.waitForTimeout(50);
  }
  assert.equal((await (await context.request.get(`${config.url}/api/files/uploads/${cancelSession}`)).json()).state, "aborted");
  assert.equal((await metadata("/cancel.bin")).status(), 404);
  // One rejected file leaves an unrelated successful sibling visible.
  await page.route("**/api/files/uploads", async (route) => {
    if (route.request().method() === "POST" && route.request().postDataJSON()?.path === "/fail.txt") {
      await route.fulfill({ status: 507, contentType: "application/problem+json", body: JSON.stringify({ type: "urn:eam:error:staging_full", title: "Staging full", status: 507, code: "staging_full", detail: "Injected staging limit", requestId: "browser-fixture" }) });
    } else await route.continue();
  });
  await page.locator('input[type="file"]').first().setInputFiles([
    { name: "fail.txt", mimeType: "text/plain", buffer: Buffer.from("no") },
    { name: "sibling.txt", mimeType: "text/plain", buffer: Buffer.from("yes") },
  ]);
  await waitMetadata("/sibling.txt");
  await page.getByText("Injected staging limit", { exact: true }).waitFor();
  assert.equal((await metadata("/fail.txt")).status(), 404);
  await page.getByRole("button", { name: "Cancel upload of fail.txt", exact: true }).click();
  await page.getByText("Injected staging limit", { exact: true }).waitFor({ state: "hidden" });
  await page.unroute("**/api/files/uploads");
  // Keep an exact deletion status pending even after native deletion settles.
  // The UI must wait for that identity before allocating/sending replacement bytes.
  await page.goto(`${config.url}/files/pixel.png`);
  let deleteId;
  let allowDeleteSuccess = false;
  await page.route("**/api/files?*", async (route) => {
    if (route.request().method() !== "DELETE") return route.continue();
    const response = await route.fetch();
    assert.equal(response.status(), 202);
    const op = await response.json(); deleteId = op.id;
    await route.fulfill({ response });
  });
  await page.route("**/api/files/operations/*", async (route) => {
    if (new URL(route.request().url()).pathname.endsWith(`/${deleteId}`) && !allowDeleteSuccess) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ id: deleteId, kind: "delete", state: "running", progress: { completed: 0, total: 1, unit: "entries" }, result: null, error: null, cancelRequested: false }) });
    }
    await route.continue();
  });
  await page.getByRole("button", { name: "Delete file", exact: true }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete", exact: true }).click();
  await page.waitForURL(`${config.url}/files`);
  const putBeforeDeleteWait = requests.filter((request) => request.method === "PUT").length;
  await page.locator('input[type="file"]').first().setInputFiles({ name: "pixel.png", mimeType: "image/png", buffer: image });
  await page.waitForTimeout(300);
  assert.equal(requests.filter((request) => request.method === "PUT").length, putBeforeDeleteWait);
  allowDeleteSuccess = true;
  await waitMetadata("/pixel.png");
  assert.ok(deleteId);
  await page.unroute("**/api/files?*"); await page.unroute("**/api/files/operations/*");
  // An independently scoped Bearer performs the same create/part/commit/read lifecycle.
  const csrf = (await (await context.request.get(`${config.url}/api/files/me`)).json()).csrfToken;
  let response = await context.request.post(`${config.url}/api/files`, { headers: { Origin: origin, "X-CSRF-Token": csrf }, data: { kind: "folder", path: "/Scope" } });
  assert.ok([200,201].includes(response.status()));
  const headers = { Authorization: `Bearer ${config.bearer}` };
  const outside = await context.request.get(`${config.url}/api/files?path=%2Fpixel.png`, { headers });
  assert.equal(outside.status(), 404);
  response = await context.request.post(`${config.url}/api/files/uploads`, { headers: { ...headers, "Idempotency-Key": "browser-bearer-same-protocol" }, data: { path: "/Scope/bearer.txt", size: 3, mediaType: "text/plain" } });
  assert.equal(response.status(), 201); const session = await response.json();
  response = await context.request.put(`${config.url}/api/files/uploads/${session.id}/parts/1`, { headers, data: Buffer.from("abc") }); assert.equal(response.status(), 201);
  response = await context.request.post(`${config.url}/api/files/uploads/${session.id}/commit`, { headers }); assert.ok([200, 202].includes(response.status()));
  await waitMetadata("/Scope/bearer.txt");
  const bearerContent = await context.request.get(`${config.url}/api/files/content?path=%2FScope%2Fbearer.txt`, { headers }); assert.equal(await bearerContent.text(), "abc");
  for (const request of requests) {
    assert.equal(new URL(request.url).origin, origin, `cross-origin request: ${request.url}`);
    assert.equal(request.url.includes("/api/fs/"), false);
    assert.equal(/imageMogr2|cos\.[^/]*myqcloud|upload\/sign|upload\/confirm/.test(request.url), false);
  }
  assert.deepEqual(problems, []);
  console.log(JSON.stringify({ origin, requests: requests.length, checks: ["built SPA", "cookie CSRF upload", "special names", "preview", "download", "refresh receipt recovery", "reselect skip confirmed part", "cancel before commit", "partial failure", "exact deletion wait", "scoped Bearer same lifecycle", "same-origin network"] }));
} finally { await browser.close(); }
