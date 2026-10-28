# everything about me

A personal data base: **an email-backed account system + a file layer + one self-organizing stream**.
Accounts, sessions, the directory tree, attributes, search, and the stream live in Postgres; file bytes live in
Tencent Cloud COS.

Frontend and backend are split: **a Go backend (net/http) + a React frontend (Vite + React Router)**, with no
Next.js.

```
browser ──session cookie──▶ Go API ──▶ Postgres (users / sessions / nodes)
   │                          │
   │                          └──▶ COS (drive/<userID>/…)
   └──STS temporary credentials, direct upload──▶ COS
```

## Stack and layout

| Path | Contents |
|---|---|
| `backend/` | Go 1.26+. Standard library `net/http` (Go 1.22 routing patterns), pgx, Tencent Cloud COS SDK. |
| `web/` | React 19 + Vite + React Router (data router / loader). |
| `sql/` | Migrations run in filename order. |
| `email-templates/` | Reference SES verification code email templates, English and Chinese (sign-in / sign-up / email change share one per language). |
| `docs/` | Design drafts and screenshots. The stream design is in [`docs/design/timeline.md`](docs/design/timeline.md). |

The backend is one binary that serves both the JSON API and the built frontend:

```bash
make build          # build web/dist, then compile bin/eam
./bin/eam server    # production: / serves the SPA, /api/* serves the API
```

In development the frontend runs on Vite (5173, proxying `/api` to 8080) and the backend runs on Go (8080):

```bash
make dev
```

## User system

- **Email code sign-in and sign-up are merged**: enter an email → receive a code → existing accounts sign in, missing accounts are created on the spot. There are no usernames, and by default no password either.
- Every account has a **unique, immutable `users.id` (a UUID)** as its internal identity; users never need to care about it, and on screen the email is the only identifier. Changing the email leaves the id and file ownership untouched.
- Passwords are **optional**: the account page lets you set, change, or remove one; once set, sign in at `/login?mode=password` (the login page does not show the password entry). Passwords are salted and hashed with Go's `scrypt`, and **the format is exactly the same as before the rewrite** (`scrypt$N$r$p$salt$hash`), so old passwords keep working.
- The session is an httpOnly cookie (`eam_session`); the database stores only the token's SHA-256, and it expires after 30 days by default. The token format also matches the old version, so existing sessions survive the migration.
- Files are isolated by `owner_id`, and each user gets a separate prefix in COS, `drive/<userID>/`, so different accounts can have paths with the same name.
- `/account` lets you change the email (which takes **two verification codes, one for the current address and one for the new one**) and set or remove a password; the page only shows the email and the signup date.
- Registration is open by default; on a personal instance, once you have registered yourself it is worth putting `ALLOW_REGISTRATION=false` in `.env` and restarting.

## Why not "just use S3"

S3 can only list by prefix, cannot search, and has no tags. The `nodes` table fills that layer in:
it is an **index**, not a copy — object bytes only ever live in COS.

## Layout conventions

| Prefix | Purpose |
|---|---|
| `drive/<userID>/` | User-visible files; the key is the real path |
| `_inventory/` | COS inventory output (optional) |

The key sits directly on the path (`drive/<userID>/Photos/2024/a.jpg`), which means:

- you can drop files straight into the bucket with `coscli` / `rclone`, run `reconcile --owner` once, and they show up on the site;
- if you stop using this site one day, a `coscli sync` pulls down the complete directory structure without running any program;
- deleting an account cascades the index away in the database, but the COS objects must be cleaned up by prefix yourself.

## The UI

