/**
 * Format decisions for the HEIC preview, kept free of imports so they can be tested directly.
 */

/** Long edge of the JPEG handed to the viewer: ~2.5× a 12 MP phone photo's shorter side. */
export const maxPreviewEdge = 2560;

/** `{width, height}` fitted inside a square of `edge`, never upscaled. */
export function scaledSize(width: number, height: number, edge = maxPreviewEdge): { width: number; height: number } {
  const longest = Math.max(width, height);
  if (longest <= edge) return { width, height };
  const factor = edge / longest;
  return { width: Math.max(1, Math.round(width * factor)), height: Math.max(1, Math.round(height * factor)) };
}

/**
 * Whether these bytes are HEIF and should go through libheif.
 *
 * Matched by extension as well as MIME type: the index records `image/heic` for a normal upload,
 * but a file copied in from elsewhere can arrive with an empty or generic type while the extension
 * still says what it is.
 */
export function isHeicFile(file: { extension: string; mimeType: string }): boolean {
  if (file.extension === "heic" || file.extension === "heif") return true;
  const mime = file.mimeType.split(";", 1)[0].trim().toLowerCase();
  return mime === "image/heic" || mime === "image/heif" || mime === "image/heic-sequence" || mime === "image/heif-sequence";
}
