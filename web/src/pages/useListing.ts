import { useCallback, useEffect, useReducer, useRef } from "react";
import { useNavigate } from "react-router";
import { ApiError, api, filePath, openListing, type Item, type ListingStream } from "../api/client";
import { t } from "../i18n";
import { crumbsForPath, initialListingState, listingKey, listingReducer, type ListingState } from "./listingState";
import { FolderRevision, withinFolder } from "./uploadRevision";

interface ListingOptions {
  /** The path the page is showing, from the route; "" is the root. */
  path: string;
  /** Measure the outgoing card before another listing replaces it. */
  rememberHeight(key: string): void;
  /** Report a failed continuation or search without replacing the rows. */
  showToast(text: string, kind: "info" | "error", sticky?: boolean): void;
}

/**
 * The listing and its request lifecycle: the first paint, the streamed continuation, the
 * search that stands in for a folder, the row chunk, and the folder revision an upload
 * leaves stale.
 *
 * Contract
 * - Caller supplies the route path, a stable `rememberHeight` and a stable `showToast`; the
 *   hook owns the request tickets, the cursor chain, the row chunk and the revision tracker.
 * - Reads return through `state`; every mutation the page performs on the loaded listing
 *   (a settle, a delete, a folder revision) is an action here, never `setState` in the page.
 *
 * Behaviour this keeps, because it is easy to lose when moving code:
 * - A navigation keeps the outgoing rows on screen until the next answer arrives, and a
 *   response older than the newest request is dropped (including its continuation pages).
 * - A continuation failure reports itself and keeps the rows already loaded: the first
 *   page proved the folder is readable, so it is not an error state.
 * - A settle inside the open folder invalidates its revision. The read-back is coalesced:
 *   one metadata request when the import ends, or when a mutation needs it first.
 * - A search result is not a folder listing: it neither receives settlements nor seeds a
 *   folder revision.
 */
export interface ListingController {
  state: ListingState;
  /** The identity the list's fade and row chunk follow. */
  key: string;
  load(target: string): Promise<void>;
  search(keyword: string): Promise<void>;
  /** Re-read what is on screen: the committed search, or the route's folder. */
  reload(): void;
  setSearchText(value: string): void;
  growRows(total: number): void;
  showAllRows(total: number): void;
  /** A file that settled into the open folder. */
  upsertItem(item: Item): void;
  /** A deleted item, and its subtree when it was a folder. */
  removePath(path: string, folder: boolean): void;
  /**
   * The route path and whether the rows are search results.
   *
   * Deliberately the route rather than the loaded listing: a callback that settles a file
   * must compare against the folder being viewed now, not the one still on screen while
   * the next one loads.
   */
  view(): { path: string; searching: boolean };
  /** An upload settled inside the open folder. */
  noteSettle(item: Item): void;
  /** Read the stale revision, once, when the import has stopped changing the folder. */
  refreshSettled(): Promise<void>;
  /** The revision a mutation of `target` must name. */
  revisionForMutation(target: Item): Promise<string>;
  /** The server refused a revision: re-read before the next attempt. */
  invalidateRevision(): void;
}

