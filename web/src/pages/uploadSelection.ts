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

export async function droppedUploadEntries(transfer: DataTransfer): Promise<UploadEntry[]> {
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
  if (sources.length === 0) return filesToUploadEntries(transfer.files);

  const collector = entryCollector();
  async function visit(entry: FileSystemEntry, parent: string): Promise<void> {
    const name = normalizePath(entry.name);
    if (name.includes("/")) throw new Error(`Invalid upload entry name: ${entry.name}`);
    const path = parent ? `${parent}/${name}` : name;
    if (entry.isDirectory) {
      collector.add(path, null);
      const reader = (entry as FileSystemDirectoryEntry).createReader();
      while (true) {
        // Chromium returns at most 100 children per readEntries call.
        const children = await new Promise<FileSystemEntry[]>((resolve, reject) => {
          reader.readEntries(resolve, reject);
        });
        if (children.length === 0) break;
        for (const child of children) await visit(child, path);
      }
    } else if (entry.isFile) {
      const file = await new Promise<File>((resolve, reject) => {
        (entry as FileSystemFileEntry).file(resolve, reject);
      });
      collector.add(path, file);
    } else {
      throw new Error(`Unsupported upload entry: ${path}`);
    }
  }

  for (const source of sources) {
    if ("entry" in source) await visit(source.entry, "");
    else collector.add(source.file.webkitRelativePath || source.file.name, source.file);
  }
  return collector.entries;
}
