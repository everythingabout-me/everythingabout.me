/**
 * How many rows the listing mounts before a sentinel asks for the next chunk.
 *
 * A folder can hold thousands of entries, and every row is a link with a glyph, a name,
 * a size and a time. Mounting all of them is one React element and one DOM node per
 * entry before the first screen can be used. A chunk of 100 bounds that first paint (and
 * every later render) to a fixed number of rows while still filling several screens, so
 * scrolling finds the next chunk already mounted instead of firing one request per row.
 *
 * The bound is on DOM nodes, not on memory: the fetched items stay because scrolling
 * back up has to be instant and the folder may still be streaming pages.
 *
 * This is the chunked-sentinel tradeoff rather than virtualisation. Mounted rows stay in
 * normal document flow, so focus order, text selection, scroll anchoring and the list's
 * real links keep working without a library or a second scroll container. The cost is
 * that only the mounted prefix exists in the DOM: browser find (Ctrl+F) and the
 * accessibility tree do not see a row past the last chunk until the sentinel has mounted
 * it, and selecting across an unmounted boundary is impossible. A windowing library
 * would hold the same bound more tightly and make find-in-page strictly worse.
 */
export const LIST_CHUNK_SIZE = 100;

/** Rows to mount from `rendered`, clamped to the number the list actually has. */
export function visibleRowCount(total: number, rendered: number): number {
  if (!Number.isFinite(total) || total <= 0) return 0;
  return Math.max(0, Math.min(Math.floor(total), Math.floor(rendered)));
}

/** Grow by one bounded chunk, never past the end of the list. */
export function growRowCount(rendered: number, total: number, chunk = LIST_CHUNK_SIZE): number {
  return visibleRowCount(total, rendered + Math.max(1, Math.floor(chunk)));
}

/** The entries this render is allowed to mount. */
export function chunkRows<T>(rows: T[], rendered: number): T[] {
  return rows.slice(0, visibleRowCount(rows.length, rendered));
}
