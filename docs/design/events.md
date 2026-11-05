# Events: structure and origins

> Status: v1.0 · 2026-10 · **P0 shipped** (implementation status in §12)
> The UI is in [`timeline.md` §17](timeline.md); this file answers only: **what an event looks like, who can write one, and who owns it once written.**
> There are no events yet. The site will write some, third-party apps will write some; let's fix the contract first.
>
> **Storage decision, 2026-10-30:** [storage.md](storage.md) governs file/media delivery. Presigned thumbnails
> below describe the current COS implementation; the accepted target serves thumbnails and previews through
> the configured API origin, with provider processing and CDN acceleration internal.

---

## 0. One line

An event = **a time span + one card (type + payload) + an optional detail link**; the origin is just attribution.

---

## 1. Four decisions

### 1.1 The timeline owns events; apps only submit them

An event written by a third party lives in the user's account, not as a foreign link owned by the app:

- The user disconnects the app, the app shuts down, the token is revoked — the events are all still there, only the origin marker goes stale (`Source disconnected`);
- When the user deletes an event, write a tombstone (`state='deleted'`): content is cleared and the dedupe key is kept — otherwise a pipeline rerun or an app re-push would resurrect it;
- When an app calls delete, the event is only frozen (`detached`); the content is kept and not taken away.

This is exactly where "a personal chronicle" and "a third-party activity wall" part ways. Doing it the other way (events belong to the app and vanish on disconnect) would turn the timeline into an unstable aggregator.

### 1.2 Cards are typed, not one fixed set of slots

Each card type has its own `type` and `payload`; the server validates by type, and the renderer emits by type. Four are built in:

| type | payload | What it looks like | Typical |
|---|---|---|---|
| `photo` | `{ media: 1–4, body?, meta? }` | image (1 full-width / a 2–4 collage) + one sentence | "Qinghai Lake · four days" |
| `note` | `{ body, meta? }` | plain text on a light background | "Moving house" |
| `metric` | `{ metrics: 1–3, body? }` | one row of numbers + one sentence | "Morning run · 12.4 km" |
| `html` | `{ html, height }` | the app's own layout, rendered in a sandbox | a ticket stub, a poster, a small report |

All types share three fields: `title`, the time, and `detail_url` (optional). The type decides content and layout; the shared fields decide how the event slots into the timeline.

**`html` is just another type, not an exception.** There is always something that cannot be expressed; rather than forcing apps to cram their content into three templates, give them an isolated canvas.

### 1.3 No editing

An event is finished once it is written:

- No user editing — no retitling, no swapping the cover, no drag-and-drop ordering, and therefore no `locked_fields` and none of that whole class of "the user changed it, so don't overwrite it" conflicts;
- A producer may **re-push** the same `external_id` to overwrite its own event (that is an update, not an edit);
- The user may delete (that is not an edit).

The whole "user edits have the highest priority" chapter from the previous design can be deleted outright. Dropping one feature drops four things with it: field locks, merge conflicts, undo, and shadow versions.

### 1.4 `html` needs isolation, not just filtering

Third-party HTML straight into our DOM would hand over the layout, dark mode, and mobile at once, on top of needing XSS protection. So:

- Render it in `<iframe sandbox>` (without `allow-scripts`), with the height given by the payload and clamped by the server;
- Inject a set of design tokens into the document (`--eam-bg` / `--eam-text` / `--eam-line` / `--eam-accent`); a card **may** use them to follow the light/dark theme, or ignore them entirely;
- Images are restricted to `https:` and `data:`; external scripts never execute;
- Height cap is 640px; overflow scrolls inside the card.

---

## 2. Data model

