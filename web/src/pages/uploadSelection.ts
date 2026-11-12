export type UploadEntry = { path: string; file: File | null };

function normalizePath(path: string): string {
  const normalized = path.normalize("NFC");
  if (normalized.split("/").some((segment) => !segment || segment === "." || segment === "..")) {
    throw new Error(`Invalid upload path: ${path}`);
  }
  return normalized;
}

function entryCollector() {
  const entries: UploadEntry[] = [];
  const directories = new Set<string>();

  function add(path: string, file: File | null) {
    const normalized = normalizePath(path);
    const segments = normalized.split("/");
    const directoryCount = file === null ? segments.length : segments.length - 1;
    for (let index = 1; index <= directoryCount; index += 1) {
      const directory = segments.slice(0, index).join("/");
      if (!directories.has(directory)) {
        directories.add(directory);
        entries.push({ path: directory, file: null });
      }
    }
    if (file !== null) entries.push({ path: normalized, file });
  }

  return { entries, add };
}

export function filesToUploadEntries(files: File[] | FileList | null): UploadEntry[] {
  const collector = entryCollector();
  for (const file of Array.from(files ?? [])) {
    collector.add(file.webkitRelativePath || file.name, file);
  }
  return collector.entries;
}

/**
 * A folder that will contain a file needs no mkdir of its own: the upload creates
 * the missing chain inside its own request (`parents`), which is one round trip
 * per file instead of one per directory. Only folders with no file in them (or no
 * selected descendant at all) still have to be created explicitly.
 */
export function foldersWithoutFiles(entries: UploadEntry[]): UploadEntry[] {
  const withFile = new Set<string>();
  for (const entry of entries) {
    if (entry.file === null) continue;
    const segments = entry.path.split("/");
    for (let index = 1; index < segments.length; index += 1) {
      withFile.add(segments.slice(0, index).join("/"));
    }
  }
  return entries.filter((entry) => entry.file !== null || !withFile.has(entry.path));
}

/** Files per handover while a dropped tree is still being read. */
const DROPPED_TREE_BATCH = 200;
/** Longest a read file waits for company before it is handed over. */
const DROPPED_TREE_HANDOVER_MS = 200;
/**
 * `file()` calls in flight while reading one directory.
 *
 * Each call is a round trip to the browser's file service, not a byte read, so a serialized
 * walk costs the sum of those round trips before it can hand over anything: a 20k-file
 * folder was 20k of them, one after another. Overlapping a few is what keeps the walk ahead
 * of the uploads it feeds; more would only queue behind the same file service.
 */
const DROPPED_TREE_CONCURRENCY = 8;

/** The row order: case-insensitive name, exact spelling as the tie-break. */
function compareEntryNames(a: FileSystemEntry, b: FileSystemEntry): number {
  const left = a.name.toLowerCase();
  const right = b.name.toLowerCase();
  if (left !== right) return left < right ? -1 : 1;
  return a.name < b.name ? -1 : a.name > b.name ? 1 : 0;
}

export interface DroppedTreeOptions {
  /** Entries read so far, reported at most ten times a second. */
  onProgress?: (count: number) => void;
  /**
   * File entries as the walk reaches them, in row order and in batches.
   *
   * This is what makes a large drop progressive: the queue starts uploading the first files
   * while the rest of the tree is still being read. A caller that passes it has already
   * queued every file, so it must not queue the returned list again — only the folders that
   * still need a task (see `foldersWithoutFiles`).
   */
  onFiles?: (files: UploadEntry[]) => void;
  batchSize?: number;
}

/**
 * Read a dropped tree.
 *
 * A dropped folder cannot be handed over in one piece without making the drop feel frozen:
 * every file costs one `file()` round trip before its size — and therefore its upload
 * intent — is known, so a large folder spends seconds reading before the first row exists.
 * The walk therefore hands each file over as it finds it, and reads the tree in the order
 * the sheet shows it (folders before files, name within a level, depth-first) so the queue
 * starts what the person sees first.
 *
 * Folders that hold no file are the one thing that cannot be streamed: whether a folder
 * needs its own task depends on the whole subtree. The returned list still contains every
 * entry, and `foldersWithoutFiles` over it is how the caller creates exactly those folders
 * — every other folder is created by the first file that lands in it.
 */
