/**
 * Reading a ZIP archive's directory — and one of its entries — without downloading the archive.
 *
 * A file detail page exists to answer "what is in here", and for a 58 MB archive the answer is a
 * few hundred names. Fetching the whole object for that would make opening a preview cost as much
 * as downloading the file, and would fail outright above the browser's memory budget. A ZIP keeps
 * its directory in one contiguous block at the end and every entry starts at an offset the
 * directory names, so two short ranged reads answer the listing and one more answers an entry.
 *
 * This is the browser's job, not the server's, because the bytes already reach the browser through
 * a read grant: a server-side `/archive` endpoint would add an API contract and put the service in
 * the byte path of every preview, for a parse the client can do with the same guarantee. The
 * storage driver stays the authority; the grants, revisions and range handling are unchanged.
 *
 * What is deliberately not here: extraction to a filesystem (there is none in a page), multi-disk
 * archives, and compression methods other than the stored/deflate pair essentially every zip
 * uses. Those answer with a typed error the page turns into a sentence.
 */

/** Largest entry a preview will inflate. Above this a page cannot hold the result, and a
 *  decompression bomb would otherwise be limited only by the archive's declared size. */
export const MAX_ENTRY_BYTES = 128 * 1000 * 1000;

/** The central directory is held in memory to be parsed; beyond this it is not a preview. */
const MAX_CENTRAL_DIRECTORY_BYTES = 64 * 1000 * 1000;

/** A directory with more entries than this is not something a person browses in a preview. */
const MAX_ENTRIES = 200_000;

const MAX_COMMENT_BYTES = 0xffff;
const END_OF_DIRECTORY_BYTES = 22;

const LOCAL_HEADER_SIGNATURE = 0x04034b50;
const CENTRAL_HEADER_SIGNATURE = 0x02014b50;
const END_OF_DIRECTORY_SIGNATURE = 0x06054b50;
const ZIP64_END_OF_DIRECTORY_SIGNATURE = 0x06064b50;
const ZIP64_LOCATOR_SIGNATURE = 0x07064b50;

/** ZIP64 marks a field it moved to the extra block with this value. */
const ZIP64_MARKER_32 = 0xffffffff;
const ZIP64_MARKER_16 = 0xffff;
const ZIP64_EXTRA_ID = 0x0001;

/** Bit 0 of the general purpose flags: the entry is encrypted and cannot be inflated. */
const FLAG_ENCRYPTED = 0x1;
/** Bit 11: the name is UTF-8 rather than the historical OEM code page. */
const FLAG_UTF8_NAME = 0x800;

export type ZipErrorKind = "not_archive" | "corrupt" | "unsupported" | "encrypted" | "too_large" | "read_failed";

export class ZipError extends Error {
  readonly kind: ZipErrorKind;

  constructor(kind: ZipErrorKind, message: string) {
    super(message);
    this.name = "ZipError";
    this.kind = kind;
  }
}

/**
 * Bytes with an ArrayBuffer backing. The default `Uint8Array` type is `ArrayBufferLike`, which a
 * `Blob` refuses — and every array here is built over an ArrayBuffer anyway.
 */
export type Bytes = Uint8Array<ArrayBuffer>;

/** Bytes of one archive, addressed by offset. `size` is 0 only until `prepare` learns it. */
export interface ByteSource {
  readonly size: number;
  /** Learn the total size when the caller could not supply it; a no-op otherwise. */
  prepare?(): Promise<void>;
  read(offset: number, length: number, signal?: AbortSignal): Promise<Bytes>;
}

export interface ZipEntry {
  /** Path inside the archive, as the archive spells it (UTF-8 after decoding). */
  name: string;
  /** Created from a trailing slash: a folder, never previewable. */
  directory: boolean;
  /** 0 = stored, 8 = deflate. Everything else is reported as unsupported on extraction. */
  method: number;
  encrypted: boolean;
  compressedSize: number;
  uncompressedSize: number;
  localOffset: number;
}

export interface ZipDirectory {
  entries: ZipEntry[];
}

/**
 * One remote object read with HTTP range requests.
 *
 * `acceptRanges` comes from the read grant's driver: a connection that cannot serve ranges gets a
 * single full download instead, held in memory so later reads are slices. The same fallback covers
 * a provider that answers 200 to a range request, and a browser whose CORS preflight rejects the
 * `Range` header on a grant that also carries authorization.
 */
export class HttpByteSource implements ByteSource {
  private readonly url: string;
  private readonly headers: Record<string, string>;
  private readonly credentials: RequestCredentials;
  private readonly ranges: boolean;
  private whole: Bytes | null = null;
  private total: number;

