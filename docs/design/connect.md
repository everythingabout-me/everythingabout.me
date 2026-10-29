# Connect

> Status: shipped · 2026-10-29
>
> One line: **Connect is the one page that hands out a credential and shows what can touch the drive.** An App
> gets a QR code; an agent, skill or script gets a block of text. Both are the same `api_tokens` row underneath.

---

## 1. The shape

Two delivery formats, two lists. Everything else is deliberately absent.

| | App | Token |
|---|---|---|
| Who | a service or a device that can scan | an agent, a skill, a one-off script |
| Delivered as | a QR code containing JSON | a copyable text block with the token |
| Lifetime | 1 hour, 1 day, 1 month, 1 year, or no expiry | 15 min, 1 h, 8 h, 24 h, 7 days |
| Appears under | Connected apps | Active tokens |
| Default lifetime | no expiry | 1 hour |
| Named by | the app itself, after scanning (`PATCH /me`) | the user, when minting |

A **skill** is a Token. An **agent session** is a Token. There is no third kind and no separate pairing flow in
this version.

`kind` (`app` / `token`) is a **label**, not a permission: `prefix` + `read` / `write` / `events` remain the
security boundary. A client may create either kind with either lifetime the server allows; the lists are a way
to read the page, not an authorization rule.

**An app is a slot, not a form.** The page mints it nameless and immediately hands over the QR; the app reports
its own name and knows its own basic information. Nothing is typed before the QR exists, so pairing starts in
one click.

## 2. The QR JSON (v1)

The app scans the code, parses the JSON, and starts using `api` + `token`. The other fields are display metadata
and must be re-checked against `GET /me`.

```json
{
  "v": 1,
  "type": "eam.connect",
  "api": "https://everythingabout.me",
  "id": "ab12cd34ef56",
  "token": "eam_ab12cd34ef56_9Qm2vX7pLk3RtYbN8sWdFgHjKcZaE4uPoI1nM6qS0tU",
  "name": "",
  "scope": "Apps/obsidian",
  "read": true,
  "write": true,
  "events": false,
  "expiresAt": null
}
```

| Field | Rule |
|---|---|
| `v` | payload version; unknown → tell the user to update the app |
| `type` | always `eam.connect`; anything else is not our code |
| `api` | public base URL, no trailing slash; all calls go here |
| `id` | `token_id`, non-secret, for support and display |
| `token` | the Bearer secret; the only sensitive field |
| `name` | empty when the page minted the app (`""`); the app sets it, see below |
| `scope` / `read` / `write` / `events` | display metadata; verify with `/me` |
| `expiresAt` | ISO-8601 UTC, or `null` for no expiry |

The app's first calls after scanning should be:

```http
GET {api}/me
Authorization: Bearer {token}
```

and then, with its own name:

```http
PATCH {api}/me
Authorization: Bearer {token}
Content-Type: application/json

{"name": "Obsidian"}
```

`/me` confirms the token, its scope and its permissions. A 401 means revoked or expired; the app should ask the
user for a new connection rather than retrying. If `v` is unsupported, or the JSON is not `type: eam.connect`,
show "not an Everything About Me code"; do not post the token anywhere.

Naming itself is the only write a credential has about itself: a token can change only its own `name` (matched
by `token_id` **and** `user_id`), never its scope, permissions or lifetime. That is what keeps the page's list
useful without asking the user to type anything before the QR exists.

The QR is rendered by the server (`skip2/go-qrcode`, PNG, error correction M, 512 px, displayed at 220 px) so the
token never goes to a third-party QR service and the browser needs no QR dependency. The response is
`Cache-Control: no-store`.

**Known tradeoff:** the QR contains the actual token. A screenshot of the code is a working credential until it
is revoked or expires. This is the same exposure as the copy block, and the reasons to accept it now are
simplicity and zero extra moving parts. The `v` field leaves room for a `code` payload plus a short-lived
`/redeem` exchange later; that is the security upgrade, not a v1 requirement.

## 3. The copy block

A Token is handed over as one block, copied with one button:

```text
You can access my drive through the Everything About Me API.

API:     https://everythingabout.me
Token:   eam_...
Scope:   read + write inside Inbox/
Expires: 2026-10-29T13:45:00Z

Use it as a Bearer token:
  curl -H "Authorization: Bearer eam_..." \
       "https://everythingabout.me/list?path=Inbox"

  curl -X PUT -H "Authorization: Bearer eam_..." \
       --data-binary @note.md "https://everythingabout.me/api/files/Inbox/note.md"

First call GET /me to confirm the token and its scope.
```

This is an API artifact, not localized UI copy: it stays in English like the save-link prompt. The page around
it is bilingual.

## 4. The page

```
Connect
Give an app long-term access with a QR code, or hand an agent a temporary token.
                                              [Add app]  [New token]

Add an app
Choose what it can reach, then scan the QR. The app names itself.        Cancel

  Path
  [ folder  Whole drive                                        ▾ ]
  Permissions
  [✓ Read]  [✓ Write]  [✓ Events]
  Lifetime
  [ 1 hour ]  [ 1 day ]  [ 1 month ]  [ 1 year ]  [✓ No expiry]

  Read + write + events · Whole drive · No expiry          [Create app]

Connected apps  1
  [window] Obsidian   Read + write · Whole drive
           Never used                                      [Revoke]

Active tokens  1
  [key]    Claude Code  Read + write · Inbox/
           Expires in 41 minutes · Never used               [Revoke]

Finished  1  ▸
```