export async function droppedUploadEntries(
  transfer: DataTransfer,
  options: DroppedTreeOptions = {},
): Promise<UploadEntry[]> {
  type Source = { entry: FileSystemEntry } | { file: File };
  const sources: Source[] = [];

  // Drag data can become inaccessible as soon as the drop handler yields.
  for (const item of Array.from(transfer.items ?? [])) {
    if (item.kind !== "file") continue;
    const entry = typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry() : null;
    if (entry) {
      sources.push({ entry });
    } else {
      const file = item.getAsFile();
      if (file) sources.push({ file });
    }
  }
  if (sources.length === 0) {
    // No entry API: the drop handed over a flat list, already read by the browser. There is
    // nothing left to walk, so it is one handover — but it still goes through `onFiles`, so a
    // caller that streams never loses the files of a browser that cannot stream.
    const entries = filesToUploadEntries(transfer.files);
    const files = entries.filter((entry) => entry.file !== null);
    if (files.length) options.onFiles?.(files);
    options.onProgress?.(entries.length);
    return entries;
  }

  const collector = entryCollector();
  const batch: UploadEntry[] = [];
  let handedAt = Number.NEGATIVE_INFINITY;
  const flush = () => {
    if (!batch.length) return;
    handedAt = performance.now();
    options.onFiles?.(batch.splice(0, batch.length));
  };
  const hand = (file: File, path: string) => {
    if (!options.onFiles) return;
    batch.push({ path, file });
    // A steady walk fills whole pages; a slow one (a network volume, a cold directory) must
    // not hold the files it has already read until the tree ends.
    if (batch.length >= (options.batchSize ?? DROPPED_TREE_BATCH)
      || performance.now() - handedAt >= DROPPED_TREE_HANDOVER_MS) flush();
  };

  let found = 0;
  let reportedAt = Number.NEGATIVE_INFINITY;
  const count = () => {
    found += 1;
    if (!options.onProgress) return;
    const now = performance.now();
    if (now - reportedAt < 100) return;
    reportedAt = now;
    options.onProgress(found);
  };
  const add = (path: string, file: File | null): string => {
    const normalized = normalizePath(path);
    collector.add(normalized, file);
    count();
    if (file) hand(file, normalized);
    return normalized;
  };

  // Chromium returns at most 100 children per readEntries call, in whatever order the
  // directory happens to store them; one directory is read to its end before descending.
  async function readDirectory(directory: FileSystemDirectoryEntry): Promise<FileSystemEntry[]> {
    const reader = directory.createReader();
    const children: FileSystemEntry[] = [];
    while (true) {
      const page = await new Promise<FileSystemEntry[]>((resolve, reject) => {
        reader.readEntries(resolve, reject);
      });
      if (page.length === 0) break;
      children.push(...page);
    }
    return children.sort(compareEntryNames);
  }
  function readFile(entry: FileSystemFileEntry): Promise<File> {
    return new Promise<File>((resolve, reject) => entry.file(resolve, reject));
  }
  // The results are placed by index, so the handover stays in name order even though the
  // round trips overlap.
  async function readFiles(entries: FileSystemFileEntry[], parent: string): Promise<void> {
    // Names are checked before the round trips start, so a bad name fails the walk instead
    // of racing its siblings.
    const names = entries.map((entry) => {
      const name = normalizePath(entry.name);
      if (name.includes("/")) throw new Error(`Invalid upload entry name: ${entry.name}`);
      return name;
    });
    const files = new Array<File | null>(entries.length).fill(null);
    let next = 0;
    const worker = async () => {
      while (next < entries.length) {
        const at = next++;
        files[at] = await readFile(entries[at]);
      }
    };
    await Promise.all(Array.from({ length: Math.min(DROPPED_TREE_CONCURRENCY, entries.length) }, worker));
    for (let at = 0; at < entries.length; at += 1) {
      // A file entry without its `file()` result is a read that failed; that promise
      // rejected above instead.
      if (files[at]) add(parent ? `${parent}/${names[at]}` : names[at], files[at]);
    }
  }
  async function visit(entry: FileSystemEntry, parent: string): Promise<void> {
    const name = normalizePath(entry.name);
    if (name.includes("/")) throw new Error(`Invalid upload entry name: ${entry.name}`);
    const path = parent ? `${parent}/${name}` : name;
    if (entry.isDirectory) {
      add(path, null);
      const children = await readDirectory(entry as FileSystemDirectoryEntry);
      const directories = children.filter((child) => child.isDirectory);
      const files = children.filter((child) => child.isFile);
      for (const child of children) {
        if (!child.isDirectory && !child.isFile) {
          throw new Error(`Unsupported upload entry: ${parent ? `${parent}/${child.name}` : child.name}`);
        }
      }
      for (const child of directories) await visit(child, path);
      await readFiles(files as FileSystemFileEntry[], path);
    } else if (entry.isFile) {
      add(path, await readFile(entry as FileSystemFileEntry));
    } else {
      throw new Error(`Unsupported upload entry: ${path}`);
    }
  }

  for (const source of sources) {
    if ("entry" in source) await visit(source.entry, "");
    else {
      const file = source.file;
      add(file.webkitRelativePath || file.name, file);
    }
  }
  flush();
  options.onProgress?.(found);
  return collector.entries;
}