  constructor(options: {
    url: string;
    headers?: Record<string, string>;
    credentials?: RequestCredentials;
    size: number;
    acceptRanges: boolean;
  }) {
    this.url = options.url;
    this.headers = options.headers ?? {};
    this.credentials = options.credentials ?? "same-origin";
    this.total = options.size;
    this.ranges = options.acceptRanges && options.size > 0;
  }

  get size(): number {
    return this.whole?.length ?? this.total;
  }

  async prepare(): Promise<void> {
    if (this.size > 0) return;
    this.whole = await this.fetchAll();
    this.total = this.whole.length;
  }

  async read(offset: number, length: number, signal?: AbortSignal): Promise<Bytes> {
    if (length <= 0) return new Uint8Array(0);
    if (this.whole) return this.whole.subarray(offset, Math.min(offset + length, this.whole.length));
    if (!this.ranges) {
      this.whole = await this.fetchAll(signal);
      this.total = this.whole.length;
      return this.whole.subarray(offset, Math.min(offset + length, this.whole.length));
    }
    try {
      const response = await fetch(this.url, {
        headers: { ...this.headers, Range: `bytes=${offset}-${offset + length - 1}` },
        credentials: this.credentials,
        signal,
      });
      if (response.status === 206) {
        const bytes = new Uint8Array(await response.arrayBuffer());
        // A short answer means the object changed under us; slicing on it would silently read
        // the wrong offsets, which is how a directory parse produces nonsense names.
        if (bytes.length < Math.min(length, Math.max(0, this.size - offset))) {
          throw new ZipError("corrupt", "The archive answered a range request with fewer bytes than requested.");
        }
        return bytes;
      }
      if (response.status === 200) {
        this.whole = new Uint8Array(await response.arrayBuffer());
        this.total = this.whole.length;
        return this.whole.subarray(offset, Math.min(offset + length, this.whole.length));
      }
      throw new ZipError("read_failed", `Reading the archive failed with HTTP ${response.status}.`);
    } catch (error) {
      // A network-level failure (CORS included) is not a reason to give up: one plain request is
      // the fallback the product would have used before ranges existed.
      if (error instanceof ZipError) throw error;
      this.whole = await this.fetchAll(signal);
      this.total = this.whole.length;
      return this.whole.subarray(offset, Math.min(offset + length, this.whole.length));
    }
  }

  private async fetchAll(signal?: AbortSignal): Promise<Bytes> {
    const response = await fetch(this.url, { headers: this.headers, credentials: this.credentials, signal });
    if (!response.ok) throw new ZipError("read_failed", `Reading the archive failed with HTTP ${response.status}.`);
    return new Uint8Array(await response.arrayBuffer());
  }
}

/** Bytes already in memory: a nested archive extracted from its parent. */
export class MemoryByteSource implements ByteSource {
  private readonly bytes: Bytes;

  constructor(bytes: Bytes) {
    this.bytes = bytes;
  }

  get size(): number {
    return this.bytes.length;
  }

  async read(offset: number, length: number): Promise<Bytes> {
    return this.bytes.subarray(offset, Math.min(offset + length, this.bytes.length));
  }
}