```sql
create table if not exists events (
  id            uuid primary key default gen_random_uuid(),
  user_id       uuid not null references users(id) on delete cascade,

  -- attribution
  origin        text not null default 'native',      -- native | auto | app
  token_id      uuid references api_tokens(id) on delete set null,
  external_id   text,                                -- the event id inside the app
  producer_key  text,                                -- auto: the pipeline's dedupe key

  -- time
  starts_at     timestamptz not null,
  ends_at       timestamptz,
  tz            text not null,
  local_date    date not null,                       -- the date of starts_at in tz, used for bucketing
  precision     text not null default 'day',         -- day | minute

  -- card
  type          text not null,                       -- photo | note | metric | html
  title         text not null,                       -- all types have it; used in lists/notifications/a11y
  payload       jsonb not null,                      -- varies by type, see §3
  detail_url    text,                                -- optional: the detail page for this event

  -- state
  state         text not null default 'published',   -- published | detached | deleted (tombstone)
  merged_into   uuid references events(id) on delete set null,
  created_at    timestamptz not null default now(),
  updated_at    timestamptz not null default now()
);

create unique index if not exists events_app_ext
  on events (user_id, token_id, external_id)
  where external_id is not null;

create unique index if not exists events_auto_key
  on events (user_id, producer_key)
  where producer_key is not null;

create index if not exists events_time
  on events (user_id, local_date desc, starts_at desc)
  where state = 'published';

-- Media: payloads carry paths, resolved to nodes on write; deleting a file degrades the card on its own
create table if not exists event_media (
  event_id uuid not null references events(id) on delete cascade,
  idx      int not null,
  node_id  uuid not null references nodes(id) on delete cascade,
  primary key (event_id, idx)
);
create index if not exists event_media_node on event_media (node_id);
```

Key points:

- **`payload` is jsonb**: few types, few fields — not worth a table per card type; validation lives in the app layer, and the renderer reads by `type`.
- **`local_date` is computed on write**, never converted by time zone on read, so traveling across zones does not drift days.
- **Media can only be files in the drive**: a third party that wants to attach images first uploads them into its own prefix with its own token (e.g. `Apps/strava/2025/0803.jpg`) and then references the path in the event. No hotlinked images — hotlink protection, link rot, moderation, and SSRF are all problems.
- **There is exactly one `detail_url`**, and it means "the detail page for this event". If there isn't one, there isn't; don't guess.
- `merged_into` is reserved for later (§10).

### 2.1 Relationship to the existing tables

- `timeline_moments` → **replaced by `events`**. During the migration, rows are loaded as `origin='auto'` with `producer_key=dedupe_key`.
- `timeline_snapshots` → **dropped**. No budget, no density; the read path queries `events` by `local_date` directly, and recomputing a snapshot on every app write would only be more expensive.
- `timeline_assets`, `timeline_series` / `timeline_points` → kept (file times, health data).

---

## 3. The four built-in types

Every payload is pure data, no HTML (except the `html` type).

### 3.1 `photo`

```json
{
  "media": ["Photos/2025/06/12/a.jpg", "Photos/2025/06/12/b.jpg"],
  "body": "On day three we climbed to 4,100 m and the wind knocked the tripod over.",
  "meta": "780 photos → 9 · 12.6 km",
  "cover": 0
}
```

- `media` holds 1–4 items; 1 is a 3:2 full-width image, 2–4 are collaged automatically;
- `cover` is an index into `media`; it defaults to 0;
- `meta` is the small gray line (photo count, number of people, distance), optional.

### 3.2 `note`

```json
{ "body": "Moved to the west side. The first night there was only a mattress and one box; the elevated road was outside the window and cars went by all night.", "meta": "214 files" }
```

One card of plain text. `body` ≤ 500 characters.

### 3.3 `metric`

```json
{
  "metrics": [
    { "label": "Distance", "value": "12.4", "unit": "km" },
    { "label": "Pace", "value": "5'02\"", "unit": "/km" },
    { "label": "Heart rate", "value": "148", "unit": "bpm" }
  ],
  "body": "Pace dropped to 5'30\" over the last two kilometers."
}
```

1–3 numbers rendered as one row; `value` is a string, so forms like `5'02"` and `12.4` are allowed.

### 3.4 `html`

```json
{
  "html": "<div class=\"ticket\">…</div>",
  "height": 172
}
```

- Rendered in `<iframe sandbox>`, see §1.4;
- `height` is 40–640, clamped by the server; the width is always 100% of the container;
- Design tokens and `<meta charset>` are injected into the document first; nothing else is injected;
- `<script>`, `<iframe>`, `<object>` are not allowed; `<style>`/`<link>` may only be inline (external stylesheets do not load).

> The server still runs an allowlist pass (tags + attributes); the sandbox is a second layer, not the only one.

---

## 4. The detail link

`detail_url` is **optional**, and it **does not become a button**:

- Present: the title itself is the link (underline on hover), opening a new tab;
- Absent: the title is not clickable, and neither is the card;
- None of our cards have one yet — there is no in-site event detail page;
- Third parties decide for themselves whether to add one, pointing at their own activity page, article page, or billing page;
- For `html` cards, the links are decided by their own HTML; we don't touch that.

No in-site detail page. Wait until there is a real need ("multiple images / the original text / the source files"), and don't sketch a screen from imagination.

---

## 5. Three origins

| origin | Who writes | Dedupe key | Typical |
|---|---|---|---|
| `native` | The site (auto-generated or user-created) | none | "Moving house", "Qinghai Lake" |
| `auto` | The file pipeline | `producer_key` | photo sessions, cross-day trips |
| `app` | Third-party apps (Bearer token) | `external_id` | "Morning run", "Ticket stub" |

Same table, same read path. **No second-class "app event" citizen**: the only difference is `origin`, i.e. that line of attribution.

**Feeds (P1)**: not every app will write to the API. Give it an import URL (RSS / Atom / iCal / JSON feed), poll it once a day in the background, and generate events with `origin='app'` keyed by item id. The data model does not change.

---

## 6. Write API

The current event routes below follow the runtime API conventions: Bearer, UTC, and the
existing response conventions. They are not part of the replacement [file API](../api/api.md), which uses
stable node IDs, plain JSON/Problem Details, and same-origin content. During implementation, event media
references and generated instructions must be aligned with that contract; existing path payload examples
here describe today's event API. An app's event dedupe handle remains its own `external_id`.

### 6.1 Create / update (idempotent)

```http
POST /events
Authorization: Bearer eam_live_xxx
Idempotency-Key: strava:run-8921

{
  "external_id": "run-8921",
  "starts_at": "2025-08-03T07:12:00Z",
  "ends_at":   "2025-08-03T08:02:00Z",
  "tz": "Asia/Shanghai",
  "precision": "minute",
  "title": "Morning run · North shore",
  "detail_url": "https://www.strava.com/activities/8921",
  "type": "metric",
  "payload": {
    "metrics": [
      { "label": "Distance", "value": "12.4", "unit": "km" },
      { "label": "Pace", "value": "5'02\"", "unit": "/km" }
    ],
    "body": "24 degrees when I left the house."
  }
}
```

```json
201 { "id": "0f1c…", "url": "https://your-host/e/0f1c…", "state": "published" }
```

- Deduped by `(user, token, external_id)`: the same `external_id` again is a **full replacement**, returning `200` and the same `id`;
- With `Idempotency-Key`, "same key, same result" applies, so retries are safe;
- An event the user deleted, pushed again → `410 gone`, and the app should stop retrying.

### 6.2 The rest

```http
DELETE /events/{id}      # app-side delete → state=detached (content kept)
GET    /api/events?from=&to= # reconciliation: returns the list of external_ids you have written
GET    /events/{id}      # one event (full JSON, for the app to render a share page)
```

No `PATCH`: re-pushing is the update; one endpoint less.

> **Path convention**: as with the file API, a public endpoint's path is the feature itself, with no `/v1` or `/api` prefix (see [api.md](../api/api.md)).
> The web read endpoint remains `GET /api/timeline` (cookie session).

### 6.3 Permissions

```sql
alter table api_tokens add column if not exists can_events boolean not null default false;
```

- When creating a token the user ticks "write events"; the token name is the attribution name (`Strava`, `Readwise`);
- An app can only write what it created, and can only reference media inside its own token prefix (reusing the existing 403 out-of-bounds logic);
- No app registry, no OAuth. If this really opens up as a platform, add an `apps` table and `app_id`; `events.token_id` does not change.

### 6.4 Limits

| Item | Value |
|---|---|
| `title` | ≤ 80 characters |
| `body` | ≤ 500 characters |
| `metrics` | ≤ 3 |
| `media` | ≤ 4 |
| `html` | ≤ 32 KB, height 40–640 |
| Request body | ≤ 64 KB |
| Rate | 60 requests/minute per token, 2,000 events/day |

Limits go into `GET /capabilities`; clients must not hard-code them.

---

## 7. Attribution and display

Only the date line reveals who wrote it:

```
8 月 3 日 · Strava ↗        ← 三方
8 月 15 日                   ← native / auto（不显示）
```