- **An app form is three choices, no typing.** Path, permissions and lifetime are prefilled (whole drive,
  all three permissions, no expiry), so `Create app` works immediately; it mints the token and replaces the
  panel with the QR. There is no name field: the app reports its name through `PATCH /me`.
- **A token form still has a name.** A token is copied by hand for one agent, so naming it at minting is the
  point; it uses the same folder picker and the same option control, with the short lifetimes.
- **Selection is a check, not a tint.** Every option is a checkbox-style pill: a box with a tick inside, a blue
  border and a light blue fill when selected. Permissions allow several; lifetime allows one. No free-text
  scope, no hidden write-implies-read rule.
- **The folder picker is a popover**, not a text field: it starts at your folders, shows a breadcrumb, and
  `Use this folder` commits the level you are in. "Whole drive" (empty prefix) is the first entry and the
  default. It reads the same `/api/fs/list` as the file page.
- **Handoff replaces the form.** An App shows the QR with "Scan this with the app", the JSON behind
  "Can't scan? Copy the JSON", and the scope line. A Token shows the copy block. Both say the secret is shown
  once. `Done` returns to the lists.
- **The lists are split by kind.** Apps and temporary tokens answer different questions ("what is connected"
  vs "what is running right now"), so they get separate headings and counts. Both are sorted by most recently
  created or used; a new connection lands on top.
- **A row is three lines:** name; access + scope; life (expiry and last use). An app shows `Unnamed app` until
  it names itself. `Revoke` is a two-click inline action, no dialog. `Finished` is a collapsed group of expired
  and revoked rows, with the kind shown because the two lists share it.
- **Empty states** are one quiet line in each list. The page does not add a tutorial; the subtitle and the
  buttons are the teaching.
- **"Connected" is not a lie:** a new App shows `Never used` until the first API call (`last_used_at`). The page
  does not claim more than it knows.

### Lifetimes

| Kind | Presets | Server rule |
|---|---|---|
| App | 1 hour, 1 day, 1 month, 1 year, no expiry | expiry ≤ 365 days, or none |
| Token | 15 min, 1 h, 8 h, 24 h, 7 days | expiry required, 1 min … 7 days |

Limits enforced on create: name 1–80 runes (an app may start empty; a token may not); at least one of read /
write / events; prefix normalized by `paths.Normalize`; at most **50 active** connections; at most
**30 creations per hour** per user (in-process limiter, per instance). Expiry is checked by `VerifyToken`, so a
token stops working at the second it expires.

## 5. API

All three endpoints are **web-only**: the httpOnly session cookie, never a Bearer token. Mutations also require
the same-origin check. This matters: a leaked scoped token must not be able to mint a wider one.

```http
GET    /api/connect/tokens          → {tokens, api}
POST   /api/connect/tokens          → {token, secret, api, payload, qr}
DELETE /api/connect/tokens/{id}     → {revoked: true}
```

`GET` returns every row (revoked and expired included) so the page can show Finished; the client splits them.

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

`name` is required for `kind: "token"` and may be omitted for `kind: "app"`, which starts nameless.
`expiresIn` is seconds; `0` means never and is only valid for `app`. The server computes `expiresAt` from its own
clock. The response is the only time `secret` exists outside the hash; there is no reveal endpoint. The `payload`
is the exact JSON encoded into `qr`; both are returned once for the handoff.

Errors use the normal `{ok:false,error}` envelope: 400 for an empty token name, no permission bit, an unknown
kind, a bad prefix or a lifetime outside the kind's range; 409 at 50 active; 429 over the creation rate limit.

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
`SetTokenName` updates only `name` for one `(user_id, token_id)` pair, which is what `PATCH /me` calls. The CLI
keeps working unchanged: `eam token create` omits `kind` and gets `app`, and those rows show under Connected apps.

## 7. Security

| Threat | Control |
|---|---|
| A scoped Bearer token mints a full-drive token | management is session-only (`requireUser` → `SessionUser`) |
| CSRF from another site | `AssertSameOrigin` on POST/DELETE; SameSite=Lax cookie |
| Secret leaks from the page | one-time display, unmounted after Done, `Cache-Control: no-store`, never in a GET or audit |
| Secret in the QR | scoped, revocable, and expiring unless the user chose no expiry; one-time-display warning; return path documented in §2 |
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
- **Pairing without copy-paste (`eam connect`).** The previous draft's device flow. The copy block is the v1
  answer.
- **Renaming from the page**, filtering, pruning finished rows, usage charts. An app names itself; the page
  never renames a connection. The lists stay readable at personal scale.
- **Moving save links here.** They remain on the account page; the copy block overlaps, so a future cleanup can
  retire one of them.
- **App-side SDK / spec page.** The JSON schema above plus `GET /me` is the whole integration; it is documented
  in [docs/api/api.md](../api/api.md) §4.15.

## 9. Verification

- `make test` (Go + `tsc --noEmit`); `make build`.
- Unit tests: kind normalization, name rules (empty app allowed, empty token not), lifetime caps, the JSON
  payload (`v`, `type`, `api`, `token`, `scope`, null expiry, no account identity).
- Manual: `Add app` → `Create app` with the prefilled choices → the QR renders immediately and the JSON is
  copyable; `PATCH /me` with the QR's token renames the row and the page shows the new name after a reload;
  create a Token → the block is copyable; the new row is on top; revoke is two clicks and moves the row to
  Finished; an expired/revoked token returns 401 from the public API; the page looks right at 390 px and in
  dark mode.
