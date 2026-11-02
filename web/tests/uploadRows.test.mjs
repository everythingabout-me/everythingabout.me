import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/pages/uploadRows.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "uploadRows.ts");
const { buildUploadRows, isUploadDirectory } = await import(
  `data:text/javascript;base64,${Buffer.from(code).toString("base64")}`
);

function task(path, options = {}) {
  return {
    id: `task:${path}`,
    path,
    name: path,
    dir: "health",
    isDir: false,
    file: null,
    size: 100,
    loaded: 0,
    speed: 0,
    uploaded: false,
    overwrite: false,
    status: "queued",
    ...options,
  };
}

function folder(path, status = "completed") {
  return task(path, { isDir: true, size: 0, status });
}

function item(relative, isDir = false) {
  const path = `/${relative}`;
  const slash = path.lastIndexOf("/");
  return {
    path,
    parentPath: path.slice(0, slash) || "/",
    name: path.slice(slash + 1),
    kind: isDir ? "folder" : "file",
    revision: "nr_test",
    contentRevision: isDir ? null : "cr_test",
    size: isDir ? null : 100,
    mediaType: null,
    sourceModifiedAt: null,
    createdAt: "2026-10-31T10:00:00Z",
    updatedAt: "2026-10-31T10:00:00Z",
    checksum: null,
  };
}

const paths = (rows) => rows.map((row) => row.path);

test("shows only immediate children and aggregates all selected descendants", () => {
  const tasks = [
    folder("health/apple-health"),
    folder("health/apple-health/2015"),
    folder("health/apple-health/2015/09"),
    task("health/apple-health/2015/09/steps.json", { status: "completed" }),
    task("health/apple-health/2015/09/distance.json", { status: "uploading", size: 300, loaded: 120, speed: 50 }),
    task("health/apple-health/2025/steps.json"),
  ];
  const rows = buildUploadRows([], tasks, "health");
  assert.deepEqual(paths(rows), ["health/apple-health"]);
  assert.equal(rows[0].isDir, true);
  assert.equal(rows[0].name, "apple-health");
  assert.deepEqual(rows[0].upload, {
    status: "uploading", files: 3, completedFiles: 1, failed: 0,
    size: 500, loaded: 220, speed: 50, percent: 44,
    cancelIds: ["task:health/apple-health"], retryIds: [],
  });
  assert.deepEqual(paths(buildUploadRows([], tasks, "health/apple-health")), [
    "health/apple-health/2015", "health/apple-health/2025",
  ]);
  assert.deepEqual(paths(buildUploadRows([], tasks, "health/apple-health/2015/09")), [
    "health/apple-health/2015/09/distance.json", "health/apple-health/2015/09/steps.json",
  ]);
});

test("coalesces real items, pending folders and overwritten files by normalized path", () => {
  const existingFolder = item("health/root", true);
  const existingFile = item("health/readme.txt");
  const rows = buildUploadRows([existingFile, existingFolder], [
    folder("health/root", "queued"),
    task("health/root/child.txt"),
    task("health/readme.txt", { status: "uploading", loaded: 50 }),
  ], "health");
  assert.deepEqual(paths(rows), ["health/root", "health/readme.txt"]);
  assert.equal(rows[0].item, existingFolder);
  assert.equal(rows[1].item, existingFile);
  assert.equal(rows[1].upload.status, "uploading");
  assert.equal(rows[1].upload.loaded, 50);
});

test("keeps mixed roots separate and never mixes siblings with the same prefix", () => {
  const tasks = [
    task("health/photos/a.jpg", { dir: "health" }),
    task("health/archive/a.json", { dir: "health" }),
    task("health/loose.txt", { dir: "health" }),
    task("healthier/other.txt", { dir: "healthier" }),
    task("outside.txt", { dir: "" }),
  ];
  assert.deepEqual(paths(buildUploadRows([], tasks, "health")), ["health/archive", "health/photos", "health/loose.txt"]);
  assert.deepEqual(paths(buildUploadRows([], tasks, "")), ["health", "healthier", "outside.txt"]);
  assert.equal(buildUploadRows([], tasks, "health/photos")[0].isDir, false);
});

test("completed file metadata contributes its full bytes even after the File is released", () => {
  const rows = buildUploadRows([], [
    folder("health/root"),
    task("health/root/done.txt", { file: null, status: "completed", size: 200, loaded: 0, speed: 999 }),
    task("health/root/pending.txt", { size: 800 }),
  ], "health");
  assert.equal(rows[0].upload.completedFiles, 1);
  assert.equal(rows[0].upload.loaded, 200);
  assert.equal(rows[0].upload.size, 1000);
  assert.equal(rows[0].upload.percent, 20);
  assert.equal(rows[0].upload.speed, 0);
  assert.equal(buildUploadRows([], [task("health/done.txt", { status: "completed", file: null })], "health")[0].isDir, false);
});

