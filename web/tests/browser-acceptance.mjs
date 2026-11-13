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
  const capabilityResponse = await context.request.get(`${config.url}/api/files/capabilities`);
  assert.equal(capabilityResponse.status(), 200);
  const capabilities = await capabilityResponse.json();
  assert.equal(capabilities.uploads, true);
  assert.ok(Number.isSafeInteger(capabilities.maxPartBytes) && capabilities.maxPartBytes > 0);
  // The server publishes no per-session parallelism: the client's part pool is the only
  // bound, so the scenario is guarded by what remains true of the session it needs — room
  // for two parts, checked here and by the session's own partCount below.
  assert.ok(capabilities.maxUploadParts >= 2, "the interruption scenario needs a two-part session");
  const uploadPartBytes = capabilities.maxPartBytes;
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
  // Folder navigation keeps the in-memory queue; refresh discards it without replaying a session.
  const navigationCsrf = (await (await context.request.get(`${config.url}/api/files/me`)).json()).csrfToken;
  const navigationFolder = await context.request.post(`${config.url}/api/files`, {
    headers: { Origin: origin, "X-CSRF-Token": navigationCsrf }, data: { kind: "folder", path: "/Navigation" },
  });
  assert.ok([200, 201].includes(navigationFolder.status()));
  await page.goto(`${config.url}/files`);
  let release;
  const held = new Promise((resolve) => { release = resolve; });
  let intercepted = false;
  let interruptedSession;
  await page.route("**/api/files/uploads/*/parts/2", async (route) => {
    intercepted = true;
    interruptedSession = new URL(route.request().url()).pathname.split("/")[4];
    await held; await route.abort().catch(() => {});
  });
  const large = { name: "interrupted.bin", mimeType: "application/octet-stream", buffer: Buffer.alloc(uploadPartBytes + 1, 65) };
  await page.locator('input[type="file"]').first().setInputFiles(large);
  for (let i = 0; i < 200 && !intercepted; i++) await page.waitForTimeout(50);
  assert.equal(intercepted, true, "the second parallel part was reached");
  const interrupted = await (await context.request.get(`${config.url}/api/files/uploads/${interruptedSession}`)).json();
  assert.equal(interrupted.partSize, uploadPartBytes);
  assert.equal(interrupted.partCount, 2);
  let firstPartDurable = false;
  for (let i = 0; i < 200 && !firstPartDurable; i++) {
    const response = await context.request.get(`${config.url}/api/files/uploads/${interruptedSession}/parts`);
    assert.equal(response.status(), 200);
    firstPartDurable = (await response.json()).items.some((part) => part.number === 1);
    if (!firstPartDurable) await page.waitForTimeout(50);
  }
  assert.equal(firstPartDurable, true, "part one must have a durable receipt before refresh");
  const second = await context.newPage();
  const secondUploadRequests = [], secondDialogs = [];
  second.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/api/files/uploads")) secondUploadRequests.push(request.url());
  });
  second.on("dialog", async (dialog) => { secondDialogs.push(dialog.message()); await dialog.dismiss(); });
  await second.goto(`${config.url}/files`);
  await second.locator('input[type="file"]').first().setInputFiles(large);
  await second.getByText("This file is being uploaded in another tab. Try again after it finishes or closes.", { exact: true }).waitFor();
  assert.deepEqual(secondUploadRequests, [], "a competing tab cannot allocate, inspect or abort the active upload");
  assert.deepEqual(secondDialogs, [], "a competing tab reports its conflict inline");
  assert.equal((await (await context.request.get(`${config.url}/api/files/uploads/${interruptedSession}`)).json()).state, "open");
  await second.close();
  const createsBeforeNavigation = requests.filter((request) => request.method === "POST" && new URL(request.url).pathname === "/api/files/uploads/batch").length;
  await page.getByTitle("Navigation", { exact: true }).click();
  await page.waitForURL(`${config.url}/files/Navigation`);
  await page.getByRole("link", { name: "All files", exact: true }).click();
  await page.getByRole("button", { name: "Cancel upload of interrupted.bin", exact: true }).waitFor();
  assert.equal(requests.filter((request) => request.method === "POST" && new URL(request.url).pathname === "/api/files/uploads/batch").length, createsBeforeNavigation);
  const leavePrompt = page.waitForEvent("dialog").then(async (dialog) => {
    const text = dialog.message();
    await dialog.dismiss();
    return text;
  });
  await page.getByRole("link", { name: "Home", exact: true }).click();
  assert.match(await leavePrompt, /Uploads are not finished/);
  await page.getByRole("button", { name: "Cancel upload of interrupted.bin", exact: true }).waitFor();
  assert.equal(await page.evaluate(() => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  }), true, "an active upload requests the browser's native leave warning");
  const obsoleteKey = await page.evaluate(async () => {
    const { data } = await (await fetch("/api/auth/me")).json();
    const key = `eam.uploads.v1:${data.user.id}`;
    localStorage.setItem(key, JSON.stringify([{ id: "old-task", path: "interrupted.bin", size: 1, createKey: "old-key" }]));
    return key;
  });
  const uploadRequestsBeforeReload = requests.filter((request) => new URL(request.url).pathname.startsWith("/api/files/uploads"));
  page.once("dialog", async (dialog) => { assert.equal(dialog.type(), "beforeunload"); await dialog.accept(); });
  await page.reload();
  release();
  await page.unroute("**/api/files/uploads/*/parts/2");
  await page.locator('input[type="file"]').first().waitFor({ state: "attached" });
  await page.waitForFunction((key) => localStorage.getItem(key) === null, obsoleteKey);
  assert.equal(await page.getByRole("button", { name: "Cancel upload of interrupted.bin", exact: true }).count(), 0);
  assert.equal(await page.getByText(/Select this file again/i).count(), 0);
  assert.deepEqual(requests.filter((request) => new URL(request.url).pathname.startsWith("/api/files/uploads")), uploadRequestsBeforeReload, "refresh must not query, retry or cancel discarded upload tasks");
  assert.equal((await metadata("/interrupted.bin")).status(), 404);
  assert.equal((await (await context.request.get(`${config.url}/api/files/uploads/${interruptedSession}`)).json()).state, "open", "page dismissal does not cancel server sessions through unload requests");
  assert.equal(await page.evaluate(() => {
    const event = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(event);
    return event.defaultPrevented;
  }), false, "the fresh empty queue has no leave warning");
  const restartDialogs = [];
  const unexpectedRestartDialog = async (dialog) => { restartDialogs.push(dialog.message()); await dialog.dismiss(); };
  page.on("dialog", unexpectedRestartDialog);
  await page.locator('input[type="file"]').first().setInputFiles(large);
  await waitMetadata("/interrupted.bin");
  page.off("dialog", unexpectedRestartDialog);
  assert.deepEqual(restartDialogs, [], "new selection restarts an abandoned upload without any confirmation");
  assert.equal((await (await context.request.get(`${config.url}/api/files/uploads/${interruptedSession}`)).json()).state, "aborted");
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
  // One rejected item leaves an unrelated successful sibling visible. The refusal is the
  // server's own: a folder occupies /fail.txt, so the admission page answers that item with
  // a Problem while the sibling's item still allocates. A route stub on the single create
  // endpoint no longer intercepts anything — the client admits files in a page.
  const conflictCsrf = (await (await context.request.get(`${config.url}/api/files/me`)).json()).csrfToken;
  const occupied = await context.request.post(`${config.url}/api/files`, {
    headers: { Origin: origin, "X-CSRF-Token": conflictCsrf }, data: { kind: "folder", path: "/fail.txt" },
  });
  assert.ok([200, 201].includes(occupied.status()));
  await page.locator('input[type="file"]').first().setInputFiles([
    { name: "fail.txt", mimeType: "text/plain", buffer: Buffer.from("no") },
    { name: "sibling.txt", mimeType: "text/plain", buffer: Buffer.from("yes") },
  ]);
  await waitMetadata("/sibling.txt");
  await page.getByText(/A folder occupies the destination/).waitFor();
  assert.equal((await (await metadata("/fail.txt")).json()).kind, "folder", "the refused file never replaced the folder");
  await page.getByRole("button", { name: "Cancel upload of fail.txt", exact: true }).click();
  await page.getByText(/A folder occupies the destination/).waitFor({ state: "hidden" });
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
  console.log(JSON.stringify({ origin, requests: requests.length, checks: ["built SPA", "cookie CSRF upload", "special names", "preview", "download", "folder navigation retains uploads", "leave warning", "refresh discards uploads", "obsolete queue removal", "automatic restart without confirmation", "active tab protection", "cancel before commit", "partial failure", "exact deletion wait", "scoped Bearer same lifecycle", "same-origin network"] }));
} finally { await browser.close(); }