/** The archive's directory: every entry's name, sizes and offset, in one or two reads. */
export async function readZipDirectory(source: ByteSource, signal?: AbortSignal): Promise<ZipDirectory> {
  await source.prepare?.();
  const size = source.size;
  if (size < END_OF_DIRECTORY_BYTES) {
    throw new ZipError("not_archive", "The file is too small to be a ZIP archive.");
  }

  // The record is the last thing in the archive, followed only by a comment of at most 64 KiB,
  // and its own comment length has to reach the end of the file — that check is what makes the
  // backwards scan land on the real record even when entry data contains the same signature.
  const tailLength = Math.min(size, END_OF_DIRECTORY_BYTES + MAX_COMMENT_BYTES);
  const tailStart = size - tailLength;
  const tail = await source.read(tailStart, tailLength, signal);
  let record = -1;
  for (let index = tail.length - END_OF_DIRECTORY_BYTES; index >= 0; index -= 1) {
    if (u32(tail, index) !== END_OF_DIRECTORY_SIGNATURE) continue;
    if (index + END_OF_DIRECTORY_BYTES + u16(tail, index + 20) === tail.length) {
      record = index;
      break;
    }
  }
  if (record < 0) {
    throw new ZipError("not_archive", "The file has no ZIP directory at its end.");
  }

  const disk = u16(tail, record + 4);
  const directoryDisk = u16(tail, record + 6);
  let count = u16(tail, record + 10);
  let directorySize = u32(tail, record + 12);
  let directoryOffset = u32(tail, record + 16);
  if (disk !== 0 || directoryDisk !== 0) {
    throw new ZipError("unsupported", "A ZIP archive split across disks cannot be previewed.");
  }
  if (count === ZIP64_MARKER_16 || directorySize === ZIP64_MARKER_32 || directoryOffset === ZIP64_MARKER_32) {
    const zip64 = await readZip64EndOfDirectory(source, tailStart + record, signal);
    count = zip64.count;
    directorySize = zip64.directorySize;
    directoryOffset = zip64.directoryOffset;
  }
  if (directorySize === 0) return { entries: [] };
  if (directorySize > MAX_CENTRAL_DIRECTORY_BYTES) {
    throw new ZipError("too_large", "This archive's directory is too large to preview.");
  }
  if (directoryOffset + directorySize > size) {
    throw new ZipError("corrupt", "The archive's directory points outside the file.");
  }

  const directory = await source.read(directoryOffset, directorySize, signal);
  const entries: ZipEntry[] = [];
  let offset = 0;
  while (offset + 46 <= directory.length) {
    if (u32(directory, offset) !== CENTRAL_HEADER_SIGNATURE) break;
    const flags = u16(directory, offset + 8);
    const method = u16(directory, offset + 10);
    let compressedSize = u32(directory, offset + 20);
    let uncompressedSize = u32(directory, offset + 24);
    const nameLength = u16(directory, offset + 28);
    const extraLength = u16(directory, offset + 30);
    const commentLength = u16(directory, offset + 32);
    let localOffset = u32(directory, offset + 42);
    const end = offset + 46 + nameLength + extraLength + commentLength;
    if (end > directory.length) {
      throw new ZipError("corrupt", "The archive's directory ends in the middle of an entry.");
    }
    const rawName = directory.subarray(offset + 46, offset + 46 + nameLength);
    const extra = directory.subarray(offset + 46 + nameLength, offset + 46 + nameLength + extraLength);
    if (
      compressedSize === ZIP64_MARKER_32 ||
      uncompressedSize === ZIP64_MARKER_32 ||
      localOffset === ZIP64_MARKER_32
    ) {
      const moved = zip64EntryFields(extra, {
        uncompressedSize: uncompressedSize === ZIP64_MARKER_32,
        compressedSize: compressedSize === ZIP64_MARKER_32,
        localOffset: localOffset === ZIP64_MARKER_32,
      });
      if (moved.uncompressedSize !== undefined) uncompressedSize = moved.uncompressedSize;
      if (moved.compressedSize !== undefined) compressedSize = moved.compressedSize;
      if (moved.localOffset !== undefined) localOffset = moved.localOffset;
    }
    const name = decodeName(rawName, flags);
    entries.push({
      name,
      directory: name.endsWith("/"),
      method,
      encrypted: (flags & FLAG_ENCRYPTED) !== 0,
      compressedSize,
      uncompressedSize,
      localOffset,
    });
    if (entries.length >= MAX_ENTRIES) {
      throw new ZipError("too_large", "This archive has too many entries to preview.");
    }
    offset = end;
  }
  if (entries.length === 0 && count > 0) {
    throw new ZipError("corrupt", "The archive's directory could not be read.");
  }
  return { entries };
}

/** One entry's bytes, inflated. Bounded by MAX_ENTRY_BYTES and by the browser's decoder. */
export async function readZipEntry(source: ByteSource, entry: ZipEntry, signal?: AbortSignal): Promise<Bytes> {
  if (entry.encrypted) {
    throw new ZipError("encrypted", "This entry is encrypted.");
  }
  if (entry.method !== 0 && entry.method !== 8) {
    throw new ZipError("unsupported", `Compression method ${entry.method} cannot be read in a browser.`);
  }
  if (entry.uncompressedSize > MAX_ENTRY_BYTES) {
    throw new ZipError("too_large", "This entry is too large to preview.");
  }
  if (entry.localOffset + 30 > source.size) {
    throw new ZipError("corrupt", "This entry starts outside the archive.");
  }
  const header = await source.read(entry.localOffset, 30, signal);
  if (u32(header, 0) !== LOCAL_HEADER_SIGNATURE) {
    throw new ZipError("corrupt", "This entry's header is missing.");
  }
  // The local header repeats the name and extra block, and its lengths are the only reliable
  // way to find the data: the sizes here may be zero when a data descriptor follows the entry.
  const dataOffset = entry.localOffset + 30 + u16(header, 26) + u16(header, 28);
  if (dataOffset + entry.compressedSize > source.size) {
    throw new ZipError("corrupt", "This entry's data extends past the end of the archive.");
  }
  const compressed = entry.compressedSize > 0
    ? await source.read(dataOffset, entry.compressedSize, signal)
    : new Uint8Array(0);
  if (entry.method === 0) return compressed;
  return inflateRaw(compressed, signal);
}