- The name comes from the token's `name`;
- After the token is revoked it shows `Strava (disconnected)`; `detail_url` no longer works, and the content is untouched;
- No app logos, no brand colors; and no "only this app" filter either — if that becomes necessary, it is search's business.

---

## 8. Lifecycle

| Action | Result |
|---|---|
| Producer creates | `state='published'` |
| Producer re-pushes the same `external_id` | payload fully replaced (event id unchanged) |
| Producer deletes | `state='detached'`, content kept, `detail_url` cleared |
| User deletes | `state='deleted'` (tombstone) + content cleared + `event_media` deleted; later re-pushes are ignored |
| Token revoked | events kept; the origin shows "disconnected" |
| User deletes all events from one app | bulk tombstone (offered in settings, not implemented) |

There is no "user modified" row, so there are no locked fields and no merge conflicts.

---

## 9. Phases

| Phase | Scope |
|---|---|
| **P0** ✅ | the `events` / `event_media` migration; the four types; the auto pipeline writes `origin='auto'`; `POST` / `DELETE` / `GET /events`; the origin line; `detail_url`; the web read endpoint switched to a flat event stream |
| **P1** | feed import (RSS / iCal); server-side allowlist sanitizing for html (currently only the sandbox); a reconciliation UI for app events |
| **P2** | merging; more types (checklist / quote / place); an app registry |

`html` sits in P1 because the sandbox, token injection, and height clamping must be right first; once the three `photo`-family types work, the timeline is usable.

---

## 10. Merging (P2, structure only)

A day's photos and one run are two events today. Later they can merge into one, with two origins hanging off the card: `merged_into` points at the primary event, and child events are not displayed.

**No automatic merging before P2.** Better two events than an algorithm insisting that "the same day" means "the same thing".

---

## 11. Undecided

1. **Should there be an in-site event detail page**: no card has a `detail_url` today, so none is clickable. Revisit when "multiple images / the original text / the source files" is a real need.
2. **Deletion granularity for events**: today only one at a time (and the web has no entry point yet). Should there be "delete all events from this app"? Bulk tombstones are cheap, but where to put the entry point is undecided.
3. **Reviewing `html`**: once P1 opens it up, who judges whether an HTML card is phishing? Start with source labeling + user deletion, or open it only to allowlisted apps first?
4. **The boundary between automatic events and app events**: should a day's photos (auto) and a run (app) mention each other on the timeline? Today they don't know the other exists.
5. **How many images per event**: 4 for now. For trips with 9, do we loosen `photo`, or wait for the detail page?

---

## 12. Implementation status

| Thing | Location |
|---|---|
| Schema | [`sql/013_events.sql`](../../sql/013_events.sql) |
| Domain logic (validation / read / write / tombstones) | [`backend/internal/events/events.go`](../../backend/internal/events/events.go) |
| Web read endpoint + public API | [`backend/internal/api/events.go`](../../backend/internal/api/events.go) |
| Producer of automatic events | [`backend/internal/timeline/service.go`](../../backend/internal/timeline/service.go) (`autoEvents` / `PublishAuto`) |
| Frontend | [`web/src/pages/StreamPage.tsx`](../../web/src/pages/StreamPage.tsx) |
| Token capability bit | `api_tokens.can_events`; CLI `eam token create --events` |

Shipped:

- The `events` / `event_media` tables; the token's third capability bit `can_events`;
- Write validation for the four card types (limits centralized in `events.ValidateApp`);
- Idempotent upsert (`external_id`), tombstones (user delete), detached (app delete);
- Media resolution: path → `node`, which must be inside the token prefix; presigned thumbnails on read;
- Web: `GET /api/timeline` returns a flat event stream (keyset cursor), cards render by type, html goes through a sandbox iframe;
- Pipeline: each month's selected moments are written as events with `origin='auto'`.

Not done yet (deliberately deferred by design, not oversights):

- **A native editor**: the site still cannot write an event by hand (native has no producer yet);
- **Feed import** (RSS / iCal);
- **Health change cards**: `timeline_series` is still there, but there is no producer that goes "spot an anomaly → generate a card";
- **Merging** (`merged_into` is only a field);
- **Delete UI**: `DELETE /api/events/{id}` and `DeleteOwned` both exist, but the web has no entry point;
- Server-side allowlist sanitizing for html (currently only the sandbox + CSP).
