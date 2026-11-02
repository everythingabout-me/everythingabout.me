# everything about me

A personal data base: **an email-backed account system + a file layer + one self-organizing stream**.
Accounts, sessions, the directory tree, attributes, search, and the stream live in Postgres; file bytes live in
Tencent Cloud COS.

Frontend and backend are split: **a Go backend (net/http) + a React frontend (Vite + React Router)**, with no
Next.js.

Current browser and Bearer upload implementation:

```
client ──/api/files/uploads + parts + commit──▶ Go API ──▶ admitted Local/COS/S3 tree
                                                   └──▶ Postgres (sessions, receipts, Operations, index)
```

The API acknowledges durable numbered parts, then commits through staged native publication and index/result
settlement. Unknown native effects remain guarded. Small clients may use streaming content PUT. Browser
session identities survive refresh; local File reselection may be required to resume acknowledged chunks.

## Accepted storage architecture

The [architecture](docs/design/storage.md) defines two layers: a unified API consumed by every app,
and storage drivers for local filesystems, S3/COS, cloud drives, WebDAV, and NAS. Control requests and file
bytes use one configured origin: `https://everythingabout.me` when hosted, or the operator's domain/LAN
endpoint when self-hosted. Clients do not receive provider URLs, credentials, or SDK requirements.

The [new file API](docs/api/api.md) and [driver contract](docs/design/storage-driver.md) are clean-slate
specifications with no legacy-client compatibility requirement. The actual storage retains complete native
folders, original accepted filenames and raw file contents below the user root. Users can leave through
ordinary filesystem tools, rclone or provider tools without the database or an Everything export. Postgres
indexes that tree and adds application metadata. Native directory operations are preferred; S3 folder moves
may still require per-file copy/delete. Durable journals reconcile backend changes and index updates.

| Unified file contract | Interface / status |
|---|---|
| All file clients | `/api/files`: one path-based API for Web, iOS, Android, CLI and third parties |
| Authentication (P2 active) | Explicit Bearer, or session cookie; Origin + session-bound CSRF required for mutations |
| Metadata and listing (P2 active) | `GET /api/files?path=/...`; `GET /api/files/list?path=/...` |
| Search and principal (P2 active) | `GET /api/files/search`; `/api/files/me`; `/api/files/capabilities` |
| File content (P2 active) | `GET/HEAD /api/files/content?path=/...`: same-origin streaming, safe single ranges |
| Mutations (P4 admitted roots) | PUT content, POST folder/move/copy, DELETE path; verified baseline and explicit backend capability evidence required |
| Long operations (P4) | `.../operations/{id}` and `/cancel`: durable progress, partial effects, recovery |
| Resumable uploads (P5 admitted roots) | POST/GET `.../uploads`; GET/DELETE `.../uploads/{id}`; PUT `.../parts/{number}`; GET `.../parts`; POST `.../commit` |
| Storage tree (P1/P4) | Native reads and admitted Local stage/publish/mkdir/move/copy/empty-only remove |

CDN/regional gateways and NAS connectors are internal deployment choices. Private cache hits require
authorization; uploads need suitable streaming ingress. Self-hosting can run in one process without a CDN.
See the architecture for performance tradeoffs, the [current implementation plan](docs/design/storage-implementation-plan.md),
and the [predecessor audit](docs/design/storage-implementation-review.md).
P0 foundations are available: [boundary decisions, read-only baseline capture and write-admission runbook](docs/design/storage-p0.md)
and [isolated native capability evidence](docs/design/storage-p0-capabilities.md). They do not activate the replacement file API.

P1 adds [read-only Local/COS/S3 native trees, backend configuration and safe index import](docs/design/storage-p1.md).
`STORAGE_BACKEND=local` or `s3` starts without COS configuration; native reads/import are available internally and
through maintenance commands. P2 now adds [authenticated metadata/search and same-origin file bytes](docs/design/storage-p2.md)
on all three backends. P3 adds [durable journals, guards, quota reservations and native recovery](docs/design/storage-p3.md),
with additive migrations 016/017. P4 adds [admitted Local native writes and Operations](docs/design/storage-p4.md)
with migrations 019/020. Writes default off: configure `FILE_COORDINATED_WRITES=true` only for coordinated
Local writers, validate/import the native index, then run the explicit `eam storage-admit` command with
`--owner-id UUID --writers-quiesced --evidence /secure/proof.json` before serving mutations. No production baseline or writer revocation is automatic. [COS/S3 mutation adapters](docs/design/storage-p4-objects.md)
now add an explicitly attested single-gateway spool strategy: default-off, exact connection probe evidence,
retained native versions, one-shot requests and permanent guards for uncertain remote effects. Real provider
probes were not configured here. Object admission also requires explicit `--physical-bytes` including history.
P5 adds [durable numbered-part ingress](docs/design/storage-p5.md) with migration 024, server SHA-256,
watermarked receipts, restart recovery and P3/P4 publication. Parts are 8 MiB with four concurrent byte
requests and at most 10000 parts. COS/S3 still reject files above their admitted single-publication policy
(default 32 MiB, at most 5 GiB); this release does not claim native multipart publication.
P6 [switches Web/media/generated instructions](docs/design/storage-p6.md) to the same resources and Problems,
removes browser storage SDKs, and retires legacy file routes with 410. [Old authority drain](docs/design/storage-p6-backend.md)
is a separate deployment gate; issued grants and in-flight effects were not revoked by this code change.