test("navigation remains available after the root directory task completes", () => {
  const tasks = [folder("health/root"), folder("health/root/nested"), task("health/root/nested/file.json")];
  assert.equal(isUploadDirectory(tasks, "health/root"), true);
  assert.equal(isUploadDirectory(tasks, "health/root/nested"), true);
  assert.equal(isUploadDirectory(tasks, "health/root/nested/file.json"), false);
  assert.equal(isUploadDirectory(tasks, "health/roo"), false);
  assert.deepEqual(paths(buildUploadRows([], tasks, "health/root")), ["health/root/nested"]);
  assert.equal(buildUploadRows([], tasks, "health/root")[0].upload.status, "queued");
  const completed = tasks.map((entry) => ({ ...entry, status: "completed", file: null }));
  assert.equal(isUploadDirectory(completed, "health/root"), true);
  const row = buildUploadRows([item("health/root", true)], completed, "health")[0];
  assert.equal(row.upload.status, "completed");
  assert.equal(row.upload.percent, 100);
  assert.equal(row.upload.files, 1);
  assert.equal(row.upload.completedFiles, 1);
  assert.deepEqual(row.upload.cancelIds, []);
  assert.deepEqual(row.upload.retryIds, []);
});

test("empty folders retain correct identity and creation state without inventing files", () => {
  const rows = buildUploadRows([], [folder("health/empty", "queued"), folder("health/done")], "health");
  assert.deepEqual(paths(rows), ["health/done", "health/empty"]);
  assert.equal(rows[0].upload.status, "completed");
  assert.equal(rows[0].upload.percent, 100);
  assert.equal(rows[1].upload.status, "queued");
  assert.equal(rows[1].upload.files, 0);
  assert.equal(rows[1].upload.percent, 0);
  assert.deepEqual(buildUploadRows([], [folder("health/empty", "queued")], "health/empty"), []);
});

test("failed directories and files propagate to their folder while successful siblings stay counted", () => {
  const tasks = [
    folder("health/root"),
    folder("health/root/broken", "failed"),
    task("health/root/broken/a.txt"),
    task("health/root/b.txt", { status: "failed", loaded: 30 }),
    task("health/root/c.txt", { status: "completed" }),
    task("health/root/d.txt", { status: "uploading", loaded: 50, speed: 10 }),
  ];
  const summary = buildUploadRows([], tasks, "health")[0].upload;
  assert.equal(summary.status, "failed");
  assert.equal(summary.failed, 2);
  assert.equal(summary.files, 4);
  assert.equal(summary.completedFiles, 1);
  assert.equal(summary.loaded, 180);
  assert.deepEqual(summary.retryIds, ["task:health/root/broken", "task:health/root/b.txt"]);
  assert.deepEqual(summary.cancelIds, ["task:health/root"]);
});

test("normalizes Unicode paths and compares directory boundaries without flattening names", () => {
  const tasks = [
    folder("健康/cafe\u0301"),
    task("健康/cafe\u0301/步数.json", { status: "completed" }),
    task("健康/caféteria/other.json"),
  ];
  const rows = buildUploadRows([item("健康/café", true)], tasks, "健康");
  assert.equal(rows.filter((row) => row.path === "健康/café").length, 1);
  assert.deepEqual(paths(buildUploadRows([], tasks, "健康/café")), ["健康/café/步数.json"]);
  assert.equal(isUploadDirectory(tasks, "健康/café"), true);
  assert.equal(buildUploadRows([], tasks, "健康/cafe\u0301")[0].name, "步数.json");
});

test("clamps sent bytes and handles zero-byte files and finalization without NaN", () => {
  const zero = [folder("health/root"), task("health/root/empty", { size: 0, status: "completed" }), task("health/root/another", { size: 0 })];
  assert.equal(buildUploadRows([], zero, "health")[0].upload.percent, 50);
  const row = buildUploadRows([], [task("health/file", { loaded: 200, size: 100, status: "finalizing" })], "health")[0];
  assert.equal(row.upload.loaded, 100);
  assert.equal(row.upload.percent, 100);
  assert.equal(row.upload.status, "finalizing");
  assert.deepEqual(row.upload.cancelIds, ["task:health/file"]);
  const cancelling = buildUploadRows([], [task("health/file", { status: "finalizing", cancelRequested: true })], "health")[0];
  assert.deepEqual(cancelling.upload.cancelIds, []);
  assert.equal(buildUploadRows([], [task("health/file", { loaded: -1 })], "health")[0].upload.loaded, 0);
});

test("a folder does not claim to be finishing while later files remain queued", () => {
  const tasks = [
    folder("health/root"),
    task("health/root/first.txt", { status: "finalizing", loaded: 100 }),
    task("health/root/second.txt"),
  ];
  assert.equal(buildUploadRows([], tasks, "health")[0].upload.status, "uploading");
  assert.equal(buildUploadRows([], tasks.slice(0, 2), "health")[0].upload.status, "finalizing");
});

test("only the newest task for a repeated path contributes to counts", () => {
  const rows = buildUploadRows([], [
    task("health/root/file", { status: "completed", size: 10 }),
    task("health/root/file", { id: "replacement", size: 20, loaded: 5, status: "uploading" }),
  ], "health");
  assert.equal(rows[0].upload.files, 1);
  assert.equal(rows[0].upload.completedFiles, 0);
  assert.equal(rows[0].upload.size, 20);
  assert.equal(rows[0].upload.loaded, 5);
});
