/**
 * Compressed files — a `.gz` or a `.zst`/`.zstd` — previewed as what is inside them.
 *
 * A single-stream compression has no directory to list (unlike a ZIP), so the only useful answer
 * to "what is this" is the decompressed content, rendered by the same plugin chain that renders
 * the file if it had been uploaded uncompressed. The name carries the inner file's kind: `x.jsonl`
 * from `x.jsonl.zstd`. That is the same rule the ZIP reader uses, and for the same reason — the
 * name is what the viewer, the plugin chain and the person holding the file all agree on, while
 * the media type a service stored is often absent or generic.
 *
 * Inflation happens in the browser, next to the read grant the page already holds: no route, no
 * documented API and no storage-driver capability is added, so the service never enters the byte
 * path of a preview (see docs/design/zip-preview.md for the same argument).
 *
 * What is deliberately not here: `.br`, `.xz`, `.bz2` and friends, which have no browser decoder
 * (each would need a wasm module for a format no user of this product produces), and `.tar`,
 * which is an archive rather than a compression. A `.tar.gz` therefore previews as a `.tar` the
 * viewer cannot open — and still downloads — and a `.tgz` is not claimed at all.
 */

import type { Bytes } from "./bytes";

export type CompressionKind = "gzip" | "zstd";

/**
 * Largest compressed object a preview will fetch.
 *
 * Inflation is single-stream, so the whole input has to be in memory before the first byte comes
 * out; the cap is what keeps a multi-gigabyte archive from turning a page turn into a download.
 * 64 MB compressed is already far more text than a person reads.
 */
export const MAX_COMPRESSED_BYTES = 64 * 1000 * 1000;

/**
 * Largest content a preview will inflate.
 *
 * The same cap and the same reason as `MAX_ENTRY_BYTES` in `zipArchive`: the result is held in
 * memory to hand to a renderer, and a decompression bomb must be bounded by a number this code
 * chose rather than by what the file claims.
 */
export const MAX_DECOMPRESSED_BYTES = 128 * 1000 * 1000;

/** A compressed file inside a compressed file is already a curiosity; past this it is a bomb. */
const MAX_LAYERS = 3;

export type CompressionErrorKind = "not_compressed" | "too_large" | "damaged" | "unsupported" | "nested";

export class CompressionError extends Error {
  readonly kind: CompressionErrorKind;

  constructor(kind: CompressionErrorKind, message: string) {
    super(message);
    this.name = "CompressionError";
    this.kind = kind;
  }
}

/** What a decoder answers: bytes, or a reason the page can turn into a sentence. */
export type InflateOutcome =
  | { ok: true; bytes: Bytes }
  | { ok: false; kind: CompressionErrorKind; detail: string };

/**
 * One decoder. `zstd` is injected (or lazily loaded) because its implementation is a dependency
 * that only a Zstandard file should download; the shape is what `zstd.inflateZstd` returns.
 */
export type Inflate = (
  bytes: Bytes,
  maxBytes: number,
  signal?: AbortSignal,
) => InflateOutcome | Promise<InflateOutcome>;

const bySuffix: Readonly<Record<string, CompressionKind>> = { gz: "gzip", zst: "zstd", zstd: "zstd" };

/** The compression this name's suffix declares, or null. */
export function compressionKind(name: string): CompressionKind | null {
  const dot = name.lastIndexOf(".");
  // A leading dot (`".gz"` as a whole name) or a trailing one is not a suffix: a hidden file named
  // `.gz` is not a gzip of anything.
  if (dot <= 0 || dot === name.length - 1) return null;
  return bySuffix[name.slice(dot + 1).toLowerCase()] ?? null;
}

/**
 * The name the same file would carry without its compression suffix: `a.jsonl.zstd` -> `a.jsonl`.
 *
 * Only a suffix this module knows is removed: `a.zip` is a file name, not a gzip of `a.zi`.
 */
export function uncompressedName(name: string): string {
  return compressionKind(name) ? name.slice(0, name.lastIndexOf(".")) : name;
}

/**
 * gzip, with the browser's own decoder. `DecompressionStream` inflates incrementally, so the cap
 * is checked as chunks arrive rather than after a result that may never fit.
 */
export const inflateGzip: Inflate = async (bytes, maxBytes, signal) => {
  if (bytes.length < 2 || bytes[0] !== 0x1f || bytes[1] !== 0x8b) {
    return { ok: false, kind: "not_compressed", detail: "The object does not start with a gzip header." };
  }
  if (typeof DecompressionStream === "undefined") {
    return { ok: false, kind: "unsupported", detail: "This browser has no DecompressionStream." };
  }
  let stream: ReadableStream<Uint8Array>;
  try {
    stream = new Blob([bytes]).stream().pipeThrough(new DecompressionStream("gzip"));
  } catch (error) {
    return { ok: false, kind: "unsupported", detail: errorMessage(error) };
  }
  const reader = stream.getReader();
  const onAbort = () => void reader.cancel().catch(() => undefined);
  signal?.addEventListener("abort", onAbort, { once: true });
  const chunks: Bytes[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      if (!value) continue;
      total += value.length;
      if (total > maxBytes) {
        void reader.cancel().catch(() => undefined);
        return { ok: false, kind: "too_large", detail: `The file expands past the ${maxBytes}-byte preview limit.` };
      }
      // `ReadableStream` types its chunks as `ArrayBufferLike`; a `DecompressionStream` chunk is
      // ArrayBuffer-backed, which is what `Bytes` narrows to (see bytes.ts).
      chunks.push(value as Bytes);
    }
  } catch (error) {
    if (signal?.aborted) return { ok: false, kind: "damaged", detail: "The preview was cancelled." };
    return { ok: false, kind: "damaged", detail: `The gzip stream could not be read: ${errorMessage(error)}` };
  } finally {
    signal?.removeEventListener("abort", onAbort);
  }
  return { ok: true, bytes: concat(chunks, total) };
};

/**
 * Inflate a name and every compression layer under it, answering the innermost name and bytes.
 *
 * `a.jsonl.zstd` answers `a.jsonl`. Nested layers are rare but cheap to follow, and the depth
 * limit is what stops a file that is only ever compressed from looping.
 */
export async function inflateChain(
  bytes: Bytes,
  name: string,
  options: { zstd?: Inflate; maxBytes?: number; signal?: AbortSignal } = {},
): Promise<{ name: string; bytes: Bytes }> {
  const maxBytes = options.maxBytes ?? MAX_DECOMPRESSED_BYTES;
  let current = { name, bytes };
  for (let layer = 0; ; layer += 1) {
    const kind = compressionKind(current.name);
    if (!kind) return current;
    if (layer >= MAX_LAYERS) {
      throw new CompressionError("nested", `The file is compressed in more than ${MAX_LAYERS} layers.`);
    }
    if (current.bytes.length > maxBytes) {
      throw new CompressionError("too_large", `The ${kind} input is larger than the ${maxBytes}-byte preview limit.`);
    }
    const inflate = kind === "zstd" ? options.zstd ?? (await import("./zstd")).inflateZstd : inflateGzip;
    const outcome = await inflate(current.bytes, maxBytes, options.signal);
    if (!outcome.ok) throw new CompressionError(outcome.kind, outcome.detail);
    current = { name: uncompressedName(current.name), bytes: outcome.bytes };
  }
}

function concat(chunks: Bytes[], total: number): Bytes {
  if (chunks.length === 1) return chunks[0];
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.length;
  }
  return out;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
