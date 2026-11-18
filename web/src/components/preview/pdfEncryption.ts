/**
 * Does this PDF carry a password, or did the viewer simply give up?
 *
 * The bundled PDF renderer has no place to type a password: it hands pdfjs a URL and, when pdfjs
 * refuses an encrypted file, replaces the document with a "download it and open it elsewhere" card.
 * Telling the two cases apart before mounting it is cheap — an encrypted PDF names `/Encrypt` in
 * its trailer or in its cross-reference stream, both of which sit in the last few kilobytes — while
 * asking pdfjs costs a parse of the whole document.
 *
 * The marker is deliberate proof-of-nothing: a PDF restricted only in printing and copying also has
 * one, and its user password is empty, so pdfjs opens it without ever asking. A positive answer only
 * chooses which viewer is mounted; whether a password is actually required is decided by pdfjs.
 */
export const PDF_TAIL_BYTES = 32 * 1024;

export function pdfLooksEncrypted(tail: Uint8Array): boolean {
  // The marker is ASCII, so a byte-wise scan needs no text decoder and no encoding guesses.
  const marker = [0x2f, 0x45, 0x6e, 0x63, 0x72, 0x79, 0x70, 0x74]; // "/Encrypt"
  outer: for (let i = 0; i + marker.length <= tail.length; i += 1) {
    for (let j = 0; j < marker.length; j += 1) {
      if (tail[i + j] !== marker[j]) continue outer;
    }
    return true;
  }
  return false;
}

/**
 * Read the tail of a downloadable file and answer whether it is an encrypted PDF.
 *
 * A suffix range keeps this to one small request; a server or a blob URL that ignores the range
 * answers with the whole file, which still contains the marker, so the answer stays correct at the
 * cost of the download the viewer was about to make anyway.
 */
export async function pdfTailLooksEncrypted(url: string, signal?: AbortSignal): Promise<boolean> {
  const response = await fetch(url, {
    headers: { Range: `bytes=-${PDF_TAIL_BYTES}` },
    signal,
  });
  if (!response.ok) throw new Error(`Reading the PDF tail failed with HTTP ${response.status}.`);
  return pdfLooksEncrypted(new Uint8Array(await response.arrayBuffer()));
}
