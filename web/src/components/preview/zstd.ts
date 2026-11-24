/**
 * Zstandard, inflated in the browser.
 *
 * The browser's own `DecompressionStream` speaks gzip, deflate and deflate-raw — not Zstandard —
 * so this is the one decoder the preview carries as a dependency (`fzstd`, pure JavaScript, no
 * wasm). It is imported dynamically, so a page that previews an image never downloads it.
 *
 * The frame header is parsed here before the decoder sees the bytes, because `fzstd` sizes its
 * output from the frame: a header that declares gigabytes would be allocated in full, and a
 * download is a more useful answer than a frozen tab. This is a bound on what the file *claims*,
 * not on what the user may open — the same file still downloads.
 *
 * This module deliberately imports nothing but the decoder: it is loaded as its own chunk, and
 * `compression.ts` owns the error vocabulary (the two outcome shapes are structurally identical,
 * `InflateOutcome` being the name it gives them).
 */

import { decompress } from "fzstd";
import type { Bytes } from "./bytes";

export type FailureKind = "not_compressed" | "too_large" | "damaged";

export type InflateOutcome =
  | { ok: true; bytes: Bytes }
  | { ok: false; kind: FailureKind; detail: string };

/** Little-endian magic of a Zstandard frame: `28 B5 2F FD`. */
const ZSTD_MAGIC = 0xfd2fb528;
/** `50 2A 4D 18` with the low nibble free: a skippable frame (seeking tables, padding). */
const SKIPPABLE_MAGIC = 0x184d2a50;
const SKIPPABLE_MASK = 0xfffffff0;
/** Skippable frames a real file may put before the data; more than this is not a zstd file. */
const MAX_SKIPPABLE = 4;

/**
 * Largest window a frame that declares no content size may ask for.
 *
 * `fzstd` sizes its output buffer from the frame, and when the content size is absent it uses the
 * window instead — which is a claim about how far back the blocks refer, not about how much data
 * they produce. A streamed file can therefore declare a window far larger than its contents (the
 * zstd CLI at level 3 declares 2 MiB; Node's streaming compressor declares 128 MiB), so this is a
 * memory bound on the decoder rather than a judgement about the file: past it, downloading is the
 * honest answer.
 */
const MAX_WINDOW_BYTES = 256 * 1000 * 1000;

export function inflateZstd(bytes: Bytes, maxBytes: number): InflateOutcome {
  const header = frameHeader(bytes);
  if (!header) {
    return { ok: false, kind: "not_compressed", detail: "The object does not start with a Zstandard frame." };
  }
  // `fzstd` sizes its output from the frame before decoding anything, so a claim past the limit is
  // refused here rather than after the allocation. A declared content size is exactly the output;
  // without one, the window is what gets allocated.
  if (header.contentSize !== null) {
    if (header.contentSize > maxBytes) {
      return {
        ok: false,
        kind: "too_large",
        detail: `The frame declares ${header.contentSize} bytes, past the ${maxBytes}-byte preview limit.`,
      };
    }
  } else if (header.windowSize > MAX_WINDOW_BYTES) {
    return {
      ok: false,
      kind: "too_large",
      detail: `The frame declares a ${header.windowSize}-byte window and no content size; decoding it would allocate that much.`,
    };
  }
  let output: Bytes;
  try {
    // `fzstd` allocates plain `Uint8Array`s and types them as `ArrayBufferLike`; the backing
    // store is an ArrayBuffer, which is what `Bytes` narrows to (see bytes.ts).
    output = decompress(bytes) as Bytes;
  } catch (error) {
    return { ok: false, kind: "damaged", detail: `The Zstandard stream could not be decoded: ${message(error)}` };
  }
  if (output.length > maxBytes) {
    return { ok: false, kind: "too_large", detail: `The file expands to ${output.length} bytes, past the preview limit.` };
  }
  return { ok: true, bytes: output };
}

interface FrameHeader {
  /** Bytes the frame says it will produce, when the header carries it. */
  contentSize: number | null;
  /** The window the decoder needs to keep; also the allocation `fzstd` makes when no size is known. */
  windowSize: number;
}

/**
 * The first frame's header, skipping any skippable frames in front of it. Null when the bytes are
 * not a Zstandard frame at all.
 */
function frameHeader(bytes: Bytes): FrameHeader | null {
  let at = 0;
  for (let skipped = 0; skipped <= MAX_SKIPPABLE; skipped += 1) {
    // A frame header is five bytes; a skippable one needs its four-byte size as well.
    if (at + 5 > bytes.length) return null;
    const magic = readInt(bytes, at, 4);
    if ((magic & SKIPPABLE_MASK) >>> 0 === SKIPPABLE_MAGIC) {
      if (at + 8 > bytes.length) return null;
      at += 8 + readInt(bytes, at + 4, 4);
      continue;
    }
    if (magic !== ZSTD_MAGIC) return null;
    return parseFrame(bytes, at);
  }
  return null;
}

function parseFrame(bytes: Bytes, at: number): FrameHeader | null {
  if (at + 5 > bytes.length) return null;
  const descriptor = bytes[at + 4];
  // Bit 3 is reserved and must be zero; a set bit means this is not a frame layout we can read.
  if (descriptor & 0x08) return null;
  const singleSegment = (descriptor >> 5) & 1;
  const dictionaryFlag = descriptor & 3;
  const contentFlag = descriptor >> 6;
  let cursor = at + 5;
  let windowSize = 0;
  if (!singleSegment) {
    if (cursor >= bytes.length) return null;
    const windowDescriptor = bytes[cursor];
    cursor += 1;
    const base = 2 ** (10 + (windowDescriptor >> 3));
    windowSize = base + (base / 8) * (windowDescriptor & 7);
  }
  cursor += dictionaryFlag === 3 ? 4 : dictionaryFlag;
  const sizeBytes = contentFlag ? 1 << contentFlag : singleSegment ? 1 : 0;
  if (cursor + sizeBytes > bytes.length) return null;
  let contentSize: number | null = null;
  if (sizeBytes > 0) {
    // `contentFlag == 1` stores size - 256 in two bytes; the other widths are direct.
    contentSize = readInt(bytes, cursor, sizeBytes) + (contentFlag === 1 ? 256 : 0);
    if (singleSegment) windowSize = contentSize;
  }
  return { contentSize, windowSize };
}

function readInt(bytes: Bytes, at: number, length: number): number {
  let value = 0;
  for (let i = length - 1; i >= 0; i -= 1) value = value * 256 + bytes[at + i];
  return value;
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
