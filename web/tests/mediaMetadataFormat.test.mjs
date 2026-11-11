import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/pages/mediaMetadataFormat.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "mediaMetadataFormat.ts");
const { cameraText, capturedAtText, coordinatesText, dimensionsText, durationText, exposureText, mayHaveMediaMetadata } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

test("a recorded wall clock is shown as written, whatever zone the viewer is in", () => {
  // The same digits must come back in any TZ the test happens to run under.
  assert.equal(capturedAtText("2024-05-01T10:20:30", "en"), "May 1, 2024, 10:20:30 AM");
  assert.equal(capturedAtText("2024-05-01T10:20:30+08:00", "en"), "May 1, 2024, 10:20:30 AM UTC+8");
  assert.equal(capturedAtText("2024-05-01T10:20:30-07:00", "en"), "May 1, 2024, 10:20:30 AM UTC−7");
  assert.equal(capturedAtText("2024-05-01T10:20:30+05:30", "en"), "May 1, 2024, 10:20:30 AM UTC+5:30");
  assert.equal(capturedAtText("2024-05-01T10:20:30+00:00", "en"), "May 1, 2024, 10:20:30 AM UTC");
});

test("an instant without a zone is converted to the viewer's zone", () => {
  const expected = new Intl.DateTimeFormat("en", { dateStyle: "medium", timeStyle: "medium" }).format(new Date("2024-05-01T02:20:30Z"));
  assert.equal(capturedAtText("2024-05-01T02:20:30Z", "en"), expected);
});

test("an unrecognized time is shown rather than dropped", () => {
  assert.equal(capturedAtText("sometime in May", "en"), "sometime in May");
});

test("coordinates carry their hemisphere", () => {
  assert.equal(coordinatesText(31.2304, 121.4737), "31.23040° N, 121.47370° E");
  assert.equal(coordinatesText(-33.85, -70.6667), "33.85000° S, 70.66670° W");
});

test("a model that already names its maker is not repeated", () => {
  assert.equal(cameraText("Apple", "iPhone 15 Pro"), "Apple iPhone 15 Pro");
  assert.equal(cameraText("Canon", "Canon EOS R5"), "Canon EOS R5");
  assert.equal(cameraText(null, "Pixel 8"), "Pixel 8");
  assert.equal(cameraText("Apple", null), "Apple");
  assert.equal(cameraText(null, null), null);
});

test("exposure reads the way a camera shows it", () => {
  assert.equal(
    exposureText({ fNumber: 1.78, exposureSeconds: 1 / 120, iso: 80, focalLength: 6.765, focalLength35mm: 24 }),
    "ƒ/1.8 · 1/120 s · ISO 80 · 24 mm",
  );
  assert.equal(
    exposureText({ fNumber: 8, exposureSeconds: 2.5, iso: null, focalLength: 50, focalLength35mm: null }),
    "ƒ/8 · 2.5 s · 50 mm",
  );
  assert.equal(exposureText({ fNumber: null, exposureSeconds: null, iso: null, focalLength: null, focalLength35mm: null }), null);
});

test("durations are clock style", () => {
  assert.equal(durationText(7.4), "0:07");
  assert.equal(durationText(95), "1:35");
  assert.equal(durationText(3723), "1:02:03");
});

test("dimensions", () => {
  assert.equal(dimensionsText(3024, 4032), "3024 × 4032");
});

test("only photo and video containers are worth a request", () => {
  assert.equal(mayHaveMediaMetadata("IMG_0001.HEIC", null), true);
  assert.equal(mayHaveMediaMetadata("clip.mov", "application/octet-stream"), true);
  assert.equal(mayHaveMediaMetadata("upload", "image/jpeg"), true);
  assert.equal(mayHaveMediaMetadata("notes.pdf", "application/pdf"), false);
  assert.equal(mayHaveMediaMetadata("screenshot.png", "image/png"), false);
});
