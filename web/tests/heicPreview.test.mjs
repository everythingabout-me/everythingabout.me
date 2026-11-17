import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/components/preview/heicFormat.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "heicFormat.ts");
const { isHeicFile, scaledSize } = await import(`data:text/javascript;base64,${Buffer.from(code).toString("base64")}`);

const file = (extension, mimeType) => ({ extension, mimeType });

test("every spelling of a HEIF file reaches the decoder, and nothing else does", () => {
  for (const [extension, mime] of [["heic", "image/heic"], ["heif", "image/heif"], ["bin", "image/heic-sequence"], ["", "image/heif"], ["heic", ""]]) {
    assert.equal(isHeicFile(file(extension, mime)), true, `${extension || "no extension"} / ${mime}`);
  }
  for (const [extension, mime] of [["jpg", "image/jpeg"], ["png", "image/png"], ["bin", "application/octet-stream"], ["html", "text/html"]]) {
    assert.equal(isHeicFile(file(extension, mime)), false, `${extension} / ${mime}`);
  }
  // The index records the type with parameters for some uploads.
  assert.equal(isHeicFile(file("", "image/heic; charset=binary")), true);
});

// libheif decodes at capture resolution; a 12 MP photo is 4032×3024 and needs ~98 MB as RGBA. The
// preview canvas is capped so the memory the viewer keeps is display-sized, not capture-sized.
test("the preview is scaled to the display cap and small photos are left at native size", () => {
  assert.deepEqual(scaledSize(4032, 3024), { width: 2560, height: 1920 });
  assert.deepEqual(scaledSize(3024, 4032), { width: 1920, height: 2560 });
  assert.deepEqual(scaledSize(800, 600), { width: 800, height: 600 });
  assert.deepEqual(scaledSize(2560, 2560), { width: 2560, height: 2560 });
  const panorama = scaledSize(12000, 400);
  assert.equal(Math.max(panorama.width, panorama.height), 2560);
  assert.ok(panorama.height >= 1);
});
