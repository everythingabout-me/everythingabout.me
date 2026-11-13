import type { Item } from "../api/client";

/**
 * The revision an upload leaves stale on the folder it landed in.
 *
 * A folder's `revision` covers its indexed direct children, so every file a settle adds
 * inside it — and every ancestor folder that same upload creates — changes the folder's
 * revision. The page loaded the folder before the transfer began and keeps that value for
 * the next `If-Revision`; without a refresh, renaming, moving or deleting the folder sends
 * the state it had before the upload and meets 412. Retrying then sends the same stale
 * value, so the folder can never be renamed or deleted from that page again.
 *
 * The settle answer cannot refresh it: a commit answers with the published file (or the
 * Operation still publishing it), and a settle page item carries exactly that. The parent
 * folder is not in the answer, so its new revision has to be read back, and a read is what
 * this holds. A settle only marks the folder stale; one metadata request at a time re-reads
 * it, and a mutation waits for that answer instead of racing it. If the read fails the
 * folder stays stale, so the next mutation retries rather than settling on a value already
 * known to be old.
 */
export class FolderRevision {
  private latest: string | null = null;
  private stale = false;
  private reading: Promise<void> | null = null;
  private readonly read: () => Promise<Item | null>;

  constructor(read: () => Promise<Item | null>) {
    this.read = read;
  }

  /** Adopt the revision of a listing the page just loaded. */
  seed(revision: string | null): void {
    this.latest = revision;
    this.stale = false;
  }

  /** The folder changed under this value: a settle landed inside it. */
  invalidate(): void {
    this.stale = true;
  }

  /**
   * The revision the next mutation of the folder must name.
   *
   * Awaits the refresh a settle marked dirty instead of returning the value already known
   * to be old, so "the file just finished, delete the folder now" does not depend on
   * which of the two won the race.
   */
  async revision(): Promise<string | null> {
    if (this.reading) {
      // A mutation that arrives during the read waits for the same answer; it must not
      // start a second request for the same folder.
      await this.reading;
      return this.latest;
    }
    if (this.stale) await this.refresh();
    return this.latest;
  }

  private refresh(): Promise<void> {
    this.reading = (async () => {
      try {
        // One extra read catches a settle that landed while the first answer was in flight.
        // It is bounded because a mutation must not wait for a whole import to stop changing
        // the folder: a settle after the last read is concurrent by definition, and the
        // server's If-Revision answers it — which is what makes the next attempt re-read.
        for (let reads = 0; reads < 2; reads++) {
          this.stale = false;
          let item: Item | null;
          try {
            item = await this.read();
          } catch {
            // An unreadable index is not an unchanged folder: keep the last known value,
            // stay stale, and let the next mutation ask again.
            this.stale = true;
            return;
          }
          if (item) this.latest = item.revision;
          if (!this.stale) return;
        }
      } finally {
        this.reading = null;
      }
    })();
    return this.reading;
  }
}

/**
 * Whether `path` is the folder itself or sits below it.
 *
 * Route paths use "" for the root, while file paths are always absolute.
 */
export function withinFolder(path: string, folder: string): boolean {
  const base = folder === "" ? "/" : folder;
  return base === "/" || path === base || path.startsWith(`${base}/`);
}
