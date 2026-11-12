import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

// Reuse Vite's TypeScript transform to support Node versions without type stripping.
const source = await readFile(new URL("../src/pages/uploadSelection.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "uploadSelection.ts");
const { droppedUploadEntries, filesToUploadEntries, foldersWithoutFiles } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

function makeFile(name, relativePath = "") {
  const file = new File([`Contents of ${name}`], name, { type: "text/plain" });
  Object.defineProperty(file, "webkitRelativePath", { value: relativePath });
  return file;
}

function fileEntry(file, name = file.name) {
  return {
    name,
    isFile: true,
    isDirectory: false,
    file(resolve) {
      queueMicrotask(() => resolve(file));
    },
  };
}

function directoryEntry(name, batches = []) {
  return {
    name,
    isFile: false,
    isDirectory: true,
    reads: 0,
    readers: 0,
    createReader() {
      this.readers += 1;
      let index = 0;
      return {
        readEntries: (resolve) => {
          this.reads += 1;
          const batch = batches[index++] ?? [];
          queueMicrotask(() => resolve(batch));
        },
      };
    },
  };
}

function entryItem(entry) {
  return {
    kind: "file",
    webkitGetAsEntry: () => entry,
    getAsFile() {
      throw new Error("An entry must not be replaced with a placeholder file");
    },
  };
}

function transferWithEntries(...entries) {
  return { items: entries.map(entryItem), files: [] };
}

function paths(entries) {
  return entries.map((entry) => entry.path);
}

test("picker accepts null, empty lists, and ordinary files", () => {
  assert.deepEqual(filesToUploadEntries(null), []);
  assert.deepEqual(filesToUploadEntries([]), []);
  const file = makeFile("report.txt");
  assert.deepEqual(filesToUploadEntries([file]), [{ path: "report.txt", file }]);
  const fileList = { 0: file, length: 1, item: () => file };
  assert.deepEqual(filesToUploadEntries(fileList), [{ path: "report.txt", file }]);
});

test("picker preserves roots and nested same basenames with all parents before children", () => {
  const first = makeFile("same.txt", "root/left/deep/same.txt");
  const second = makeFile("same.txt", "root/right/same.txt");
  const third = makeFile("other.txt", "root/left/deep/other.txt");
  const entries = filesToUploadEntries([first, second, third]);
  assert.deepEqual(entries, [
    { path: "root", file: null },
    { path: "root/left", file: null },
    { path: "root/left/deep", file: null },
    { path: "root/left/deep/same.txt", file: first },
    { path: "root/right", file: null },
    { path: "root/right/same.txt", file: second },
    { path: "root/left/deep/other.txt", file: third },
  ]);
});

test("picker normalizes Unicode relative paths and deduplicates normalized ancestors", () => {
  const first = makeFile("re\u0301sume\u0301.txt", "Cafe\u0301/旅行/re\u0301sume\u0301.txt");
  const second = makeFile("notes.txt", "Café/旅行/notes.txt");
  const ordinary = makeFile("nai\u0308ve.txt");
  const entries = filesToUploadEntries([first, second, ordinary]);
  assert.deepEqual(paths(entries), [
    "Café", "Café/旅行", "Café/旅行/résumé.txt", "Café/旅行/notes.txt", "naïve.txt",
  ]);
  assert.equal(entries[2].file, first);
  assert.equal(first.name, "re\u0301sume\u0301.txt");
});

// Reading a dropped tree is one stat per file in the browser, so a large folder spends
// seconds there before the queue exists. The page can only say so if the walk reports what
// it has read: without a count on screen the drop looked like a freeze.
test("a dropped tree reports how much of it has been read", async () => {
  const root = directoryEntry("Health", [
    [directoryEntry("day-1", [[fileEntry(makeFile("a.json")), fileEntry(makeFile("b.json"))]]), fileEntry(makeFile("c.json"))],
    [],
  ]);
  const reports = [];
  const entries = await droppedUploadEntries(transferWithEntries(root), (count) => reports.push(count));
  assert.deepEqual(paths(entries), ["Health", "Health/day-1", "Health/day-1/a.json", "Health/day-1/b.json", "Health/c.json"]);
  assert.ok(reports.length > 0, "the walk reported nothing while it read");
  assert.equal(reports.at(-1), entries.length, "the last report is the whole selection");
  for (let index = 1; index < reports.length; index += 1) {
    assert.ok(reports[index] > reports[index - 1], "reports never go backwards");
  }
});

test("picker rejects empty, dot, and dotdot path segments", () => {
  for (const path of ["/file", "root/", "root//file", ".", "..", "./file", "../file", "root/./file", "root/../file"]) {
    assert.throws(() => filesToUploadEntries([makeFile("file", path)]), /Invalid upload path/);
  }
  assert.throws(() => filesToUploadEntries([makeFile("")]), /Invalid upload path/);
});

test("drops preserve nested same basenames, mixed files, and empty folders", async () => {
  const left = makeFile("same.txt");
  const right = makeFile("same.txt");
  const loose = makeFile("loose.txt");
  const empty = directoryEntry("empty");
  const root = directoryEntry("root", [[
    directoryEntry("left", [[fileEntry(left)]]),
    directoryEntry("right", [[fileEntry(right)]]),
    empty,
  ]]);
  const entries = await droppedUploadEntries(transferWithEntries(root, fileEntry(loose)));
  assert.deepEqual(entries, [
    { path: "root", file: null },
    { path: "root/left", file: null },
    { path: "root/left/same.txt", file: left },
    { path: "root/right", file: null },
    { path: "root/right/same.txt", file: right },
    { path: "root/empty", file: null },
    { path: "loose.txt", file: loose },
  ]);
  assert.equal(empty.reads, 1);
});

test("drops retain top-level empty folders without reading their placeholder files", async () => {
  const folder = directoryEntry("empty");
  const transfer = transferWithEntries(folder);
  transfer.files = [makeFile("empty")];
  assert.deepEqual(await droppedUploadEntries(transfer), [{ path: "empty", file: null }]);
});

test("drops drain every readEntries batch beyond 100 children", async () => {
  const files = Array.from({ length: 205 }, (_, index) => makeFile(`child-${index}.txt`));
  const children = files.map((file) => fileEntry(file));
  const folder = directoryEntry("many", [children.slice(0, 100), children.slice(100, 200), children.slice(200)]);
  const entries = await droppedUploadEntries(transferWithEntries(folder));
  assert.deepEqual(entries, [
    { path: "many", file: null },
    ...files.map((file) => ({ path: `many/${file.name}`, file })),
  ]);
  assert.equal(folder.readers, 1);
  assert.equal(folder.reads, 4);
});

test("drops normalize Unicode entry names while retaining file objects", async () => {
  const file = makeFile("re\u0301sume\u0301.txt");
  const folder = directoryEntry("Cafe\u0301", [[fileEntry(file)]]);
  assert.deepEqual(await droppedUploadEntries(transferWithEntries(folder)), [
    { path: "Café", file: null },
    { path: "Café/résumé.txt", file },
  ]);
});

test("all top-level entries and fallback files are captured synchronously before yielding", async () => {
  let active = true;
  const captured = [];
  const first = directoryEntry("folder");
  const second = fileEntry(makeFile("second.txt"));
  const fallback = makeFile("fallback.txt");
  const capture = (name, value) => {
    assert.equal(active, true, `${name} was captured after the event ended`);
    captured.push(name);
    return value;
  };
  const result = droppedUploadEntries({
    items: [
      { kind: "file", webkitGetAsEntry: () => capture("first", first) },
      { kind: "file", webkitGetAsEntry: () => capture("second", second) },
      { kind: "file", getAsFile: () => capture("fallback", fallback) },
    ],
    get files() {
      throw new Error("The fallback FileList must not duplicate captured items");
    },
  });
  active = false;
  assert.deepEqual(captured, ["first", "second", "fallback"]);
  assert.deepEqual(paths(await result), ["folder", "second.txt", "fallback.txt"]);
});

test("drops combine entry API and regular fallback items and ignore string data", async () => {
  const regular = makeFile("regular.txt");
  const nullEntryFile = makeFile("null-entry.txt");
  const result = await droppedUploadEntries({
    items: [
      entryItem(directoryEntry("folder")),
      { kind: "file", getAsFile: () => regular },
      { kind: "file", webkitGetAsEntry: () => null, getAsFile: () => nullEntryFile },
      { kind: "file", getAsFile: () => null },
      { kind: "string", webkitGetAsEntry() { throw new Error("String data is not a file"); } },
    ],
    files: [regular, nullEntryFile],
  });
  assert.deepEqual(result, [
    { path: "folder", file: null },
    { path: "regular.txt", file: regular },
    { path: "null-entry.txt", file: nullEntryFile },
  ]);
});

test("drops fall back to transfer.files when items are missing, empty, or unavailable", async () => {
  const file = makeFile("file.txt", "root/file.txt");
  const expected = [{ path: "root", file: null }, { path: "root/file.txt", file }];
  for (const items of [undefined, [], [{ kind: "file", getAsFile: () => null }]]) {
    assert.deepEqual(await droppedUploadEntries({ items, files: [file] }), expected);
  }
  assert.deepEqual(await droppedUploadEntries({ items: [], files: [] }), []);
});

test("drops propagate file callback errors without falling back to placeholders", async () => {
  const failure = new DOMException("Cannot read file", "NotReadableError");
  const entry = fileEntry(makeFile("unreadable.txt"));
  entry.file = (_resolve, reject) => queueMicrotask(() => reject(failure));
  await assert.rejects(droppedUploadEntries(transferWithEntries(entry)), (error) => error === failure);
});

test("drops propagate later directory batch errors without returning a partial tree", async () => {
  const failure = new DOMException("Cannot read directory", "NotReadableError");
  const folder = directoryEntry("folder");
  folder.createReader = () => {
    let calls = 0;
    return {
      readEntries(resolve, reject) {
        calls += 1;
        queueMicrotask(() => calls === 1 ? resolve([fileEntry(makeFile("first.txt"))]) : reject(failure));
      },
    };
  };
  await assert.rejects(droppedUploadEntries(transferWithEntries(folder)), (error) => error === failure);
});

test("drops propagate synchronous file and directory reader errors", async () => {
  const failure = new Error("Reader failed synchronously");
  const file = fileEntry(makeFile("file.txt"));
  file.file = () => { throw failure; };
  await assert.rejects(droppedUploadEntries(transferWithEntries(file)), (error) => error === failure);
  const folder = directoryEntry("folder");
  folder.createReader = () => ({ readEntries() { throw failure; } });
  await assert.rejects(droppedUploadEntries(transferWithEntries(folder)), (error) => error === failure);
});

test("drops reject invalid entry names at any depth", async () => {
  for (const name of ["", ".", "..", "/absolute", "nested/name", "nested//name", "nested/../name"]) {
    await assert.rejects(droppedUploadEntries(transferWithEntries(directoryEntry(name))), /Invalid upload/);
    const folder = directoryEntry("root", [[fileEntry(makeFile("safe.txt"), name)]]);
    await assert.rejects(droppedUploadEntries(transferWithEntries(folder)), /Invalid upload/);
  }
});

test("drops validate fallback paths as strictly as picker paths", async () => {
  const file = makeFile("file.txt", "root/../file.txt");
  await assert.rejects(droppedUploadEntries({ items: [], files: [file] }), /Invalid upload path/);
  await assert.rejects(droppedUploadEntries({
    items: [{ kind: "file", getAsFile: () => file }],
    files: [],
  }), /Invalid upload path/);
});

test("folders that contain a file are left to the upload, empty ones stay", () => {
  const entries = [
    { path: "apple-health", file: null },
    { path: "apple-health/2025", file: null },
    { path: "apple-health/2025/07", file: null },
    { path: "apple-health/2025/07/01", file: null },
    { path: "apple-health/2025/07/01/steps.json", file: new File(["{}"], "steps.json") },
    { path: "apple-health/empty", file: null },
  ];
  const kept = foldersWithoutFiles(entries).map((entry) => entry.path);
  assert.deepEqual(kept, ["apple-health/2025/07/01/steps.json", "apple-health/empty"]);
});
