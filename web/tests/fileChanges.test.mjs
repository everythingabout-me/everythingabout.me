import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { transformWithOxc } from "vite";

const source = await readFile(new URL("../src/api/client.ts", import.meta.url), "utf8");
const { code } = await transformWithOxc(source, "client.ts");
let instance = 0;
const client = () => import(`data:text/javascript;base64,${Buffer.from(`${code}\n// changes ${++instance}`).toString("base64")}`);
const targetID = "1411944e-bcbe-49a2-aa30-7196f3dc09b4";
const snapshotID = "b9ca691b-27de-4f86-bd5a-b853efc2e498";
const subscriptionID = "c2101a6a-6c43-46fa-8a40-097195407bc4";
const cursor = "opaque+#?%2F/=checkpoint";
const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const snapshot = (filter) => ({ id: snapshotID, state: "ready", filter, totalEntries: 1,
  entriesUrl: `/api/files/change-snapshots/${snapshotID}`, changesCursor: cursor,
  createdAt: "2026-11-04T08:00:00Z", expiresAt: "2026-11-05T08:00:00Z" });
const subscription = (filter) => ({ id: subscriptionID, filter, state: "active",
  expiresAt: "2026-11-11T08:00:00Z", changesCursor: cursor, eventsUrl: `/api/files/subscriptions/${subscriptionID}/events` });
function capture(t, respond) {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (input, init) => {
    const call = { input, url: new URL(input, "https://drive.example"), init };
    calls.push(call);
    return input === "/api/files/me" ? json({ csrfToken: "session-token", changes: true, subscribe: true }) : respond(call);
  };
  t.after(() => { globalThis.fetch = original; });
  return calls;
}
const invalidSelector = (error) => error.status === 400 && error.code === "invalid_selector";

test("change snapshot creation sends the chosen ID or literal path with cookie CSRF and no lookup", async (t) => {
  const { api, files } = await client();
  const calls = capture(t, ({ init }) => json(snapshot(JSON.parse(init.body)), 201));
  const signal = new AbortController().signal;
  const byID = { target: { id: targetID }, kind: "folder", recursive: true };
  assert.deepEqual((await files.createChangeSnapshot(byID, signal)).filter, byID);
  const byPath = { target: { path: "/Notes/cafe\u0301 +#?%2F" }, kind: "folder", recursive: false };
  assert.deepEqual((await api.createChangeSnapshot(byPath)).filter, byPath);
  assert.equal(calls.length, 3);
  assert.equal(calls[0].input, "/api/files/me");
  for (const call of calls.slice(1)) {
    assert.equal(call.url.pathname, "/api/files/change-snapshots");
    assert.equal(call.url.search, "");
    assert.equal(call.init.method, "POST");
    assert.equal(call.init.credentials, "same-origin");
    assert.equal(call.init.headers.get("X-CSRF-Token"), "session-token");
    assert.equal(call.init.headers.get("Content-Type"), "application/json");
  }
  assert.equal(calls[1].init.signal, signal);
  assert.equal(JSON.parse(calls[2].init.body).target.path, byPath.target.path);
});

test("snapshot pages preserve saved markers and encode cursors once; deletion accepts empty 204", async (t) => {
  const { api } = await client();
  const filter = { target: { id: targetID }, kind: "folder", recursive: true };
  const metadata = { id: targetID, lastChangedAt: "2026-11-04T08:00:00Z", changeToken: "event-id", revision: "saved-revision" };
  const calls = capture(t, ({ init }) => init.method === "DELETE" ? new Response(null, { status: 204 }) : json({ ...snapshot(filter), items: [metadata], nextCursor: null }));
  const signal = new AbortController().signal;
  const page = await api.changeSnapshot(snapshotID, { cursor, limit: 17 }, signal);
  assert.deepEqual(page.items, [metadata]);
  assert.equal(page.nextCursor, null);
  assert.equal(page.changesCursor, cursor);
  assert.equal(calls[0].url.pathname, `/api/files/change-snapshots/${snapshotID}`);
  assert.equal(calls[0].url.searchParams.get("cursor"), cursor);
  assert.equal(calls[0].url.searchParams.get("limit"), "17");
  assert.ok(calls[0].url.search.includes("%252F"));
  assert.equal(calls[0].init.signal, signal);
  assert.equal(await api.deleteChangeSnapshot(snapshotID, signal), undefined);
  assert.equal(calls[1].input, "/api/files/me");
  assert.equal(calls[2].init.method, "DELETE");
  assert.equal(calls[2].init.headers.get("X-CSRF-Token"), "session-token");
});

test("change pull preserves empty checkpoints and terminal Problems without replay or scope parameters", async (t) => {
  const { api, FileApiError } = await client();
  const calls = capture(t, ({ url }) => url.searchParams.get("cursor") === "terminal"
    ? json({ type: "urn:eam:error:watch_deleted", title: "Conflict", status: 409, code: "watch_deleted", detail: "The target was deleted.", requestId: "req_test" }, 409)
    : json({ items: [], nextCursor: "next-empty-checkpoint", hasMore: false }));
  assert.deepEqual(await api.changes(cursor, { limit: 23 }), { items: [], nextCursor: "next-empty-checkpoint", hasMore: false });
  assert.equal(calls[0].url.pathname, "/api/files/changes");
  assert.deepEqual([...calls[0].url.searchParams], [["cursor", cursor], ["limit", "23"]]);
  await assert.rejects(api.changes("terminal"), (error) => error instanceof FileApiError && error.code === "watch_deleted" && error.status === 409);
  assert.equal(calls.length, 2);
  assert.ok(calls.every(({ init }) => init.credentials === "same-origin" && !init.headers.has("Authorization")));
});

test("subscriptions retain selector mode and SSE delivery across create inspect renew and delete", async (t) => {
  const { api, files } = await client();
  let saved;
  const calls = capture(t, ({ url, init }) => {
    if (init.method === "DELETE") return new Response(null, { status: 204 });
    if (init.method === "POST" && url.pathname.endsWith("/subscriptions")) {
      const input = JSON.parse(init.body);
      assert.deepEqual(input.delivery, { type: "sse" });
      const { delivery, ...filter } = input;
      saved = subscription(filter);
      return json(saved, 201);
    }
    return json(url.pathname.endsWith("/subscriptions") ? { items: [saved] } : saved);
  });
  for (const target of [{ id: targetID }, { path: "/Notes/future +%2F.txt" }]) {
    const created = await api.createSubscription({ target, kind: "file", recursive: false, delivery: { type: "sse" } });
    assert.deepEqual(created.filter.target, target);
  }
  assert.deepEqual((await files.subscriptions()).items, [saved]);
  assert.deepEqual(await api.subscription(subscriptionID), saved);
  assert.deepEqual(await api.renewSubscription(subscriptionID), saved);
  await api.deleteSubscription(subscriptionID);
  const actions = calls.filter(({ input }) => input !== "/api/files/me");
  assert.equal(actions.length, 6);
  assert.equal(actions[4].url.pathname, `/api/files/subscriptions/${subscriptionID}/renew`);
  assert.equal(actions[4].init.method, "POST");
  assert.equal(actions[5].init.method, "DELETE");
  for (const { init } of actions.filter(({ init }) => init.method)) assert.equal(init.headers.get("X-CSRF-Token"), "session-token");
  const events = new URL(api.subscriptionEventsURL(subscriptionID), "https://drive.example");
  assert.equal(events.pathname, `/api/files/subscriptions/${subscriptionID}/events`);
  assert.equal(events.search, "");
  assert.equal(calls.length, 7); // Constructing an SSE URL opens no socket and advances no cursor.
});

test("ambiguous or empty snapshot and subscription selectors fail before CSRF or network access", async (t) => {
  const { api } = await client();
  const calls = capture(t, () => { throw new Error("invalid selectors reached the network"); });
  for (const target of [{}, { id: "" }, { path: "" }, { id: targetID, path: "/Notes" }, { id: targetID, extra: true }]) {
    assert.throws(() => api.createChangeSnapshot({ target, kind: "folder", recursive: true }), invalidSelector);
    assert.throws(() => api.createSubscription({ target, kind: "folder", recursive: true, delivery: { type: "sse" } }), invalidSelector);
  }
  assert.equal(calls.length, 0);
});

test("owner audit uses plain cookie resources with no file CSRF bootstrap and preserves denial Problems", async (t) => {
  const { api, FileApiError } = await client();
  const entry = { id: "audit-id", action: "read", outcome: "completed", allowed: true, status: 200, bytesWritten: 0, actor: { kind: "owner" }, origin: "http", observedAt: "2026-11-04T08:00:00Z" };
  const calls = capture(t, ({ url }) => url.searchParams.get("cursor") === "denied"
    ? json({ type: "urn:eam:error:forbidden", title: "Forbidden", status: 403, code: "forbidden", detail: "An owner session is required.", requestId: "req_test" }, 403)
    : json({ items: [entry], nextCursor: null }));
  assert.deepEqual(await api.audit({ cursor, limit: 9 }), { items: [entry], nextCursor: null });
  assert.equal(calls[0].url.pathname, "/api/audit");
  assert.equal(calls[0].url.searchParams.get("cursor"), cursor);
  assert.equal(calls[0].url.searchParams.get("limit"), "9");
  assert.equal(calls[0].init.credentials, "same-origin");
  assert.equal(calls[0].init.headers.has("Authorization"), false);
  assert.equal(calls[0].init.headers.has("X-CSRF-Token"), false);
  await assert.rejects(api.audit({ cursor: "denied" }), (error) => error instanceof FileApiError && error.status === 403 && error.code === "forbidden");
  assert.equal(calls.length, 2);
});
