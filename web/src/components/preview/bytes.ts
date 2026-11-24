/**
 * Bytes with an ArrayBuffer backing.
 *
 * The default `Uint8Array` type is `Uint8Array<ArrayBufferLike>`, which a `Blob` — and therefore
 * the viewer, the download path and every `BlobPart` — refuses at the type level. Every buffer the
 * preview creates is a plain `ArrayBuffer`, so the narrower alias is the honest one, and it lives
 * here because the archive reader, the compression reader and the Zstandard decoder all hand bytes
 * to the same consumer.
 */
export type Bytes = Uint8Array<ArrayBuffer>;