async function readZip64EndOfDirectory(
  source: ByteSource,
  recordOffset: number,
  signal?: AbortSignal,
): Promise<{ count: number; directorySize: number; directoryOffset: number }> {
  if (recordOffset < 20) throw new ZipError("corrupt", "The ZIP64 locator is missing.");
  const locator = await source.read(recordOffset - 20, 20, signal);
  if (u32(locator, 0) !== ZIP64_LOCATOR_SIGNATURE) {
    throw new ZipError("corrupt", "The ZIP64 locator is missing.");
  }
  const record = await source.read(u64(locator, 8), 56, signal);
  if (u32(record, 0) !== ZIP64_END_OF_DIRECTORY_SIGNATURE) {
    throw new ZipError("corrupt", "The ZIP64 directory record is missing.");
  }
  return {
    count: u64(record, 32),
    directorySize: u64(record, 40),
    directoryOffset: u64(record, 48),
  };
}

/** Fields a ZIP64 extra block moves out of the fixed header, in the order the format stores them. */
function zip64EntryFields(
  extra: Uint8Array,
  wanted: { uncompressedSize: boolean; compressedSize: boolean; localOffset: boolean },
): { uncompressedSize?: number; compressedSize?: number; localOffset?: number } {
  let block: Uint8Array | null = null;
  for (let offset = 0; offset + 4 <= extra.length; ) {
    const id = u16(extra, offset);
    const length = u16(extra, offset + 2);
    const start = offset + 4;
    if (start + length > extra.length) break;
    if (id === ZIP64_EXTRA_ID) {
      block = extra.subarray(start, start + length);
      break;
    }
    offset = start + length;
  }
  if (!block) return {};
  const result: { uncompressedSize?: number; compressedSize?: number; localOffset?: number } = {};
  let cursor = 0;
  const take = (): number | undefined => {
    if (cursor + 8 > block.length) return undefined;
    const value = u64(block, cursor);
    cursor += 8;
    return value;
  };
  if (wanted.uncompressedSize) result.uncompressedSize = take();
  if (wanted.compressedSize) result.compressedSize = take();
  if (wanted.localOffset) result.localOffset = take();
  return result;
}

/**
 * A name is a byte string whose encoding the archive declares, not always correctly. UTF-8 is
 * tried when the flag says so, and — because many Windows tools write a code-page name without
 * setting it — also as the first guess when the flag is clear. A name that is not valid UTF-8 is
 * then read as GBK, which is what a Chinese Windows zip actually contains.
 */
function decodeName(bytes: Uint8Array, flags: number): string {
  if (bytes.length === 0) return "";
  if ((flags & FLAG_UTF8_NAME) !== 0) return decodeWith(bytes, "utf-8") ?? latin1(bytes);
  const utf8 = decodeWith(bytes, "utf-8", true);
  if (utf8 !== undefined) return utf8;
  return decodeWith(bytes, "gbk") ?? decodeWith(bytes, "gb18030") ?? latin1(bytes);
}

function decodeWith(bytes: Uint8Array, encoding: string, fatal = false): string | undefined {
  try {
    return new TextDecoder(encoding, { fatal }).decode(bytes);
  } catch {
    return undefined;
  }
}

function latin1(bytes: Uint8Array): string {
  let text = "";
  for (let index = 0; index < bytes.length; index += 1) text += String.fromCharCode(bytes[index]);
  return text;
}

/** Raw deflate, assembled with a running total so a bomb stops at the limit instead of the heap. */
async function inflateRaw(input: Bytes, signal?: AbortSignal): Promise<Bytes> {
  if (typeof DecompressionStream !== "function") {
    throw new ZipError("unsupported", "This browser cannot decompress archive entries.");
  }
  const stream = new Blob([input]).stream().pipeThrough(new DecompressionStream("deflate-raw"));
  const reader = stream.getReader();
  const chunks: Bytes[] = [];
  let total = 0;
  try {
    for (;;) {
      if (signal?.aborted) throw new DOMException("The read was cancelled.", "AbortError");
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > MAX_ENTRY_BYTES) {
        throw new ZipError("too_large", "This entry expands past the preview limit.");
      }
      chunks.push(value);
    }
  } finally {
    reader.cancel().catch(() => undefined);
  }
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

function u16(view: Uint8Array, offset: number): number {
  return view[offset] | (view[offset + 1] << 8);
}

function u32(view: Uint8Array, offset: number): number {
  return (view[offset] | (view[offset + 1] << 8) | (view[offset + 2] << 16) | (view[offset + 3] << 24)) >>> 0;
}

function u64(view: Uint8Array, offset: number): number {
  const value = new DataView(view.buffer, view.byteOffset + offset, 8).getBigUint64(0, true);
  if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new ZipError("too_large", "This archive is larger than a browser preview can address.");
  }
  return Number(value);
}
