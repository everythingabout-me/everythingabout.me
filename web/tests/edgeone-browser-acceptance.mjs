import assert from "node:assert/strict";
import { pathToFileURL } from "node:url";

let input = "";
for await (const chunk of process.stdin) input += chunk;
const config = JSON.parse(input);
const { chromium } = await import(pathToFileURL(config.module).href);
const browser = await chromium.launch({ executablePath: config.chrome || "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", headless: true });
try {
  const context = await browser.newContext();
  await context.addCookies([{ name: "eam_session", value: config.cookie, url: config.origin, httpOnly: true, sameSite: "Lax" }]);
  const page = await context.newPage();
  const requests = [];
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/api/files/transfer/")) requests.push(request);
  });
  await page.goto(`${config.origin}/edgeone-browser-probe`);
  const result = await page.evaluate(async ({ grant, data }) => {
    const bytes = Uint8Array.from(atob(data), (character) => character.charCodeAt(0));
    const body = new Blob([bytes], { type: "application/octet-stream" });
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open(grant.method, grant.url);
      xhr.withCredentials = false;
      // Content-Length is intentionally absent: the browser owns this forbidden
      // header and must derive exactly the length bound in the COS signature.
      for (const [name, value] of Object.entries(grant.headers)) xhr.setRequestHeader(name, value);
      xhr.onload = () => resolve({ status: xhr.status, bytes: body.size });
      xhr.onerror = () => reject(new Error("Signed browser part transport failed"));
      xhr.send(body);
    });
  }, { grant: config.grant, data: config.data });
  assert.equal(result.status, config.expectedStatus);
  assert.equal(result.bytes, config.size);
  assert.equal(requests.length, 1, "the real browser must issue one signed PUT without cross-origin redirection");
  const request = requests[0];
  assert.equal(request.method(), "PUT");
  assert.equal(request.url(), new URL(config.grant.url, config.origin).href);
  assert.equal(new URL(request.url()).origin, config.origin);
  const headers = await request.allHeaders();
  assert.equal(headers["content-length"], String(config.size), "browser derived the exact signed part length");
  assert.equal(headers["content-md5"], config.grant.headers["Content-MD5"]);
  assert.ok(headers.cookie?.includes("eam_session="), "fixture exercises EO cookie removal on the actual wire");
  console.log(JSON.stringify({ checks: ["real Chromium XMLHttpRequest", "same-origin signed PUT", "browser-derived Content-Length", "Content-MD5", "lost response status"], size: config.size, status: result.status }));
} finally {
  await browser.close();
}