export function useListing({ path, rememberHeight, showToast }: ListingOptions): ListingController {
  const navigate = useNavigate();
  const [state, dispatch] = useReducer(listingReducer, undefined, initialListingState);
  const stateRef = useRef(state);
  stateRef.current = state;
  const pathRef = useRef(path);
  pathRef.current = path;
  // Every list / search request gets a ticket; a response older than the newest is dropped.
  const requestId = useRef(0);
  // The listing read in progress. A new folder cancels its continuation, so a page the
  // person already left cannot keep reading or append to the new folder's rows.
  const stream = useRef<ListingStream | null>(null);
  // The revision of the folder this listing shows, and whether an upload has made it
  // stale. A folder's revision covers its direct children, so every settle inside the
  // folder invalidates the value the page loaded with. A settle answer that names the
  // folder's new revision is adopted directly; a settle that does not leaves the folder
  // stale, and the new revision is read back from metadata — lazily, when the import ends
  // or a mutation needs it.
  const revision = useRef<{ path: string; tracker: FolderRevision } | null>(null);
  // Set by a settle and consumed when the uploads finish: the folder is stale then.
  const settleInvalidated = useRef(false);

  /**
   * Read back the open folder's revision after an upload changed it.
   *
   * `FolderRevision` coalesces that read: settles that did not carry the folder's revision
   * mark it stale, and this is called once when the queue empties, so an import costs one
   * metadata request at the end rather than one per file — and none at all when every
   * commit answer carried the revision.
   */
  const refreshFolderRevision = useCallback(async () => {
    const listing = revision.current;
    if (!listing || listing.path !== pathRef.current) return;
    const next = await listing.tracker.revision();
    if (!next || revision.current !== listing) return;
    dispatch({ type: "revision", revision: next });
  }, []);

  const load = useCallback(async (target: string) => {
    const request = ++requestId.current;
    stream.current?.cancel();
    const listing = openListing(target);
    stream.current = listing;
    dispatch({ type: "load" });
    try {
      // Metadata and the first page were requested together. Waiting for both is the
      // whole first paint; the cursor chain below is not part of it.
      const data = await listing.first;
      if (requestId.current !== request) {
        listing.cancel();
        return;
      }
      rememberHeight(`${target}|`);
      dispatch({ type: "loaded", path: target, items: data.items, current: data.node, crumbs: data.breadcrumb });
      // The listing is the new truth for this folder, so it also seeds the revision the
      // next mutation will name — an upload that settles later invalidates it again.
      const tracker = new FolderRevision(async () => {
        const item = await api.metadata(target);
        return item.kind === "folder" ? item : null;
      });
      tracker.seed(data.node?.revision ?? null);
      revision.current = { path: target, tracker };
      // The rest streams in behind the rows already on screen. A failed continuation
      // keeps what loaded and reports itself; it does not replace the folder with an
      // error, because the first page proved the folder is readable.
      void listing.rest((page) => {
        if (requestId.current !== request) return;
        dispatch({ type: "page", items: page });
      }).catch((error: unknown) => {
        if (requestId.current !== request) return;
        showToast(error instanceof Error ? error.message : t("drive.loadMoreFailed"), "error");
      });
    } catch (error) {
      if (requestId.current !== request) return;
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      dispatch({ type: "failed", path: target, error: error instanceof Error ? error.message : t("error.loadFailed") });
    } finally {
      if (requestId.current === request) dispatch({ type: "settled" });
    }
  }, [navigate, rememberHeight, showToast]);

  const search = useCallback(async (keyword: string) => {
    const value = keyword.trim();
    if (!value) {
      void load(pathRef.current);
      return;
    }
    // Search results replace the folder rows, so its cursor chain has nothing left to feed.
    stream.current?.cancel();
    stream.current = null;
    const request = ++requestId.current;
    dispatch({ type: "load" });
    try {
      const data = await api.search(value);
      if (requestId.current !== request) return;
      rememberHeight(`${pathRef.current}|${value}`);
      dispatch({ type: "searched", path: pathRef.current, query: value, items: data.items, crumbs: crumbsForPath(pathRef.current) });
    } catch (error) {
      if (requestId.current !== request) return;
      if (error instanceof ApiError && error.status === 401) {
        navigate("/login", { replace: true });
        return;
      }
      showToast(error instanceof Error ? error.message : t("error.searchFailed"), "error");
      // A failed search should fall back to the folder, not to an endless skeleton.
      void load(pathRef.current);
    } finally {
      if (requestId.current === request) dispatch({ type: "settled" });
    }
  }, [load, navigate, rememberHeight, showToast]);

  // A reload must re-read whatever is actually on screen. The callback is stable because
  // the page hands it to gestures that must not be rebuilt by every publish.
  const reload = useCallback(() => {
    if (stateRef.current.query) void search(stateRef.current.query);
    else void load(pathRef.current);
  }, [load, search]);

  const setSearchText = useCallback((value: string) => dispatch({ type: "search-text", value }), []);
  const growRows = useCallback((total: number) => dispatch({ type: "grow-rows", total }), []);
  const showAllRows = useCallback((total: number) => dispatch({ type: "all-rows", total }), []);

  const upsertItem = useCallback((item: Item) => dispatch({ type: "item", item }), []);
  const removePath = useCallback((target: string, folder: boolean) => dispatch({ type: "remove", path: target, folder }), []);

  const view = useCallback(() => ({
    path: pathRef.current,
    searching: Boolean(stateRef.current.query),
  }), []);

  /**
   * A settle landed inside the folder this page shows.
   *
   * The commit answer names the folder's revision after its own settle, so a file that
   * landed directly in the open folder adopts it and the page keeps working without a
   * read. A deeper settle (an upload that created folders on its way in changes an
   * ancestor's direct membership too), or an answer from a server that does not carry the
   * revision, still leaves the value stale for the coalesced read.
   */
  const noteSettle = useCallback((item: Item) => {
    const listing = revision.current;
    if (stateRef.current.query || !listing || listing.path !== pathRef.current || !withinFolder(item.path, listing.path)) return;
    if (item.parentPath === filePath(listing.path) && item.parentRevision && listing.tracker.adopt(item.parentRevision)) {
      // The adopted value is also what the page shows for this folder, so the renderer's
      // copy follows without the read this page no longer has to make.
      dispatch({ type: "revision", revision: item.parentRevision });
      return;
    }
    listing.tracker.invalidate();
    settleInvalidated.current = true;
  }, []);

  const refreshSettled = useCallback(async () => {
    if (!settleInvalidated.current) return;
    settleInvalidated.current = false;
    await refreshFolderRevision();
  }, [refreshFolderRevision]);

  /**
   * The revision a mutation of this folder must carry.
   *
   * Rename, move and delete of the folder send `If-Revision`, and the value the page holds
   * is the one it loaded before any upload settled inside the folder. Waiting for the
   * refresh here is what makes the next mutation use the new value: a retry with the stale
   * revision answers 412 again, and no retry can ever recover from it.
   */
  const revisionForMutation = useCallback(async (target: Item): Promise<string> => {
    const listing = revision.current;
    if (target.kind !== "folder" || !listing || listing.path !== target.path) return target.revision;
    return (await listing.tracker.revision()) ?? target.revision;
  }, []);

  const invalidateRevision = useCallback(() => { revision.current?.tracker.invalidate(); }, []);

  // The path is the URL (/files/Photos/2024), so back/forward and direct visits open the right place.
  useEffect(() => {
    void load(path);
  }, [path, load]);

  // A browser-level navigation must not leave a cursor chain reading for a page that is gone.
  useEffect(() => () => { stream.current?.cancel(); }, []);

  return {
    state,
    key: listingKey(state),
    load,
    search,
    reload,
    setSearchText,
    growRows,
    showAllRows,
    upsertItem,
    removePath,
    view,
    noteSettle,
    refreshSettled,
    revisionForMutation,
    invalidateRevision,
  };
}