The old `/api/files/{path...}` wildcard has been removed. Use `/api/files/content?path=/...` for downloads.
Legacy COS file routes return 410; historical behavior remains in the [legacy reference](docs/api/legacy-api.md).
Server startup requires a stable `APP_SECRET`; production also requires explicit SES credentials.
See the [P1 upgrade steps](docs/design/storage-p1.md#configuration-and-identity-upgrade) before changing
storage/database credentials. Configure `FILE_COOKIE_ORIGINS` for cookie mutations and credentialed CORS;
GET `/api/files/me` bootstraps its session-bound CSRF token. P2 adds no database schema changes.

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
- Folder uploads: drag folders or choose **Upload → Upload folder** to retain the selected root and nested paths. The list shows only immediate children throughout upload; each folder summarizes selected descendant file counts, confirmed files, bytes and percentage. Pending folders are navigable, completed rows lose their progress bars, and folder actions cancel/retry the remaining descendants. Dragged folders also preserve empty directories; the browser folder picker only exposes directories containing files. Existing directories merge, while replacing files requires confirmation.
- Refreshing preserves owner-scoped P5 session IDs, idempotency keys, target intent and Operation waits. Select the original File again when the browser cannot retain its handle; acknowledged parts are hash-verified and skipped. Cancellation reconciles the same session/Operation before a newer mutation can receive bytes. Apply all forward migrations, including `024_file_uploads.sql`, before starting the new binary. See [P6 client acceptance](docs/design/storage-p6.md) and the separate [authority drain](docs/design/storage-p6-backend.md).
- Search matches both the filename and the full path, and each result shows its directory on the right
- New directories use an inline input, and deletion has a confirmation dialog (it is permanent)
- One shared top bar on every signed-in page: brand, `Home` / `Files` navigation with the current page highlighted, the account chip, and the file page's search and upload actions
- The current folder or file is the URL itself (`/files/...`), so you can share or refresh straight into it; old `/drive?path=...` links redirect
- Frontend routes: `/` (stream), `/files` (file browser), `/files/<path>` (folder or file detail), `/login` (code sign-in, `?mode=password` goes straight to password sign-in), `/account`

## Getting started

Prerequisites: **Go 1.26+**, **Node 20.19+** (only for building the frontend), and a Postgres.

```bash
cp .env.example .env      # fill in DATABASE_URL / APP_SECRET / selected storage backend
make install              # go mod download + frontend npm install
make migrate              # create tables: nodes / users / sessions / email_codes
make check                # self-check: database / selected storage / mail configuration
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
   SES_SECRET_ID=<mail-subaccount-id>
   SES_SECRET_KEY=<mail-subaccount-secret>
   SES_REGION=ap-guangzhou
   SES_FROM_EMAIL_ADDRESS="Everything About Me <noreply@yourdomain.com>"
   SES_TEMPLATE_ID=123456          # default (English) template
   SES_TEMPLATE_ID_ZH=123457       # optional: Chinese template
   SES_TEMPLATE_VAR=code
   SES_SUBJECT=Everything About Me verification code
   SES_SUBJECT_ZH=Everything About Me 验证码
   # Explicit SES credentials do not inherit COS keys.
   ```

   Keep the template body generic (see `email-templates/sign-in-code.en.txt`) and do not hard-code "sign-up" into it; otherwise sign-in and email changes would also receive sign-up wording.
   The verification email also **does not distinguish sign-in from sign-up**: the backend can tell whether an address is already registered when it sends a code, but baking that into the email would leak whether an account exists for that address, so one template serves all cases.

4. For the dedicated SES sub-account key, attach a policy that grants **only send permission** to that sub-user (CAM → Users → Attach policy; or create a custom policy → create by policy syntax):

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

### COS connection admission

Clients use the application origin for all file requests and bytes. Browser bucket CORS and STS issuance
are no longer part of the file protocol. Keep provider credentials server-side, then establish the exact
connection's retained-version, single-attempt and coordinated-writer evidence described in
[object admission](docs/design/storage-p4-objects.md). Follow the separate [legacy-authority drain](docs/design/storage-p6-backend.md)
before enabling replacement writes. Unsupported image conversions use authorized raw content or placeholders.

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
first; in the unified API, recursive directory deletion requires explicit `?recursive=true`.

> Upgrading from an older version: if the database still has rows with a non-null `deleted_at`, run
> `eam purge --days 0` once with the old binary to delete the objects too, then run `migrate`. The migration
> removes that column for good.

## API

### The public API (for third-party apps / scripts / AI agents)

The unified authenticated file API serves browser and Bearer clients at `/api/files`; see
[its contract](docs/api/api.md), [P5 limits](docs/design/storage-p5.md), and [P6 cutover](docs/design/storage-p6.md).
Historical protocols are retained in the [legacy reference](docs/api/legacy-api.md) and return 410.

```bash
# first issue a token for an app that can only touch Apps/badminton
cd backend && go run ./cmd/eam token create \
  --email you@example.com --name badminton --prefix Apps/badminton

# after that it is just plain HTTP
curl -G -H "Authorization: Bearer eam_..." --data-urlencode 'path=/Apps/badminton' \
  https://your-host/api/files/list
```

Content uses `GET/HEAD /api/files/content?path=/...`; the old `/api/files/<path>` family is retired.
The table lists the active shared file resources and retained account/Connect entry points.

| Method | Path | Description |
|---|---|---|
| GET | `/api/files?path=` | Path-only native file/folder metadata |
| GET | `/api/files/list?path=&limit=&cursor=` | Shared immediate-child listing |
| GET | `/api/files/search?q=&path=&kind=&mediaType=` | Authorized indexed search with scoped opaque cursors |
| GET | `/api/files/me` · `/api/files/capabilities` | Authenticated scope/usage/read limits and cookie CSRF bootstrap |
| GET, HEAD | `/api/files/content?path=` | Same-origin bytes and representation headers; single Range support |
| PUT | `/api/files/content?path=` | Admitted Local create/conditional replacement after staged bytes and durable settlement |
| POST | `/api/files` · `/api/files/move` · `/api/files/copy` | Admitted Local folder creation and finite native move/copy, no target merge |
| DELETE | `/api/files?path=&recursive=true` | Captured native deletion; explicit recursion for nonempty folders |
| GET, POST | `/api/files/operations/{id}` · `.../cancel` | Durable state/result and safe cancellation |
| POST, GET | `/api/files/uploads` | Create/list persistent principal-bound upload sessions |
| GET, DELETE | `/api/files/uploads/{id}` | Compact durable status or open-session abort |
| PUT | `/api/files/uploads/{id}/parts/{number}` | Exact immutable numbered part and SHA-256 receipt |
| GET | `/api/files/uploads/{id}/parts?limit=&cursor=` | Watermarked durable receipt pages |
| POST | `/api/files/uploads/{id}/commit` | Frozen-manifest commit; metadata or stable Operation |
| GET | `/me` | Current account, token permissions, usage |
| PATCH | `/me` | `{"name"}` names the connection this token belongs to (an app names itself after scanning) |
| GET | `/s/<code>` | Agent instructions: a save-link code returns the save prompt; a Connect token (`eam_...`) returns that connection's prompt, live |

All clients stream same-origin content or use P5 parts followed by explicit commit. Long work returns
202 + Location below `/api/files/operations/{id}`; polling/cancellation uses that exact identity. Native
uncertainty never authorizes blind re-upload. File identity is the path, and workflow IDs are temporary.

Save links (for agents):

```text
https://your-host/s/abcd2345efgh2345
```

The URL itself is the token plus the instructions: `GET` only returns a prompt and accepts no content.
Generated save and Connect prompts call `/api/files` content or P5 session/part/commit endpoints and
follow durable Operations. The account page can mint as many save links as needed, show recent use,
and revoke them at any time; a revoked
link stops working immediately. The short code has no `eam_` prefix and also works directly as a Bearer token for
the full file API. The account page's "recent activity" lists who changed which path and when; save links and
event submissions both land in `audit_log`.

Token management:

```bash
eam token list   --email you@example.com
eam token revoke --email you@example.com --id <token ID>
```

The browser has the same thing at `/connect`: clicking **Add app** mints a connection with the defaults and
shows a QR code plus a one-line prompt, while its path, permissions and lifetime stay editable in the same
panel. The prompt links to `/s/<token>`, which returns the full instructions from the live row. The page
mints apps only; tokens can still be created with the CLI or the web API and are listed there for revocation.
Everything is backed by `api_tokens`. An app starts nameless and names itself with `PATCH /me` after
scanning. The payload and the page design are in [docs/design/connect.md](docs/design/connect.md).

### The API the web UI uses

File requests use `/api/files` with plain resources and Problem Details. Cookie mutations require the
session CSRF token and configured Origin. The account/event APIs below retain their envelopes.
Legacy `/api/fs/*` routes return 410.

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
| GET | `/api/timeline?cursor=&limit=` | Read the event stream with authorized same-origin media references |
| DELETE | `/api/events/{id}` | Delete one event (a tombstone, so it cannot come back) |
| GET | `/api/timeline/status` | Organizing progress (unfinished tasks, files organized, earliest/latest month) |
| POST | `/api/timeline/rebuild` | Force a full re-organize of the current account (drops the analysis results and backfills from scratch) |
| GET | `/api/health` | Liveness probe |

The auth write endpoints validate `Origin`/`Host` and rate limit in in-process memory (code sends: 30 per hour per
IP, 5 per hour per email, with a 60-second cooldown per email; code verification: 30 per 15 minutes per IP;
password sign-in: 30 per 15 minutes per IP, 10 per 15 minutes per email).

## Environment variables

| Variable | Description |
|---|---|
| `DATABASE_URL` | Postgres connection string |
| `APP_SECRET` | Stable application secret independent of storage credentials |
| `STORAGE_BACKEND` | `local`, `cos` or `s3`; configure bound roots per P1 |
| `FILE_COOKIE_ORIGINS` | Explicit allowed cookie mutation origins including port |
| `FILE_COORDINATED_WRITES` | Default false; enable only after native evidence, legacy drain and root budget admission |
| `FILE_MAX_SINGLE_PUT_BYTES` | Simple PUT/object publication policy; default 32 MiB, admitted object maximum 5 GiB |
| `FILE_OBJECT_ADMISSION_FILE` / `FILE_OBJECT_SPOOL_ROOT` | Exact-connection capability evidence and durable shared object spool |
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
| `UPLOAD_MULTIPART_THRESHOLD` / `UPLOAD_PART_SIZE` | Retired legacy transfer settings; not P5 geometry or publication limits |
| `SES_REGION` | Tencent Cloud SES region, `ap-guangzhou` by default |
| `SES_FROM_EMAIL_ADDRESS` | A sender address already verified in SES; may include a display name, as in `Everything About Me <noreply@yourdomain.com>` |
| `SES_TEMPLATE_ID` | Verification code template ID (default / English) |
| `SES_TEMPLATE_ID_ZH` | Optional Chinese template ID; falls back to `SES_TEMPLATE_ID` |
| `SES_TEMPLATE_VAR` | Template variable name, `code` by default |
| `SES_SUBJECT` | Subject of the default (English) verification email |
| `SES_SUBJECT_ZH` | Subject of the Chinese verification email |
| `SES_REPLY_TO_ADDRESS` | Optional reply-to address |
| `SES_SECRET_ID` / `SES_SECRET_KEY` | Independent server-side mail credentials |

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
| `make test` | Go tests + frontend typecheck and behavior tests |

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
- **The frontend is a pure SPA**: first paint waits for application JS; heavy preview dependencies load on demand. Storage SDKs and credentials stay server-side.
- **Automatic events are heuristic for now**: they only look at path/size/type/time in the index and never read EXIF/GPS or image content; analysis, grouping, scoring, and naming are all interfaces (`Analyzer` / `Grouper` / `Scorer` / `Narrator` in `internal/timeline`), so wiring in multimodal models or an LLM is a matter of swapping the implementation. The health CSV is a built-in `AmbientExtractor`.
- **The event structure is in [`docs/design/events.md`](docs/design/events.md)**: one event = a time + one card (`photo` / `note` / `metric` / `html`) + an optional detail link; third-party apps submit through `POST /events` with a token that has `can_events`.
- **Old databases need file times backfilled**: `nodes.source_modified_at` is new in this change, and legacy rows only get it after one `reconcile` run; until then the stream falls back to the index update time.
- **"Daily video" detection is heuristic**: videos in the same directory spanning ≥7 days with ≥7 videos get folded into one series card; when the guess is wrong, a manual override could come later, or a model could replace it.
- **Moving or copying a directory does not refresh the stream immediately**: the background sweeper scans recent changes every 20 minutes; if you want it now, use `POST /api/timeline/rebuild`.
- **Events have no in-site editor yet**: right now the only sources are automatic (photo sessions) and third-party submissions; hand-written events, health-change cards, and merging are all still missing.
- **Upgrading to the event model takes one migration run**: `sql/013_events.sql` creates `events` / `event_media` and marks the existing monthly snapshots stale; the next sweeper pass (or `POST /api/timeline/rebuild`) recomputes each month's pieces into events. Before the migration the stream is empty.
