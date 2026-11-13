import type { Crumb, Item } from "../api/client";
import { LIST_CHUNK_SIZE, growRowCount, visibleRowCount } from "./listChunk";

/**
 * The rows on screen and the request that produced them.
 *
 * These cells were eight independent `useState`s, and every listing path wrote them in a
 * different order: a first load, a search, a continuation page, a settlement and a delete
 * each touched a different subset, so "which of `items`, `loadedPath`, `activeQuery` and
 * `loading` are true together" could only be answered by reading the whole page. One
 * reducer makes that answer a single transition, and makes the transitions testable
 * without a DOM.
 *
 * Two identities matter and are deliberately separate:
 * - `path` is the listing the rows belong to ("" is the root); it changes only when an
 *   answer arrives, so a navigation keeps the outgoing rows on screen while it loads.
 * - `query` is the committed search keyword; a search result stands in for the folder
 *   listing and must never be mixed with a folder settlement.
 */
export interface ListingState {
  /** The path whose rows are on screen, or null before the first answer. */
  path: string | null;
  items: Item[];
  /** The file or folder the path names; null when the path does not exist yet. */
  current: Item | null;
  crumbs: Crumb[];
  loading: boolean;
  /** Why the last load failed; null while it has not. */
  error: string | null;
  /** The committed search, or "" when the rows are the folder listing. */
  query: string;
  /** The text in the search box, committed or not. */
  search: string;
  /**
   * Rows mounted for this listing. The sentinel grows it by one chunk; a different
   * listing (path or committed query) starts over from one chunk in the same transition,
   * so the new listing can never mount the old one's row count even for a frame.
   */
  rows: number;
}

export type ListingAction =
  /** A request started: the outgoing rows stay until it answers. */
  | { type: "load" }
  | { type: "loaded"; path: string; items: Item[]; current: Item | null; crumbs: Crumb[] }
  | { type: "failed"; path: string; error: string }
  | { type: "settled" }
  /** A continuation page of the listing already on screen. */
  | { type: "page"; items: Item[] }
  | { type: "search-text"; value: string }
  | { type: "searched"; path: string; query: string; items: Item[]; crumbs: Crumb[] }
  /** A file that settled into the open folder. */
  | { type: "item"; item: Item }
  /** A deleted path, and its subtree when it was a folder. */
  | { type: "remove"; path: string; folder: boolean }
  /** The open folder's new revision after a settle or a refused mutation. */
  | { type: "revision"; revision: string }
  | { type: "grow-rows"; total: number }
  | { type: "all-rows"; total: number };

export function initialListingState(): ListingState {
  // The first paint is a skeleton: nothing has answered yet.
  return { path: null, items: [], current: null, crumbs: [], loading: true, error: null, query: "", search: "", rows: LIST_CHUNK_SIZE };
}

/** The identity the row chunk and the fade key follow. */
export function listingKey(state: ListingState): string {
  return `${state.path ?? ""}|${state.query}`;
}

/** A local trail for the first paint, and for a path the API has no breadcrumb for yet.
 *
 * The root crumb is the API's own empty name; the page names it, because "all files" is
 * product copy and this module holds no copy.
 */
export function crumbsForPath(path: string): Crumb[] {
  const trail: Crumb[] = [{ name: "", path: "/" }];
  let cursor = "";
  for (const segment of path.split("/")) {
    if (!segment) continue;
    cursor += `/${segment}`;
    trail.push({ name: segment, path: cursor });
  }
  return trail;
}

/** One level below `dir`; the root has no trailing slash, so it is the empty string. */
export function joinPath(dir: string, name: string): string {
  return dir ? `${dir}/${name}` : name;
}

/** The listing order: folders first, then case-insensitive name, exact spelling as tie-break. */
export function compareNames(a: string, b: string): number {
  const left = a.toLowerCase();
  const right = b.toLowerCase();
  if (left === right) return a < b ? -1 : a > b ? 1 : 0;
  return left < right ? -1 : 1;
}

export function compareItems(a: Item, b: Item): number {
  if (a.kind !== b.kind) return a.kind === "folder" ? -1 : 1;
  return compareNames(a.name, b.name);
}

/**
 * Append a streamed page to the rows already on screen.
 *
 * The keyset cursor should never repeat an entry, but an upload can settle into the
 * folder while the rest loads, and a retried page can overlap; replacing by ID keeps a
 * newer revision without moving the row or duplicating a key.
 */
export function mergeListingItems(previous: Item[], page: Item[]): Item[] {
  if (page.length === 0) return previous;
  const merged = new Map(previous.map((item) => [item.id, item] as const));
  for (const item of page) merged.set(item.id, item);
  return [...merged.values()];
}

/** A new identity starts from one chunk; the same identity keeps what is mounted. */
function withRows(state: ListingState, next: ListingState): ListingState {
  return listingKey(next) === listingKey(state) ? next : { ...next, rows: LIST_CHUNK_SIZE };
}

export function listingReducer(state: ListingState, action: ListingAction): ListingState {
  switch (action.type) {
    case "load":
      return { ...state, loading: true, error: null };
    case "loaded":
      return withRows(state, {
        ...state,
        path: action.path,
        items: action.items,
        current: action.current,
        crumbs: action.crumbs,
        loading: false,
        error: null,
        // A folder listing answers the question; a search box over it starts empty,
        // exactly as it did when the page cleared the same two cells by hand.
        query: "",
        search: "",
      });
    case "failed":
      // The path is recorded so the retry names it, but the committed search and its text
      // stay: a failed refresh is not a decision to leave the search.
      return withRows(state, { ...state, path: action.path, loading: false, error: action.error });
    case "settled":
      return state.loading ? { ...state, loading: false } : state;
    case "page":
      return { ...state, items: mergeListingItems(state.items, action.items) };
    case "search-text":
      return { ...state, search: action.value };
    case "searched":
      return withRows(state, {
        ...state,
        path: action.path,
        items: action.items,
        crumbs: action.crumbs,
        loading: false,
        error: null,
        query: action.query,
      });
    case "item": {
      const next = state.items.filter((existing) => existing.path !== action.item.path);
      next.push(action.item);
      next.sort(compareItems);
      return { ...state, items: next };
    }
    case "remove": {
      const prefix = `${action.path}/`;
      return {
        ...state,
        items: state.items.filter((item) => action.folder
          ? item.path !== action.path && !item.path.startsWith(prefix)
          : item.path !== action.path),
      };
    }
    case "revision":
      // Only the open folder carries a revision this page mutates; a file's settle answer
      // is its own identity and must not rewrite the listing's.
      return state.current && state.current.kind === "folder" && state.current.revision !== action.revision
        ? { ...state, current: { ...state.current, revision: action.revision } }
        : state;
    case "grow-rows":
      return { ...state, rows: growRowCount(state.rows, action.total) };
    case "all-rows":
      // Without an IntersectionObserver (an old engine), showing the list beats leaving
      // rows unreachable.
      return { ...state, rows: visibleRowCount(action.total, action.total) };
  }
}
