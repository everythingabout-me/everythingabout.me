# Connect

> Status: shipped · 2026-10-29
>
> One line: **Connect is the one page that hands out a credential and shows what can touch the drive.** An App
> gets a QR code and a one-line prompt; an agent, skill or script gets the same one-line prompt, with the
> instructions behind a `/s/` link. Both are the same `api_tokens` row underneath.

---

## 1. The shape

The page mints **apps** only, and hands one over in two ways: a QR code for the app and a one-line prompt for
an agent. The `token` kind still exists in `api_tokens` for the CLI and scripts; the page lists those rows
under Active tokens so they can be revoked, but it does not mint them.

| | App | Token |
|---|---|---|
| Minted by | `Add app` on this page | the CLI / the web API |
| Delivered as | a QR code and a one-line `/s/` prompt | the raw token |
| Lifetime | 1 hour, 1 day, 1 month, 1 year, or no expiry | required, 1 min … 7 days |
| Appears under | Connected apps | Active tokens |
| Default lifetime | no expiry | required |
| Named by | the app itself, after scanning (`PATCH /me`) | the caller |

A **skill** can use an app's prompt; an **agent session** can use the same one. There is no separate token form
on the page any more.

`kind` (`app` / `token`) is a **label**, not a permission: `prefix` + `read` / `write` / `events` remain the
security boundary. A client may create either kind with either lifetime the server allows; the lists are a way
to read the page, not an authorization rule.

**An app is a slot, not a form.** Clicking `Add app` mints it with the defaults — whole drive, all three
permissions, no expiry — and opens the handoff at once. The handoff is two columns: the credential on the
left (QR + agent prompt), the live configuration on the right. The app reports its own name and reads its
policy from `/me`; nothing is typed before the QR exists.

## 2. The QR JSON (v1)

The QR is a **credential transport, not a snapshot of the connection**. It carries only what cannot change:
the API base, the token id and the secret. The page can edit the scope, permissions and lifetime after the
code is drawn, and the server does not keep the secret to redraw it; carrying policy here would freeze the
defaults into the code.

```json
{
  "v": 1,
  "type": "eam.connect",
  "api": "https://everythingabout.me",
  "id": "ab12cd34ef56",
  "token": "eam_ab12cd34ef56_9Qm2vX7pLk3RtYbN8sWdFgHjKcZaE4uPoI1nM6qS0tU"
}
```

| Field | Rule |
|---|---|
| `v` | payload version; unknown → tell the user to update the app |
| `type` | always `eam.connect`; anything else is not our code |
| `api` | public base URL, no trailing slash; all calls go here |
| `id` | `token_id`, non-secret, for support and display |
| `token` | the Bearer secret; the only sensitive field |

The app's first call after scanning is:

```http
GET {api}/me
Authorization: Bearer {token}
```

`/me` returns the name, the prefix, `read` / `write` / `events` and `expiresAt` — the values the right-hand
pane edits. A 401 means revoked or expired; the app should ask the user for a new connection rather than
retrying. If `v` is unsupported, or the JSON is not `type: eam.connect`, show "not an Everything About Me
code"; do not post the token anywhere.

Then, with its own name:

```http
PATCH {api}/me
Authorization: Bearer {token}
Content-Type: application/json

{"name": "Obsidian"}
```

Naming itself is the only write a credential has about itself: a token can change only its own `name` (matched
by `token_id` **and** `user_id`), never its scope, permissions or lifetime. The page's session-only
`PATCH /api/connect/tokens/{id}` is the other half: it edits policy without touching the secret, the name or
the kind.

Because the QR carries no policy, it never goes stale: cut a permission off after the app scanned, and the
next `/me` call already says so.

The QR is rendered by the server (`skip2/go-qrcode`, PNG, error correction M, 512 px, displayed at 220 px) so the
token never goes to a third-party QR service and the browser needs no QR dependency. The response is
`Cache-Control: no-store`.

**Known tradeoff:** the QR contains the actual token. A screenshot of the code is a working credential until it
is revoked or expires. This is the same exposure as the prompt link, and the reasons to accept it now are
simplicity and zero extra moving parts. The `v` field leaves room for a `code` payload plus a short-lived
`/redeem` exchange later; that is the security upgrade, not a v1 requirement.

