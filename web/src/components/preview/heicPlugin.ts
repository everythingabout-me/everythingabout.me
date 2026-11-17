import type { PreviewCommand, PreviewContext, PreviewPlugin } from "@open-file-viewer/core";
import { t } from "../../i18n";
import { decodeHeic, previewEdge } from "./heicDecode";
import { isHeicFile } from "./heicFormat";

/**
 * Previews HEIC/HEIF photos by decoding them ourselves.
 *
 * The viewer's own `imagePlugin` claims `heic`, but the decoder it falls back to (`heic2any`
 * 0.0.4, bundling a libheif from 2019) rejects every real iPhone file tested — 8-bit, 10-bit,
 * grid-coded, all of them — with `ERR_LIBHEIF format not supported`, and its failure path is
 * silent: the raw HEIC is handed to an `<img>`, which Chrome cannot decode, so the user sees
 * "Image preview failed" with no idea which half failed. The decoding is therefore ours, and so
 * is the failure message.
 *
 * The decoder is `libheif-js` (libheif 1.23 compiled to WASM, LGPL-3.0 — the same licence family
 * as the `heic2any` it replaces). It is imported dynamically so this module stays cheap and the
 * 2 MB decoder only loads for a HEIC file.
 */
export function heicPlugin(): PreviewPlugin {
  return {
    name: "heic",
    match(file) {
      return isHeicFile(file);
    },
    async render(ctx: PreviewContext) {
      ctx.setLoading(true);
      try {
        const decoded = await decodeHeic(await readBytes(ctx), previewEdge);
        const url = URL.createObjectURL(decoded.blob);
        return showImage(ctx, url, decoded.width, decoded.height);
      } catch (error) {
        ctx.setError(t("preview.heicFailed"));
        throw error;
      } finally {
        ctx.setLoading(false);
      }
    },
  };
}

async function readBytes(ctx: PreviewContext): Promise<ArrayBuffer> {
  const blob = ctx.file.blob ?? (await readSource(ctx));
  return blob.arrayBuffer();
}

async function readSource(ctx: PreviewContext): Promise<Blob> {
  const source = ctx.file.source;
  if (typeof source === "string") {
    // A short-lived read grant or a blob: URL the caller already built, never a session-protected
    // page, so no credentials travel with the request.
    const response = await fetch(source, { signal: ctx.signal, credentials: "omit" });
    if (!response.ok) throw new Error(`HEIC preview read failed: HTTP ${response.status}`);
    return response.blob();
  }
  if (source instanceof Blob) return source;
  return new Blob([source], { type: "image/heic" });
}

/** The decoded photo, with the zoom and rotate commands the viewer's other image preview offers. */
function showImage(ctx: PreviewContext, url: string, width: number, height: number) {
  const stage = document.createElement("div");
  stage.style.cssText = "display:flex;align-items:center;justify-content:center;width:100%;height:100%;overflow:hidden";

  const photo = document.createElement("img");
  photo.src = url;
  photo.alt = ctx.file.name;
  photo.decoding = "async";
  photo.width = width;
  photo.height = height;
  photo.style.cssText = "max-width:100%;max-height:100%;object-fit:contain;transform-origin:center;transition:transform 120ms ease-out";
  stage.append(photo);
  ctx.viewport.append(stage);

  let scale = 1;
  let rotation = 0;
  const apply = () => {
    photo.style.transform = `scale(${scale}) rotate(${rotation}deg)`;
    ctx.toolbar?.setZoom(scale === 1 ? undefined : scale);
  };
  apply();

  const transforms: PreviewCommand[] = ["zoom-in", "zoom-out", "zoom-reset", "rotate-right"];
  return {
    canCommand: (command: PreviewCommand) => transforms.includes(command),
    command: (command: PreviewCommand) => {
      if (!transforms.includes(command)) return false;
      if (command === "zoom-in") scale = Math.min(4, Number((scale + 0.25).toFixed(2)));
      if (command === "zoom-out") scale = Math.max(0.25, Number((scale - 0.25).toFixed(2)));
      if (command === "zoom-reset") {
        scale = 1;
        rotation = 0;
      }
      if (command === "rotate-right") rotation = (rotation + 90) % 360;
      apply();
      return true;
    },
    destroy: () => {
      stage.remove();
      URL.revokeObjectURL(url);
    },
  };
}
