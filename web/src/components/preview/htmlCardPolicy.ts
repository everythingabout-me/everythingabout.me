/** Sandboxed event HTML may embed local representations, never remote media or fonts. */
export function htmlCardPolicy(origin: string): string {
  const source = new URL(origin).origin;
  return `default-src 'none'; img-src ${source} data: blob:; media-src ${source} data: blob:; style-src 'unsafe-inline'; font-src data:`;
}