## 3. The prompt

The app handoff shows one sentence:

```text
Use my drive as described at https://everythingabout.me/s/eam_ab12cd34ef56_...
```

The URL is the same `/s/` route save links use, and it accepts a Connect token (`eam_...`) as the code.
Opening it returns the full instructions below; the page reads the live row, so scope, permissions and expiry
are always current. An agent can fetch it, and a human can open it in a browser (an instruction page with a
copy button). The one-liner follows the page language (zh / en); the artifact behind the link stays in English.

The page returns Markdown by default, an HTML instruction page for a browser, and `?format=json`.

```text
# everythingabout.me

You can access my drive through the Everything About Me API.

API:     https://everythingabout.me
Token:   eam_...
Access:  read + write
Scope:   Inbox/
Expires: 2026-10-29T13:45:00Z

Use it as a Bearer token:
  curl -H "Authorization: Bearer eam_..." \
       "https://everythingabout.me/list?path=Inbox"

  curl -X PUT -H "Authorization: Bearer eam_..." \
       --data-binary @note.md "https://everythingabout.me/api/files/Inbox/note.md"

First call GET /me to confirm the token and its scope.
```

The URL is a credential, not a bookmark: it contains the token, exactly like the QR. The page sends
`Cache-Control: no-store`, the request logger redacts `/s/<code>`, and revoking the connection makes the page
404. This is the save-link tradeoff applied to a scoped connection: one short sentence instead of a block,
with the block one click away.

## 4. The page

```
Connect
                                                         [Add app]

Scan this with the app                                              Done

  ┌──────────────┐                     Configuration
  │              │                     Path
  │      QR      │                     [ folder  Whole drive        ▾ ]
  │              │                     Permissions
  └──────────────┘                     [✓ Read]  [✓ Write]  [✓ Events]
                                       Lifetime
                                       [ 1 hour ] [ 1 day ] [ 1 month ] [ 1 year ] [✓ No expiry]
  AGENT PROMPT                  Copy
  Use my drive as described     Changes apply immediately.
  at https://.../s/eam_...      (the link page shows the latest config)

Connected apps  1
  [window] Obsidian   Read + write · Whole drive
           Never used                                      [Revoke]

Active tokens  1
  [key]    Claude Code  Read + write · Inbox/
           Expires in 41 minutes · Never used               [Revoke]

Finished  1  ▸
```

- **One click mints an app.** `Add app` creates it with the defaults (whole drive, all three permissions, no
  expiry) and opens the handoff at once; there is no app form and no name field, and the row is already in the
  list while the QR is on screen.
- **The handoff is two columns.** Left: the QR and the prompt. Right: path, permissions and lifetime. `Done`
  closes the panel; the connection stays in the list.
- **Two hairline separators.** A vertical rule (a shadcn-`Separator`-style 1px `--border`) sits between the
  handoff and the configuration; an `or` rule sits between the QR and the prompt, because scanning and copying
  are alternatives. The vertical rule is hidden when the columns stack; the `or` rule stays.
- **The configuration is live.** Each change `PATCH`es the token and the list row updates. The QR does not
  change (it carries only the credential) and neither does the one-line prompt; the link page it points at
  always renders the current scope, permissions and expiry. The secret, the name and the kind are not editable
  here.
- **The prompt is for an agent.** It is one sentence with the `/s/<token>` link. Following the link returns
  the token, the scope and the calls, so scanning is optional and the chat message stays short. The URL is a
  credential; see §3.
- **The page no longer mints tokens.** The Active tokens list stays for rows minted by the CLI or the web API,
  so they can still be revoked; there is no token form and no second handoff.
- **Selection is a check, not a tint.** Every option is a checkbox-style pill: a box with a tick inside, a blue
  border and a light blue fill when selected. Permissions allow several; lifetime allows one. No free-text
  scope, no hidden write-implies-read rule.
- **The folder picker is a popover**, not a text field: it starts at your folders, shows a breadcrumb, and
  `Use this folder` commits the level you are in. "Whole drive" (empty prefix) is the first entry and the
  default. It reads the same `/api/fs/list` as the file page.
