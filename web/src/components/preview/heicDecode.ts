import { scaledSize } from "./heicFormat";

/**
 * The one HEIC decoder in the app.
 *
 * Both call sites need the same three things — a 2 MB wasm module loaded once per page, decodes
 * that never overlap, and a JPEG sized for where it will be shown — so they live here instead of
 * in either caller.
 */

/** Display size for the file preview: ~2.5× a 12 MP phone photo's shorter side. */
export const previewEdge = 2560;
/** Display size for a timeline tile: covers a 2× tile without keeping a preview-sized canvas. */
export const tileEdge = 512;

interface HeifImage {
  get_width(): number;
  get_height(): number;
  display(data: ImageData, done: (ok: boolean) => void): void;
}

interface HeifDecoder {
  HeifDecoder: new () => { decode(bytes: Uint8Array): HeifImage[] };
}

interface DecodedPhoto {
  blob: Blob;
  width: number;
  height: number;
}

let decoder: Promise<HeifDecoder> | undefined;

function loadDecoder(): Promise<HeifDecoder> {
  // The ESM entry exports the Emscripten factory; the wasm binary is inlined in the bundle, so
  // there is no separate asset to serve. Instantiate once: a second copy is another 2 MB parse
  // and another heap.
  decoder ??= import("libheif-js/libheif-wasm/libheif-bundle.mjs").then(
    (module) => (module.default as () => Promise<HeifDecoder>)(),
  );
  return decoder;
}

// libheif can only hand back full-resolution RGBA — ~98 MB for a 12 MP photo — and has no scaled
// decode, so decodes run one at a time. A card with four HEIC tiles must not hold four of those,
// and neither must a phone whose whole tab is the limit. Queue order is arrival order, which is
// also the order the tiles became visible.
let queue: Promise<unknown> = Promise.resolve();

function serialized<T>(task: () => Promise<T>): Promise<T> {
  const result = queue.then(task, task);
  queue = result.catch(() => undefined);
  return result;
}

/** HEIC bytes -> a JPEG no larger than `maxEdge`, drawn at `quality`. */
export function decodeHeic(bytes: ArrayBuffer, maxEdge: number, quality = 0.86): Promise<DecodedPhoto> {
  return serialized(async () => {
    const libheif = await loadDecoder();
    const image = new libheif.HeifDecoder().decode(new Uint8Array(bytes))[0];
    if (!image) throw new Error("HEIC decode failed: libheif decoded no image");

    const width = image.get_width();
    const height = image.get_height();
    const source = new OffscreenCanvas(width, height).getContext("2d");
    if (!source) throw new Error("HEIC decode failed: no 2D context");
    const frame = source.createImageData(width, height);
    await new Promise<void>((resolve, reject) => {
      image.display(frame, (ok) => (ok ? resolve() : reject(new Error("HEIC decode failed: libheif refused to display"))));
    });
    source.putImageData(frame, 0, 0);

    const scaled = scaledSize(width, height, maxEdge);
    const target = new OffscreenCanvas(scaled.width, scaled.height);
    const context = target.getContext("2d");
    if (!context) throw new Error("HEIC decode failed: no 2D context");
    context.drawImage(source.canvas, 0, 0, scaled.width, scaled.height);
    return { blob: await target.convertToBlob({ type: "image/jpeg", quality }), ...scaled };
  });
}

/**
 * A URL (read grant or blob:) -> an object URL of the decoded JPEG.
 *
 * The caller owns the object URL and must revoke it. An abort stops the download; a decode that
 * already started still finishes inside the queue, which is why the caller checks the signal again
 * before using the result.
 */
export async function decodeHeicUrl(source: string, maxEdge: number, signal?: AbortSignal): Promise<string> {
  const response = await fetch(source, { signal, credentials: "omit" });
  if (!response.ok) throw new Error(`HEIC read failed: HTTP ${response.status}`);
  const decoded = await decodeHeic(await response.arrayBuffer(), maxEdge);
  return URL.createObjectURL(decoded.blob);
}