- **Bilingual**: the locale follows the system/browser preferred language (`zh` → Chinese, everything else → English) with no language switcher; the stream's months, card subtitles, and relative times follow it too
- Three pages — sign in / sign up (merged into one) / account — that follow the system light/dark setting
- `/` is a flat event stream: one card is one event (a photo / a sentence / a group of numbers / an app's own HTML), with no year or month headings and no statistics
- `/files` is the file browser: the open folder is the URL (`/files/Photos/2024`), with breadcrumbs, relative times, type icons and hover actions
- Every file has its own page at `/files/<path>`, built like a folder page (same header, breadcrumbs and title block): an inline preview for images, PDF, Office (docx / xlsx / pptx), plain text and source code, audio and video, plus type, size and timestamps;
  the viewer ([open-file-viewer](https://github.com/xushanpei/open-file-viewer)) plus pdfjs go through `React.lazy`,
  so nothing downloads on first paint and a single chunk (~191KB gzip) is fetched when you open one, with the per-format renderer loading on top of that; formats without a preview still offer a download
- Drag / multi-select upload: placeholder rows appear immediately, with a queue, progress, and speed, cancelable, retryable on failure; `/` focuses search
- Search matches both the filename and the full path, and each result shows its directory on the right
- New directories use an inline input, and deletion has a confirmation dialog (it is permanent)
- One shared top bar on every signed-in page: brand, `Home` / `Files` navigation with the current page highlighted, the account chip, and the file page's search and upload actions
- The current folder or file is the URL itself (`/files/...`), so you can share or refresh straight into it; old `/drive?path=...` links redirect
- Frontend routes: `/` (stream), `/files` (file browser), `/files/<path>` (folder or file detail), `/login` (code sign-in, `?mode=password` goes straight to password sign-in), `/account`

## Getting started

Prerequisites: **Go 1.26+**, **Node 20.19+** (only for building the frontend), and a Postgres.

```bash
cp .env.example .env      # fill in DATABASE_URL / COS_*
make install              # go mod download + frontend npm install
make migrate              # create tables: nodes / users / sessions / email_codes
make check                # self-check: database / COS auth / STS issuing / SES
make dev                  # backend :8080 + frontend :5173 (/api is proxied)
```

Open http://127.0.0.1:5173, enter your email, and receive a code; the first verification creates the account
automatically. Without SES configured in development, the code is printed in the backend terminal that
`make dev` runs and is also filled into the login page for you.

You can also run it in two terminals:

```bash
cd backend && go run ./cmd/eam server   # API + static files
cd web && npm run dev                   # Vite dev server
```

> Backend commands read the repo root's `.env` automatically (searching up to three directories up from the current one), so you never export by hand.
> After `make build`, `./bin/eam server` runs the whole site in one process: Go serves `web/dist` as an SPA and falls back to `index.html` for unknown paths.

### Sign-in / sign-up verification codes (Tencent Cloud SES)

Sign-in, sign-up, and email changes all share the same 6-digit email verification code. With SES configured it sends real mail; **without SES**, development prints the code to the server log and fills it into the login page, while production refuses to send at all.

> In development mode the code is returned straight to the browser, so never expose a development server to the public internet.
> Production behavior (Secure cookies, refusing to send codes without SES) only kicks in when `APP_ENV=production`.

1. Verify the sending domain / sender address in the Tencent Cloud SES console (including SPF and DKIM).
2. Create the verification code template. `email-templates/` has ready-to-paste copies in both languages:
   `sign-in-code.en.txt` / `.en.html` (default) and `sign-in-code.zh.txt` / `.zh.html`.

   Note the template ID. Sending through the SES API requires a template (the `Simple` field is deprecated).
3. Fill in `.env`:

   ```bash
   SES_REGION=ap-guangzhou
   SES_FROM_EMAIL_ADDRESS="Everything About Me <noreply@yourdomain.com>"
   SES_TEMPLATE_ID=123456          # default (English) template
   SES_TEMPLATE_ID_ZH=123457       # optional: Chinese template
   SES_TEMPLATE_VAR=code
   SES_SUBJECT=Everything About Me verification code
   SES_SUBJECT_ZH=Everything About Me 验证码
   # when SES_SECRET_ID / SES_SECRET_KEY are empty, the COS keys are reused
   ```

   Keep the template body generic (see `email-templates/sign-in-code.en.txt`) and do not hard-code "sign-up" into it; otherwise sign-in and email changes would also receive sign-up wording.
   The verification email also **does not distinguish sign-in from sign-up**: the backend can tell whether an address is already registered when it sends a code, but baking that into the email would leak whether an account exists for that address, so one template serves all cases.

4. When reusing the COS sub-account key, attach a policy that grants **only send permission** to that sub-user (CAM → Users → Attach policy; or create a custom policy → create by policy syntax):

   ```json
   {
     "version": "2.0",
     "statement": [
       {
         "effect": "allow",
         "action": ["ses:SendEmail"],
         "resource": ["*"]
       }
     ]
   }
   ```

   If that SecretId is a root account key it already has permission and needs no extra policy; but root keys are far too broad, so switch to a sub-account. The preset `QcloudSESFullAccess` policy works too, it is just broader.

The template variable name must match `SES_TEMPLATE_VAR`; `SES_SUBJECT` sets the subject of the default
(English) email. The backend picks the language per request from the browser's `Accept-Language` header:
`SES_TEMPLATE_ID_ZH` / `SES_SUBJECT_ZH` are used when configured, and English is the fallback. Keep the
template variable identical in both templates.
Codes expire after 10 minutes, allow at most 5 attempts, and issuing a new code invalidates the old one automatically.

### Upgrading from an older version (claiming existing files)

Data from before the multi-user change has an empty `owner_id` and does not automatically show up in any account. Sign in with a verification code (which creates the account automatically), then run:

```bash
cd backend
go run ./cmd/eam claim --email you@example.com --dry-run   # first see which objects would move where
go run ./cmd/eam claim --email you@example.com             # the real claim
```

It copies COS objects from `drive/<path>` to `drive/<userID>/<path>`, rewrites the keys and `owner_id` in the
database, and then deletes the old objects (`--keep-source` keeps them). For a lot of data, move it directory by
directory with coscli first, then run `reconcile --owner you@example.com`.

**Migrating from the Next.js version touches no data**: the schema, COS keys, password hashes, and session tokens are all unchanged, so just start the new `eam` binary and users do not have to sign in again.

### Three things to configure on the COS side

1. **CORS** (to allow browser direct uploads and to let previews read directly)
   - Origins: `http://localhost:5173`, `http://127.0.0.1:5173`, your production domain
   - Methods: `PUT, GET, POST, HEAD`
   - Headers: `*`; exposed headers: `ETag, x-cos-request-id`
   - `GET` is not only for downloads: file previews use `fetch` and follow the 302 from `/api/fs/download` to COS,
     so without a GET CORS rule on the bucket previews get no bytes (the browser console reports CORS and the overlay shows a load failure)
2. **Sub-account + STS permission**: keep root account keys on the server only; `eam` issues temporary credentials scoped to a single object key (30–60 minutes).
3. **(Optional) Cloud Infinite**: once enabled, images can produce thumbnails through `?imageMogr2/thumbnail/300x` without running Sharp yourself.

## Loading terabytes of data the first time

Do not use web upload. Put the data straight into the bucket with the official CLI:

```bash
coscli cp -r ~/Pictures cos://your-bucket-1250000000/drive/<userID>/Pictures
cd backend
go run ./cmd/eam reconcile --owner you@example.com --dry-run   # first see how many rows it would write
go run ./cmd/eam reconcile --owner you@example.com
```

At millions of objects, switch to a COS **inventory** to avoid the request fees of a full List:

```bash
go run ./cmd/eam reconcile --owner you@example.com --inventory ./inventory.csv
```

`reconcile` does three things: add new entries, update the ones that changed (size/etag), and drop records whose
objects have disappeared from the bucket. It only scans the given user's prefix and never touches other accounts;
it also never deletes any object — the power to delete stays with you.

## Deletion

Deletion is permanent: the object and the index go together, with no trash. The web UI asks for confirmation
first; in the API, deleting a directory requires an explicit `?recursive=1`, which is a guardrail against slips.

> Upgrading from an older version: if the database still has rows with a non-null `deleted_at`, run
> `eam purge --days 0` once with the old binary to delete the objects too, then run `migrate`. The migration
> removes that column for good.

## API

### The public API (for third-party apps / scripts / AI agents)

An API of our own: one Bearer token, CRUD over a path tree. The design and tradeoffs are in
[docs/api/api.md](docs/api/api.md) and [docs/api/flows.md](docs/api/flows.md).

```bash
# first issue a token for an app that can only touch Apps/badminton
cd backend && go run ./cmd/eam token create \
  --email you@example.com --name badminton --prefix Apps/badminton

# after that it is just plain HTTP
curl -H "Authorization: Bearer eam_..." https://your-host/list?path=Apps/badminton
```

File content lives under `/api/files/<path>` because `/files` is the web app's own route; every other public
endpoint stays at the root.

| Method | Path | Description |
|---|---|---|
| GET | `/capabilities` | No auth: limits, capabilities, rate limits (clients read them here, do not hard-code) |
| GET | `/list?path=&limit=&cursor=` | List a directory (keyset pagination, includes `writable`) |
| GET | `/api/files/<path>` | 302 to a presigned download URL; `?json=1` returns the URL |
| HEAD | `/api/files/<path>` | Metadata (length / type / ETag / modification time) |
| PUT | `/api/files/<path>` | Write a file (≤ 32 MiB; never overwrites by default, add `?overwrite=1` to overwrite) |
| DELETE | `/api/files/<path>` | Permanent delete (directories need `?recursive=1`) |
| POST | `/mkdir` | `{"path"}` creates a directory (parents created automatically; an existing directory returns 200, idempotent) |
| POST | `/move` | `{"from","to","overwrite"}` move / rename |
| POST | `/copy` | `{"from","to","overwrite"}` copy (files and directories) |
| GET | `/move/{id}` · `/copy/{id}` | Progress of a directory operation (`POST .../cancel` · `.../retry`) |
| GET | `/search?q=&path=&mime=&type=` | Search filenames and paths |
| POST | `/upload` | Create an upload session and return presigned PUT URLs (multipart above the threshold; overwriting needs `overwrite: true`) |
| GET | `/upload/{id}` | See how far it got (the parts already received) and resume after a disconnect |
| POST | `/upload/complete` | Confirm the upload; verifies the size, then writes the index |
| GET | `/me` | Current account, token permissions, usage |
| GET | `/s/<code>` | Save link for agents: returns only a token and a prompt; saving goes through `/api/files` or `/upload` |

Large file flow: `POST /upload` for URLs → PUT straight to COS (never through this server) → `POST /upload/complete`.
Writes are recorded in `audit_log`; the rate limit is 600 requests per minute per token.
Uploads above the threshold automatically go multipart (one presigned URL per part, merged at `complete`).

Long operations all use the standard `Prefer: respond-async[, wait=N]`: if the server finished it answers `200`,
if not it answers `202` + `Location` (pointing at the action's own `/{id}`), and the client needs one branch.
There is no "task" concept on the outside, and no object `id` — the path is the identity.

Save links (for agents):

```text
https://your-host/s/abcd2345efgh2345
```

The URL itself is the token plus the instructions: `GET` only returns a prompt and accepts no content. An agent
opens it, gets the token, and saves through the existing file API: short text with `PUT /api/files/Inbox/...`; images,
videos, and PDFs with `POST /upload` for a URL that uploads straight to COS, bytes never passing through the
server. The account page can mint as many as you like, show recent use, and revoke them at any time; a revoked
link stops working immediately. The short code has no `eam_` prefix and also works directly as a Bearer token for
the full file API. The account page's "recent activity" lists who changed which path and when; save links and
event submissions both land in `audit_log`.

Token management:

```bash
eam token list   --email you@example.com
eam token revoke --email you@example.com --id <token ID>
```

### The API the web UI uses

Every `/api/fs/*` endpoint requires a session and only touches the current account's own files (an anonymous
request gets 401). URLs and the response envelope (`{ok, data}` / `{ok, error}`) are exactly the same as before
the rewrite.

| Method | Path | Description |
|---|---|---|
| POST | `/api/auth/code` | Send a sign-in/sign-up verification code; in development without SES it returns `devCode` |
| POST | `/api/auth/verify` | Sign in with a code; creates a passwordless account when the email is unknown and registration is open |
| POST | `/api/auth/login` | Password sign-in (only for accounts that have a password) |
| POST | `/api/auth/password` | Set/change the password; requires the current password when one exists |
| DELETE | `/api/auth/password` | Remove the password (requires the current password) |
| POST | `/api/auth/logout` | End the current session |
| GET | `/api/auth/me` | Current user (including `hasPassword`); anonymous requests return `user: null` |
| GET | `/api/account` | Account page summary (current user + count of unclaimed legacy files) |
| POST | `/api/auth/email/current-code` | Send an email-change code to the **current address** (requires a session, used to confirm identity) |
| POST | `/api/auth/email/code` | Send an email-change code to the **new address** (requires a session) |
| POST | `/api/auth/email` | Change the email (requires codes for both the current and the new address) |
| GET | `/api/timeline?cursor=&limit=` | Read the stream: a flat event feed, keyset paginated newest first, with presigned thumbnails |
| DELETE | `/api/events/{id}` | Delete one event (a tombstone, so it cannot come back) |
| GET | `/api/timeline/status` | Organizing progress (unfinished tasks, files organized, earliest/latest month) |
| POST | `/api/timeline/rebuild` | Force a full re-organize of the current account (drops the analysis results and backfills from scratch) |
| GET | `/api/fs/list?path=` | List the current level (Postgres only, not the bucket; also returns the path's own `node`, so the UI can tell a folder from a file) |
| GET | `/api/fs/search?q=` | Fuzzy search by name/path |
| POST | `/api/fs/upload/sign` | Get STS temporary credentials for a browser direct upload |
| POST | `/api/fs/upload/confirm` | Writes the index after a HeadObject check |
| GET | `/api/fs/download?path=` | 302 to a presigned URL (`&json=1` returns JSON) |
| POST | `/api/fs/mkdir` | Create a directory (writes a zero-byte placeholder object) |
| POST | `/api/fs/rename` | Rename/move a single file; use coscli for directories |
| POST | `/api/fs/delete` | Permanent delete (every record in the subtree) |
| GET | `/api/health` | Liveness probe |

The auth write endpoints validate `Origin`/`Host` and rate limit in in-process memory (code sends: 30 per hour per
IP, 5 per hour per email, with a 60-second cooldown per email; code verification: 30 per 15 minutes per IP;
password sign-in: 30 per 15 minutes per IP, 10 per 15 minutes per email).

## Environment variables

| Variable | Description |
|---|---|
| `DATABASE_URL` | Postgres connection string |
| `COS_SECRET_ID` / `COS_SECRET_KEY` | Server-side keys; never give them to the frontend |
| `COS_BUCKET` / `COS_REGION` | The bucket must include the APPID |
| `APP_ENV` | `development` (default) or `production`; decides Secure cookies and the behavior when SES is not configured |
| `SESSION_TTL_DAYS` | Session lifetime, 30 days by default |
| `TIMELINE_ENABLED` | Whether events get organized in the background (grouping photos into events), `true` by default; set it to `false` to keep only the read endpoints |
| `TIMELINE_WORKERS` | Background organizer concurrency, 2 by default |
| `TIMELINE_MONTH_BUDGET` | How many display units a month may occupy at most (1 unit ≈ 72px), 24 by default |
| `TIMELINE_TZ` | Which day an event's time is bucketed into, `Asia/Shanghai` by default |
| `COOKIE_SECURE` | Secure is added automatically in production; set it to false for local http debugging |
| `ALLOW_REGISTRATION` | Set to false to close registration; open by default |
| `ADDR` / `PORT` | Listen address; `:8080` by default (`ADDR` wins) |
| `WEB_DIR` | Frontend build output directory; when empty it finds the repo's `web/dist` |
| `USER_QUOTA_BYTES` | Storage limit per account in bytes; 0 or empty means unlimited |
| `UPLOAD_MULTIPART_THRESHOLD` | Above how many bytes uploads go multipart, 5 GiB by default |
| `UPLOAD_PART_SIZE` | Part size, 64 MiB by default (enlarged automatically if that would exceed 1000 parts) |
| `SES_REGION` | Tencent Cloud SES region, `ap-guangzhou` by default |
| `SES_FROM_EMAIL_ADDRESS` | A sender address already verified in SES; may include a display name, as in `Everything About Me <noreply@yourdomain.com>` |
| `SES_TEMPLATE_ID` | Verification code template ID (default / English) |
| `SES_TEMPLATE_ID_ZH` | Optional Chinese template ID; falls back to `SES_TEMPLATE_ID` |
| `SES_TEMPLATE_VAR` | Template variable name, `code` by default |
| `SES_SUBJECT` | Subject of the default (English) verification email |
| `SES_SUBJECT_ZH` | Subject of the Chinese verification email |
| `SES_REPLY_TO_ADDRESS` | Optional reply-to address |
| `SES_SECRET_ID` / `SES_SECRET_KEY` | Optional; when empty the COS keys are reused |

## Ops commands

Every backend maintenance script lives in one binary (`make` is just a shortcut):

```bash
cd backend && go run ./cmd/eam --help          # or ./bin/eam --help
go run ./cmd/eam migrate                       # create / upgrade tables
go run ./cmd/eam doctor [--write]              # self-check (`--write` writes a probe object, then deletes it)
go run ./cmd/eam reconcile --owner … [--inventory …] [--dry-run]
go run ./cmd/eam claim --email … [--dry-run] [--keep-source]
go run ./cmd/eam token create --email … --name … [--prefix …] [--write=false]
go run ./cmd/eam token list   --email …
go run ./cmd/eam token revoke --email … --id …
```

`make` has matching targets:

| Target | What it does |
|---|---|
| `make install` | Install dependencies the first time (Go modules + frontend npm) |
| `make dev` | Start the Go backend and Vite together |
| `make build` | Build the frontend + compile `bin/eam` |
| `make migrate` / `make check` / `make check-write` | Map to `eam migrate` / `eam doctor` |
| `make test` | Go tests + frontend typecheck |

## Known tradeoffs

- **Without SES configured, production cannot do code sign-in/sign-up**: development falls back to codes in the log; configure SES and a template before going live. Accounts that already have a password can still sign in with it.
- **Changing the email requires two codes, for both the current and the new address**: a session alone cannot change it, and the old address receives no change notice (a notification email could be added later).
- **Passwords are optional**: once set you can sign in with one; an account without a password depends entirely on the email being reachable.
- **A forgotten password is not fatal**: sign in with an email code and reset it on the account page; but losing the email itself leaves no recovery path (no backup email, no recovery codes).
- **Rate limiting is in-process memory**: with multiple instances each keeps its own count, so strict limiting needs Redis.
- **Renaming a directory** copies objects one at a time on the server, which takes ages at TB scale. Change it with coscli first, then reconcile.
- **No instant upload (dedup)**: computing a full SHA256 over TBs means reading all the data once. If we ever do it, use `size + hash of the first and last 1MB`.
- **Legacy data must be claimed by hand**: `eam claim` is O(number of objects) server-side copying; run `--dry-run` first.
- **Deleting an account** only cascades database records; the objects under `drive/<userID>/` in COS must be cleaned up by prefix yourself.
- **The frontend is a pure SPA**: first paint waits for the JS to load (~110KB gzip, with the COS SDK loaded on demand); there is no SSR, but there is also no need to render user data on the server.
- **Automatic events are heuristic for now**: they only look at path/size/type/time in the index and never read EXIF/GPS or image content; analysis, grouping, scoring, and naming are all interfaces (`Analyzer` / `Grouper` / `Scorer` / `Narrator` in `internal/timeline`), so wiring in multimodal models or an LLM is a matter of swapping the implementation. The health CSV is a built-in `AmbientExtractor`.
- **The event structure is in [`docs/design/events.md`](docs/design/events.md)**: one event = a time + one card (`photo` / `note` / `metric` / `html`) + an optional detail link; third-party apps submit through `POST /events` with a token that has `can_events`.
- **Old databases need file times backfilled**: `nodes.source_modified_at` is new in this change, and legacy rows only get it after one `reconcile` run; until then the stream falls back to the index update time.
- **"Daily video" detection is heuristic**: videos in the same directory spanning ≥7 days with ≥7 videos get folded into one series card; when the guess is wrong, a manual override could come later, or a model could replace it.
- **Moving or copying a directory does not refresh the stream immediately**: the background sweeper scans recent changes every 20 minutes; if you want it now, use `POST /api/timeline/rebuild`.
- **Events have no in-site editor yet**: right now the only sources are automatic (photo sessions) and third-party submissions; hand-written events, health-change cards, and merging are all still missing.
- **Upgrading to the event model takes one migration run**: `sql/013_events.sql` creates `events` / `event_media` and marks the existing monthly snapshots stale; the next sweeper pass (or `POST /api/timeline/rebuild`) recomputes each month's pieces into events. Before the migration the stream is empty.
- **`cos-js-sdk-v5` has an audit advisory in its XML parser dependency**: the official SDK pins `fast-xml-parser@4.5.0`, which `npm audit` reports as critical. It only parses XML returned by COS (over TLS), and there is currently no lossless upgrade path; follow up once the official SDK upgrades.
