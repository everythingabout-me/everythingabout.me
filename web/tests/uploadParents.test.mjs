import assert from "node:assert/strict";
import test from "node:test";
import { setup, TestApiError, uploadServer } from "./helpers/uploadHarness.mjs";

// The destination parent belongs in the request that needs it. A dropped tree names folders
// the caller meant to create, and an earlier, cancelled import may never have made one of
// them; a retry is the same create, so the create itself must be able to materialize the
// chain (`parents: true`). These tests pin that the queue sends the flag, that it never
// creates a folder for a file itself, and that a retry replays the flagged request.
const entry = (path, bytes = "ab") => ({ path, file: new File([bytes], path.split("/").at(-1), { type: "application/json", lastModified: 1443268800000 }) });
async function mount(t, api, options) { const harness = await setup(api, options); t.after(harness.unmount); return harness; }
function enqueue(harness, entries, dir = "") { harness.render().enqueue(entries, dir); }
const callsOf = (server, type) => server.calls.filter(([kind]) => kind === type);
const allComplete = (queue) => queue.tasks.length > 0 && queue.tasks.every((task) => task.status === "completed");

test("a file from a dropped tree asks its create to materialize the missing ancestors", async (t) => {
  const server = uploadServer();
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("2026/07/16/a.jpg")]);
  await harness.until(allComplete);
  const creates = callsOf(server, "create");
  assert.equal(creates.length, 1);
  assert.equal(creates[0][1].parents, true, "the create carries parents: true");
  assert.equal(creates[0][1].condition.mode, "create");
  assert.equal(callsOf(server, "mkdir").length, 0, "the client does not create the folder itself");
});

test("a standing replace answer still asks the overwrite create for its ancestors", async (t) => {
  const server = uploadServer();
  const harness = await mount(t, server.api);
  // The answer a re-import gives: every existing destination is replaced. A file whose
  // folder is absent is admitted by the same standing answer, so its condition is overwrite.
  harness.render().answerConflict("replace-all");
  enqueue(harness, [entry("2026/07/16/a.jpg")]);
  await harness.until(allComplete);
  const creates = callsOf(server, "create");
  assert.equal(creates.length, 1);
  assert.equal(creates[0][1].condition.mode, "overwrite");
  assert.equal(creates[0][1].parents, true, "an overwrite into a missing folder asks for the chain too");
  assert.equal(callsOf(server, "mkdir").length, 0);
});

test("a parent_missing refusal is the server's answer, not a client folder create", async (t) => {
  const server = uploadServer();
  const attempts = [];
  server.api.createUpload = async (input) => {
    attempts.push(input);
    throw new TestApiError("The destination parent does not exist.", 409, { code: "parent_missing", requestId: "req_missing" });
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("absent/a.jpg")]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  assert.equal(queue.tasks[0].error, "The destination parent does not exist.");
  assert.equal(attempts.length, 1);
  assert.equal(callsOf(server, "mkdir").length, 0, "the upload queue never creates a folder for a file");
  queue.retry(queue.tasks[0].id);
  queue = await harness.until((queue) => queue.tasks[0]?.status === "failed" && attempts.length === 2);
  assert.equal(callsOf(server, "mkdir").length, 0, "a retry repeats the request; it does not add a folder repair");
});

test("a retry replays the same flagged create once the folder exists", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  const attempts = [];
  let ready = false;
  server.api.createUpload = async (input, key) => {
    attempts.push({ input: structuredClone(input), key });
    if (!ready) throw new TestApiError("The destination parent does not exist.", 409, { code: "parent_missing", requestId: "req_missing" });
    return create(input, key);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("late/tree/a.jpg")]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  const id = queue.tasks[0].id;
  assert.equal(attempts.length, 1);
  assert.equal(attempts[0].input.parents, true);
  ready = true;
  queue.retry(id);
  queue = await harness.until(allComplete);
  assert.equal(attempts.length, 2);
  assert.equal(attempts[1].key, attempts[0].key, "the retry reuses the retained create key");
  assert.deepEqual(attempts[1].input, attempts[0].input, "the retry is the same request, flag included");
  assert.equal(harness.completed.length, 1);
});

test("a retry of an intent frozen before the flag rebuilds it with parents", async (t) => {
  const server = uploadServer();
  const create = server.api.createUpload;
  const attempts = [];
  let first = true;
  server.api.createUpload = async (input, key) => {
    // The first attempt fails for a reason that allocated no session; later attempts are
    // accepted only if the create actually asks for its ancestors, like a server that
    // predates this queue but not the flag.
    attempts.push({ input: structuredClone(input), key });
    if (first) { first = false; throw new TestApiError("quota exceeded", 507, { code: "quota_exceeded" }); }
    if (input.parents !== true) throw new TestApiError("The destination parent does not exist.", 409, { code: "parent_missing" });
    return create(input, key);
  };
  const harness = await mount(t, server.api);
  enqueue(harness, [entry("stale/tree/a.jpg")]);
  let queue = await harness.until((queue) => queue.tasks[0]?.status === "failed");
  // A row admitted before this change retained a create without the flag. The published
  // task shares its intent object with the queue's own state, so removing the field models
  // exactly that frozen request.
  delete queue.tasks[0].intent.parents;
  queue.retry(queue.tasks[0].id);
  queue = await harness.until(allComplete);
  assert.equal(attempts.length, 2);
  assert.equal(attempts[1].input.parents, true, "the rebuilt intent carries the flag the frozen one lacked");
  assert.equal(harness.completed.length, 1);
});