- **The lists are split by kind.** Apps and temporary tokens answer different questions ("what is connected"
  vs "what is running right now"), so they get separate headings and counts. Both are sorted by most recently
  created or used; a new connection lands on top.
- **A row is three lines:** name; access + scope; life (expiry and last use). An app shows `Unnamed app` until
  it names itself. `Revoke` is a two-click inline action, no dialog. `Finished` is a collapsed group of expired
  and revoked rows, with the kind shown because the two lists share it.
- **Empty states** are one quiet line in each list. The page does not add a tutorial; the button and the
  panel are the teaching.
- **"Connected" is not a lie:** a new App shows `Never used` until the first API call (`last_used_at`). Closing
  the handoff without scanning leaves an unused row on purpose; it is revocable like any other.

### Lifetimes

| Kind | Presets | Server rule |
|---|---|---|
| App | 1 hour, 1 day, 1 month, 1 year, no expiry | expiry ≤ 365 days, or none |
| Token | — (CLI / API only) | expiry required, 1 min … 7 days |

Limits enforced on create: name 1–80 runes (an app may start empty; a token may not); at least one of read /
write / events; prefix normalized by `paths.Normalize`; at most **50 active** connections; at most
**30 creations per hour** per user (in-process limiter, per instance). Expiry is checked by `VerifyToken`, so a
token stops working at the second it expires.

## 5. API

All four endpoints are **web-only**: the httpOnly session cookie, never a Bearer token. Mutations also require
the same-origin check. This matters: a leaked scoped token must not be able to mint or widen one.

```http
GET    /api/connect/tokens          → {tokens, api}
POST   /api/connect/tokens          → {token, secret, api, payload, qr}
PATCH  /api/connect/tokens/{id}     → {token}
DELETE /api/connect/tokens/{id}     → {revoked: true}
```

`GET` returns every row (revoked and expired included) so the page can show Finished; the client splits them.

`PATCH` is the live configuration: a full policy replacement with `{prefix, read, write, events, expiresIn}`.
It never changes the name, the kind or the secret, so the QR and the one-line prompt stay valid. The kind is
read from the row, so the lifetime cap still matches it; a revoked or unknown id is a 404.

The prompt link is not part of the web API: `GET /s/<code>` is public and accepts either a save-link code or
the connection's token secret. It returns the Markdown prompt by default, an HTML page for browsers, and JSON
with `?format=json`; it is documented with save links in [docs/api/api.md](../api/api.md) §4.14–4.15.

The app side has one endpoint of its own: `PATCH /me` with the Bearer token (documented in
[docs/api/api.md](../api/api.md) §4.11) sets the display name of that token. It cannot touch scope,
permissions or lifetime, and it is the only write a credential has about itself.

`POST` body:

```json
{
  "name": "Claude Code",
  "kind": "token",
  "prefix": "Inbox",
  "read": true,
  "write": true,
  "events": false,
  "expiresIn": 3600
}
```

The page only sends the app shape: `{"kind": "app", "prefix": "", "read": true, "write": true, "events":
true, "expiresIn": 0}`. The token body above remains for scripts calling the web API. `name` is required for
`kind: "token"` and may be omitted for an app, which starts nameless. `expiresIn` is seconds; `0` means never and is only valid for `app`. The server computes `expiresAt`
from its own clock. The response is the only time `secret` exists outside the hash; there is no reveal
endpoint. The `payload` is the exact credential-only JSON encoded into `qr`; both are returned once for the
handoff.

Errors use the normal `{ok:false,error}` envelope: 400 for an empty token name, no permission bit, an unknown
kind, a bad prefix or a lifetime outside the kind's range; 404 for an unknown or revoked id on PATCH; 409 at
50 active; 429 over the creation rate limit.

## 6. Schema

`sql/015_connect.sql`:

```sql
alter table api_tokens add column if not exists kind text not null default 'app';

create index if not exists api_tokens_user_active
  on api_tokens (user_id, created_at desc)
  where revoked_at is null;
```

Existing rows were all minted for apps, so they default to `app`. The file is idempotent because `make migrate`
replays every migration. No check constraint: the Go layer rejects unknown kinds, and a bad label is not a
security issue.

`auth.Token` now carries `Kind` and `RevokedAt`; `NewTokenInput` carries `Kind`. `CreateToken` validates the
kind, the rune-counted name and the lifetime; an app may pass an empty name, a token may not.
`SetTokenName` updates only `name` for one `(user_id, token_id)` pair, which is what `PATCH /me` calls.
`UpdateToken` rewrites prefix / permissions / expiry for one pair, which is what the page's `PATCH
/api/connect/tokens/{id}` calls. The CLI keeps working unchanged: `eam token create` omits `kind` and gets
`app`, and those rows show under Connected apps.

## 7. Security

| Threat | Control |
|---|---|
| A scoped Bearer token mints a full-drive token | management is session-only (`requireUser` → `SessionUser`) |
| A scoped Bearer token widens itself | `PATCH /api/connect/tokens/{id}` is session + same-origin only; a Bearer token can only `PATCH /me`, its own name |
| CSRF from another site | `AssertSameOrigin` on POST/PATCH/DELETE; SameSite=Lax cookie |
| Secret leaks from the page | one-time display, unmounted after Done, `Cache-Control: no-store`; the only GET that carries it is the prompt link itself |
| Secret in the QR | scoped, revocable, and expiring unless the user chose no expiry; one-time-display warning; return path documented in §2 |
| Secret in the prompt URL | the URL is a credential; `no-store`, redacted in the request log, 404 once the connection is revoked, and only ever shown in the one-time handoff |
| A QR outlives the policy it was minted with | the QR carries no policy; the app reads `/me`, which always returns the current row |
| A link page shows an old policy | the page reads the token row on every request, so it always renders the current scope, permissions and expiry |
| A token renames another connection or grants itself more | `PATCH /me` updates only its own `name`, matched by `token_id` and `user_id`; scope, permissions and lifetime are not writable |
| A leaked token lives forever | every Token expires; every App can be revoked; expiry checked at verification |
| Brute force / abuse | 30 creates/hour/user + 50 active cap + login required |
| XSS through a name | React renders names as text; the server marshals the payload with `encoding/json` |

One deliberate limit: the rate limiter is in-process, like every other limiter here. On multiple instances each
counts separately.

## 8. Not in this version

- **One-time QR code + `/redeem`.** The additive security upgrade; needs a short-lived claim row and one
  unauthenticated endpoint. The QR `v` field is the compatibility hook.
- **The page scanning the app's QR** (camera). The App scans the page; the page never asks for camera access.
- **OAuth / per-app consent, app logos, a directory of integrations.** A personal drive has no developer
  ecosystem to justify it.
- **Pairing without copy-paste (`eam connect`).** The previous draft's device flow. The one-line prompt is
  the v1 answer.
- **Renaming from the page**, filtering, pruning finished rows, usage charts. An app names itself; the page
  never renames a connection. The lists stay readable at personal scale.
- **Moving save links here.** They remain on the account page; the `/s/` link page overlaps, so a future cleanup
  can retire one of them.
- **App-side SDK / spec page.** The JSON schema above plus `GET /me` is the whole integration; it is documented
  in [docs/api/api.md](../api/api.md) §4.15.

## 9. Verification

- `make test` (Go + `tsc --noEmit`); `make build`.
- Unit tests: kind normalization, name rules (empty app allowed, empty token not), lifetime caps, the QR
  payload (only `v`, `type`, `api`, `id`, `token`; no policy fields, no account identity), and the token
  prompt behind `/s/<token>` (API, token, access, scope, expiry, `GET /me`).
- Manual: `Add app` → the QR, the one-line prompt and the new row appear at once; follow the link → the full
  prompt with the live scope/permissions/expiry, Markdown for curl and an HTML page for a browser; change the
  path, a permission and the lifetime on the right → the link page reflects it and the row updates, while the
  QR and the sentence stay the same; `PATCH /me` with the QR's token renames the row; revoke → the link 404s;
  a token row minted elsewhere shows under Active tokens and can be revoked; the page looks right at 390 px
  and in dark mode.
