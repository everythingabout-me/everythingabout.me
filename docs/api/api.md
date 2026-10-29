# Everything About Me API

> An API of our own. **The semantics are the same as S3 / Drive / Dropbox** — it is still "CRUD over a path
> tree", only the shape is ours to define. No version number in the URL, and no `/v1`.

---

## 0. In one sentence

One Bearer token, CRUD over a path tree, plus search and upload. **File bytes never pass through the server**:
both reads and writes trade an address directly with COS.

```
POST /upload          → get an upload address (a small file has exactly one part)
PUT  <that address>   → bytes go straight to COS
GET  /api/files/<path>    → 302 to a download address

everything else goes through us: /list /search /mkdir /move /copy /delete /me /capabilities
```

No concept here needs the docs to be guessed right. `mkdir` creates a directory, `copy` copies, and
`Prefer: respond-async` means "you may do this in the background".

---

## 1. Why not just use S3

Semantically we do the same thing S3 does. But S3 lacks three things we need:

1. **No "user".** It does not know who this app belongs to or which directory it may touch — that is our user
   system.
2. **No search**, and no "who wrote what".
3. Fine-grained control over third-party apps means building an S3 facade (SigV4, XML, multipart, 307):
   ~2500 lines on the server, and the client picks up five more settings — region/bucket/endpoint.

So: **copy the semantics, define the interface ourselves.** A client needs just two values, `endpoint` and
`token`.

---

## 2. Semantic comparison with familiar APIs

The same job under different names; the work is identical:

| Job | S3 | Google Drive | Dropbox | Ours |
|---|---|---|---|---|
| List a directory | `ListObjectsV2` | `files.list` | `files/list_folder` | `GET /list?path=` |
| Read a file | `GetObject` | `files.get` | `files/download` | `GET /api/files/<path>` |
| Write a file | `PutObject` | `files.create` | `files/upload` | `PUT /api/files/<path>` |
| Large file | `MultipartUpload` | resumable upload | `upload_session` | `POST /upload` |
| Metadata | `HeadObject` | `files.get` | `files/get_metadata` | `HEAD /api/files/<path>` |
| Create a directory | — (no directories) | `files.create` | `files/create_folder` | `POST /mkdir` |
| Delete | `DeleteObject` | `files.delete` | `files/delete_v2` | `DELETE /api/files/<path>` |
| Move/rename | `CopyObject`+`Delete` | `files.update` | `files/move_v2` | `POST /move` |
| Copy | `CopyObject` | `files.copy` | `files/copy_v2` | `POST /copy` |
| Search | — | `files.list(q=)` | `files/search_v2` | `GET /search?q=` |

Request parameters and return values stay close to theirs too: paths, `cursor` pagination, `etag`, `size`,
`mime`, `modifiedAt`. **None of them is a new concept.**

---

## 3. Conventions

| Item | Convention |
|---|---|
| Base URL | `https://your-host` |
| Auth | `Authorization: Bearer eam_live_xxx` (the web cookie session works too) |
| Path | a logical path with no leading `/`, percent-encoded per segment (`/` is not encoded) |
| Time | ISO-8601 UTC, `2026-10-24T02:11:07Z` |
| Pagination | `cursor` is an opaque string; `limit` defaults to 1000, max 5000 |
| Errors | `{"error":"not_found","message":"..."}` plus a correct HTTP status code; `message` is a short English sentence you can show or log directly |
| Characters | UTF-8; the server normalizes to NFC |
| Size | a single `PUT` is ≤ 32 MiB; anything larger goes through `/upload` |
| Async | any request that changes state may carry `Prefer: respond-async[, wait=N]`, see §4.12 |
| Capabilities | `GET /capabilities` needs no auth; clients read their limits from it, **do not hard-code** |
| Parent path | when the parent is a file, writes and mkdir return `409` (nothing grows a subtree under a file) |

The one public route under `/api` is file content: `/api/files/<path>`. `/files` belongs to the web app
(the file browser and its file detail pages), so the by-path file API moved one level deeper. Every other
endpoint below stays at the root.

**There is no `id`.** The path is the identity (the same as an S3 key). The server does have a row primary key
internally, but it never enters JSON — see §8.4 for why.

---

## 4. Endpoints

```
GET    /capabilities                      limits and capabilities (no auth)
GET    /me                                who am I
GET    /list?path=                        list a directory
GET    /api/files/<path>                  read a file
HEAD   /api/files/<path>                  look at metadata
PUT    /api/files/<path>                  write a small file
DELETE /api/files/<path>                  permanent delete
POST   /mkdir     {path}                  create a directory
POST   /move      {from,to,overwrite}     move / rename
POST   /copy      {from,to,overwrite}     copy
POST   /upload → GET /upload/{id} → POST /upload/complete
GET    /search?q=                         search
GET    /move/{id}  POST /move/{id}/cancel  POST /move/{id}/retry
GET    /copy/{id}  POST /copy/{id}/cancel  POST /copy/{id}/retry
```

### 4.1 List a directory

```http
GET /list?path=Photos/2024&limit=200&cursor=
```

```json
{
  "path": "Photos/2024",
  "writable": true,
  "items": [
    {
      "name": "a.jpg",
      "path": "Photos/2024/a.jpg",
      "parent": "Photos/2024",
      "type": "file",
      "size": 248123,
      "mime": "image/jpeg",
      "etag": "9f2b…",
      "sha256": "…",
      "createdAt": "2026-10-24T02:11:07Z",
      "modifiedAt": "2026-10-24T02:11:07Z"
    },
    {"name": "raw", "path": "Photos/2024/raw", "parent": "Photos/2024", "type": "dir", "size": 0}
  ],
  "cursor": null
}
```

- Directories come first, then it sorts by `lower(name)`.
- A `cursor` of `null` means there is no next page.
- Empty directories still come back as `type: "dir"` (creating a directory writes a placeholder object plus an
  index record).
- `writable` says whether **this directory** is writable (a read-only token, or a directory outside the token's
  prefix, both make it `false`).
  Agents use it to decide whether to bother writing — never discover it by collecting 403s.
- `sha256` is a content fingerprint, not an identity: unchanged content keeps the same value. It is a more
  stable cache key than the path.

### 4.2 Read a file

```http
GET /api/files/Photos/2024/a.jpg
Authorization: Bearer eam_live_xxx

→ 302 Location: https://<data domain>/drive/<uid>/Photos/2024/a.jpg?q-signature=…
```

> **The current implementation** points at the COS domain (`<bucket>.cos.<region>.myqcloud.com`), which differs
> from the API domain, so the "cross-origin strips Authorization" requirement below is already satisfied. Once a
> CDN is in front, swap the data domain for `data.example.com`; the signature has to be re-signed for that host.

Clients do not handle this 302 — every HTTP client follows it automatically:

```python
r = requests.get(f"{API}/api/files/Photos/2024/a.jpg", headers=auth)
open("a.jpg", "wb").write(r.content)      # bytes come from the CDN → COS
```

The real path is:

```
client ──GET /api/files/a.jpg──▶ API (auth, accounting) ──302──▶ client ──▶ CDN ──▶ COS
```

- **Location must point at a separate data domain** (such as `data.example.com`). A same-origin redirect does
  not strip `Authorization`, so the token ends up in the CDN/COS logs; cross-origin, mainstream clients
  (requests, fetch, Go, curl) all strip it automatically. This one is a hard requirement.
- `Range` is supported (resumable downloads, video scrubbing).
- For clients that cannot follow redirects: `GET /api/files/<path>?json=1` → `{"url":…,"name":…,"size":…,"mime":…}`,
  then issue the GET yourself and **do not send `Authorization`**. This is the only query parameter in the whole
  API that changes the response type.
- If the path is a directory you get `400 is_directory`; use `/list`.

### 4.3 Metadata

```http
HEAD /api/files/Photos/2024/a.jpg
→ 200
   Content-Length: 248123
   Content-Type: image/jpeg
   ETag: "9f2b…"
   Last-Modified: Fri, 24 Oct 2026 02:11:07 GMT
```

**HEAD only.** The earlier `GET /api/files/<path>?meta=1` is gone (two entrances to one thing is the easiest fat to
grow on an API); passing `meta=1` now returns `400` with a message pointing at HEAD.

### 4.4 Write a small file (one-step shortcut)

```http
PUT /api/files/Apps/badminton/analyses/a.json
Authorization: Bearer eam_live_xxx
Content-Type: application/json

{"score": 21}
```

```
201 Created
Content-Type: application/json

{"name":"a.json","path":"Apps/badminton/analyses/a.json","type":"file","size":12,
 "mime":"application/json","etag":"…","createdAt":"…","modifiedAt":"…"}
```

- **No overwrite by default**: an existing path returns `409`. To overwrite, add `?overwrite=1` explicitly, or
  send a matching `If-Match` (safer, it prevents concurrent overwrites).
- Overwriting replaces directly and **keeps no version history**.
- `Content-Type` lands in `mime`, defaulting to `application/octet-stream`.
- `If-None-Match: *` is still supported (create only if absent); failure returns `412`.
- Above 32 MiB returns `413` and points you at `/upload`.
- **A client that does not want to reason about size can always use `/upload` — it holds for any size.**

### 4.5 Upload (any size, always three steps)

Bytes go straight to COS the whole way, never through the server. A small file is simply "a single part", so the
client code needs no branches.

```http
POST /upload
{"path":"Videos/a.mp4","size":18234122311,"mime":"video/mp4"}
```

```json
{
  "uploadId": "u_7f3a…",
  "mode": "multipart",
  "size": 18234122311,
  "partSize": 67108864,
  "parts": [
    {"partNumber": 1, "url": "https://data.example.com/…?signA", "headers": {"Content-Type": "video/mp4"}},
    {"partNumber": 2, "url": "https://data.example.com/…?signB", "headers": {"Content-Type": "video/mp4"}}
  ],
  "expiresAt": "2026-10-24T03:00:00Z"
}
```

```python
# client: one code path whether it is one part or many
for p in resp["parts"]:
    requests.put(p["url"], data=chunk(p["partNumber"]), headers=p["headers"])

requests.post(f"{API}/upload/complete", headers=auth, json={
    "uploadId": resp["uploadId"],
    "parts": [{"partNumber": p["partNumber"], "etag": p["etag"]} for p in done],
})
```

```http
POST /upload/complete
{"uploadId":"u_7f3a…","parts":[{"partNumber":1,"etag":"\"9f2b…\""}]}
→ 201 <file object>
```

- The part count is decided by `size`: ≤ 64 MiB is a single part, anything larger gets sliced automatically.
  **The shape of the API never changes**, so adding concurrency or resumability later needs no client change.
- Parts PUT to `data.example.com` (the data domain) without `Authorization`; the signature in the URL is the
  credential.
- `complete` runs a `HeadObject` to verify the size, then writes the index. Skipping it still works, the index
  just lags.
- For single-part uploads you may omit `parts`; the server HEADs the object itself.
- Re-issuing `POST /upload` while an upload is in flight invalidates the old one.

#### After a disconnect, ask first — do not start over

```http
GET /upload/u_7f3a…
→ 200
{
  "uploadId": "u_7f3a…",
  "path": "Videos/a.mp4",
  "mode": "multipart",
  "size": 18234122311,
  "partSize": 67108864,
  "expiresAt": "2026-10-24T03:00:00Z",
  "status": "pending",
  "received": [
    {"partNumber": 1, "size": 67108864, "etag": "\"9f2b…\""},
    {"partNumber": 2, "size": 12345678, "etag": "\"1a2c…\""}
  ]
}
```

`received` is the parts COS has already taken (the server turns it into `ListParts`); the client only uploads
what is missing.
`status` is `pending` / `completed` / `cancelled` / `expired`.
Single-part mode returns an empty `received` — just retransmit the whole part, PUT is idempotent on its own.

> **The current implementation** hands out a single part when `size` ≤ 5 GiB (or `UPLOAD_MULTIPART_THRESHOLD`);
> above that it slices into multiple parts automatically (64 MiB per part by default, at most 1000 parts,
> growing the part size when that would be exceeded).

### 4.6 Delete

```http
DELETE /api/files/Documents/old.pdf
→ 204
```

- **Permanent delete**: the object and the index go together, with no trash.
- `DELETE /api/files/<dir>` requires `?recursive=1`, otherwise `409` — the parameter is there as a guardrail against
  slips.
- The client should confirm once before deleting (that dialog on the web).

### 4.7 Move / rename

```http
POST /move
{"from":"Notes/a.md","to":"Notes/b.md","overwrite":false}
→ 200 {"name":"b.md","path":"Notes/b.md", …}
```

- **Files**: done synchronously, returning the moved object directly.
- **Directories**: N objects means N server-side copies. The server tries synchronously for at most 30 seconds:
  finished returns `200`, unfinished returns `202` + `Location`.

```http
POST /move
Prefer: respond-async, wait=2
{"from":"Videos/src","to":"Videos/dst"}

→ 202
   Location: /move/m_5f90…
   Retry-After: 2
   {"id":"m_5f90…","op":"move","from":"Videos/src","to":"Videos/dst",
    "status":"running","total":412,"done":180}
```

```http
GET /move/m_5f90…
→ 200 {"id":"m_5f90…","status":"running","total":412,"done":180,"cancelRequested":false}
→ 200 {"id":"m_5f90…","status":"done","total":412,"done":412}
```

- Status: `pending → running → done`; a failure is `failed` and carries `error`; while `cancelRequested` is true
  it stops after the current item finishes, then becomes `cancelled`.
- `POST /move/{id}/cancel` asks it to stop; `POST /move/{id}/retry` rescans the source and continues where it
  left off.
- A move is "copy first, then delete the source, then rewrite the index", so **the source is still there** until
  the move finishes and can be read repeatedly.
- Directories have no "rename in one shot" primitive, so by nature this can be a long operation; but there is no
  "task" concept on the outside — only "this move has not finished yet".

### 4.8 Copy

```http
POST /copy
{"from":"Photos/2024/a.jpg","to":"Backup/a.jpg","overwrite":false}
→ 201 <new file object>
```

- Both files and directories are supported. Directories copy recursively and follow the same `Prefer` rules as
  §4.12; progress is at `GET /copy/{id}`, cancel/retry at `POST /copy/{id}/cancel|retry`.
- An existing target with `overwrite=false` → `409`; directory copies **do not merge**, an existing target is
  `409`.
- The copy is a **new object**: a new creation time, and its first index write is a new row. The source is
  untouched.
- A target inside the source (`copy a → a/b`) returns `400`.

### 4.9 Create a directory

```http
POST /mkdir
{"path":"Photos/2024/东京"}
```

```json
201 {"name":"东京","path":"Photos/2024/东京","parent":"Photos/2024","type":"dir","size":0,
     "createdAt":"2026-10-24T02:11:07Z","modifiedAt":"2026-10-24T02:11:07Z"}
```

| Case | Response |
|---|---|
| Created | `201` + the directory object |
| Already exists as a directory | `200` + the directory object (**idempotent**, an agent retrying does not blow up) |
| Already exists as a file | `409` |
| Parent missing | created automatically (`mkdir -p`), no extra parameter |
| Parent is a file | `409` |

Why `POST /mkdir` instead of `PUT /api/files/<dir>/`: a directory is "a resource without bytes", and sharing
`/api/files/<path>` with files would force a **trailing slash** to tell them apart — the biggest trap in WebDAV.
With a verb plus a body, a path is always a path and there is no ambiguity.

### 4.10 Search

```http
GET /search?q=羽毛球&path=Videos&mime=video/&type=file&limit=50
```

The result shape is the same as `/list` (including `writable`), and the cursor rules match. It matches filenames
and full paths. `type` is `file` or `dir`.

### 4.11 Who am I / how big is this service

```http
GET /me
```

```json
{
  "user": {"email": "you@example.com"},
  "token": {"name":"badminton","prefix": "Apps/badminton", "read": true, "write": true},
  "usage": {"bytes": 1823412231, "files": 20481}
}
```

For agents to self-check: confirm your own prefix and permissions.

```http
GET /capabilities        # no auth, you do not even need to bring a token
```

```json
{
  "maxPutBytes": 33554432,
  "maxUploadBytes": 5497558138880,
  "multipartThreshold": 5368709120,
  "partSize": 67108864,
  "maxParts": 1000,
  "list": {"defaultLimit": 1000, "maxLimit": 5000},
  "rateLimit": {"perMinute": 600},
  "quotaBytes": 0,
  "async": {"header": "Prefer", "value": "respond-async", "defaultWaitSeconds": 30, "maxWaitSeconds": 60},
  "features": ["range", "if-match", "overwrite", "recursive-delete", "copy", "search"]
}
```

Let clients read this once at startup and never hard-code 32 MiB / 64 MiB in their sources.

### 4.12 Long operations: `Prefer: respond-async`

**Every request that changes state accepts it**, including `PUT` and `DELETE`, not only move/copy:

```http
Prefer: respond-async            # do not wait, give me a 202 right now
Prefer: respond-async, wait=10   # wait at most 10 seconds, 202 if unfinished
(omitted)                        # the server waits at most 30 seconds, 202 if unfinished
```

- A `202` carries `Location: /<action>/{id}` and `Retry-After`; just poll that address.
- A `200`/`201` means **it is already done**; the client handles both cases — one branch:

```python
resp = post("/move", json={"from": a, "to": b}, headers={"Prefer": "respond-async, wait=10"})
if resp.status_code == 202:
    while (job := get(resp.headers["Location"])).json()["status"] in ("pending", "running"):
        time.sleep(2)
```

- The server **may ignore** this header (RFC 7240 allows it). Today only directory `move` / `copy` actually
  return 202; `mkdir`, single-file operations, and `DELETE` finish in milliseconds and always return
  200/201/204. If some operation gets slow one day, it starts returning 202 on its own, and **that line of
  client code does not change**.
- The 202 body and the `GET /<action>/{id}` body have the **same shape**, so one parser.
- The progress address is always "the action's own path + id": no `/tasks`, no queue name, no `type` field.

### 4.13 Events (the stream)

The second kind of thing besides the file API. **One event = a span of time + one card + an optional detail
link**; the design is in [`docs/design/events.md`](../design/events.md).

Events are not files: files use the path as identity, events have an `id`; the stable handle on the outside is
your own `external_id`.

The token needs the `can_events` capability bit:

```bash
eam token create --email you@example.com --name Strava --prefix Apps/strava --events
```

#### Create / update (idempotent)

```http
POST /events

{
  "external_id": "run-8921",
  "starts_at": "2025-08-03T07:12:00Z",
  "ends_at":   "2025-08-03T08:02:00Z",
  "tz": "Asia/Shanghai",
  "precision": "minute",
  "title": "晨跑 · 北岸",
  "detail_url": "https://www.strava.com/activities/8921",
  "type": "metric",
  "payload": {
    "metrics": [
      {"label": "距离", "value": "12.4", "unit": "km"},
      {"label": "配速", "value": "5'02\"", "unit": "/km"}
    ],
    "body": "出门时 24 度。"
  }
}
```

```json
201 { "id": "0f1c…", "state": "published", "created": true }
```

- Posting the same `(token, external_id)` again is a **whole-object replacement**, returning `200` +
  `created:false`;
- Pushing an event the user deleted → `410 gone`; stop retrying;
- Media are paths (`payload.media: ["Apps/strava/2025/0803/a.jpg"]`), must sit inside the token's prefix, and are
  uploaded through the file API first; `photo` allows at most 4, and there are no external image URLs;
- `tz` defaults to the server's `TIMELINE_TZ` (`Asia/Shanghai`).

The four `type` values and their payloads:

| type | payload | Limits |
|---|---|---|
| `photo` | `media[1–4]` + optional `body` / `meta` | title 80 characters, body 500 characters |
| `note` | `body` + optional `meta` | same as above |
| `metric` | `metrics[1–3]` + optional `body` | label / value required |
| `html` | `html` + `height` | html ≤ 32 KB, height 40–640 |

We render `html` cards inside a `sandbox` iframe (scripts disabled, CSP restricted) and inject the design tokens
`--eam-bg` / `--eam-surface` / `--eam-text` / `--eam-text2` / `--eam-text3` / `--eam-line` / `--eam-accent`.

#### Delete / read

```http
DELETE /events/{id}          → {"state":"detached"}   content kept, external link cleared
GET    /events?from=&to=     → reconcile: the events you wrote yourself (including external_id)
GET    /events/{id}          → one event as full JSON
```

There is no `PATCH`: re-posting is updating.

### 4.14 Save links (for agents)

The link only hands out a token and the instructions; saving goes through the existing file API, and `/s/<code>`
itself accepts no content.

```http
GET /s/abcd2345efgh2345
```

- `Accept: text/markdown` (or `curl`'s default) returns the prompt meant for the agent.
- Opening it in a browser returns an instruction page.
- `?format=json` returns `{token, url, api, files, upload, prompt, ...}`.
- `POST` to `/s/<code>` returns `405`.

```markdown
# everythingabout.me

Connected to this drive.

Token: abcd2345efgh2345
API: https://everythingabout.me
Authorization: Bearer abcd2345efgh2345

Save a short text (one request):
  PUT https://everythingabout.me/api/files/Inbox/2026/10/28/note.md
  Content-Type: text/markdown

  <content>

Save a file (image, video, PDF, any size):
  POST https://everythingabout.me/upload
  {"path": "Inbox/2026/10/28/photo.jpg", "size": 12345, "mime": "image/jpeg"}

  PUT the bytes to each returned part URL, then:
  POST https://everythingabout.me/upload/complete

Use a unique path; PUT does not overwrite by default.
```

Short text goes straight through [`PUT /api/files/<path>`](#44-write-a-small-file-one-step-shortcut):

```http
PUT /api/files/Inbox/2026/10/28/note.md
Authorization: Bearer abcd2345efgh2345
Content-Type: text/markdown

今天聊出来的那段……
```

Images, videos, and PDFs go through [`POST /upload`](#45-upload-any-size-always-three-steps):

```http
POST /upload
Authorization: Bearer abcd2345efgh2345

{"path":"Inbox/2026/10/28/photo.jpg","size":248123,"mime":"image/jpeg"}
→ presigned part URL
PUT <part URL>                       # bytes go straight to COS
POST /upload/complete                # verify the size and write the index
```

- The short code is 16 lowercase base32 characters, **without the `eam_` prefix**; sending it as
  `Authorization: Bearer <short code>` is a credential for the full file API.
- A save link has the same rights as the user: no prefix, no capability bits, no expiry by default.
- The path is the agent's choice; `Inbox/YYYY/MM/DD/` is a good spot. `PUT /api/files` creates parent directories
  automatically and does not overwrite by default (`?overwrite=1` or `If-Match` overwrites).
- `PUT /api/files` caps at 32 MiB; larger files go through `/upload`, with bytes never passing through the server.
- Saving is audited as the existing file API's `put` / `upload` / `upload_complete`, visible in the account
  page's "recent activity".
- `/api/save-links` (cookie session) on the account page creates, lists, and revokes them; after revocation
  `/s/<code>` 404s immediately.
- The MAC in the short code is derived from a server secret; replacing `COS_SECRET_KEY` invalidates existing save
  links.

### 4.15 Connect: the QR payload

The web app's `/connect` page mints the same Bearer tokens as the CLI. An **App** is handed over as a QR code;
a **Token** is handed over as a copyable text block. The QR is only a transport — the API calls are the ones
documented above.

```json
{
  "v": 1,
  "type": "eam.connect",
  "api": "https://everythingabout.me",
  "id": "ab12cd34ef56",
  "token": "eam_...",
  "name": "Obsidian",
  "scope": "Apps/obsidian",
  "read": true,
  "write": true,
  "events": false,
  "expiresAt": "2027-01-27T04:00:00.000Z"
}
```

- `api` is the base URL; `token` is the Bearer secret. Those two are all an app needs to store.
- `v` is the payload version and `type` is always `eam.connect`; if either is not understood, say "update the
  app" / "not an Everything About Me code" and stop.
- `name` / `scope` / `read` / `write` / `events` are display metadata. Confirm them with
  `GET /me` before trusting them; a `401` means the connection was revoked or expired.
- `expiresAt` is ISO-8601 UTC, or `null` for an App created without an expiry.
- The QR contains the actual token. It is shown once and can be revoked from the same page; treat a screenshot
  as a credential.
- `/api/connect/tokens` (the page's own create/list/revoke endpoint) is **cookie-session only**; third-party
  apps never need it and cannot call it with a Bearer token.

---

## 5. Things the server does behind your back

Callers do not need to know, but the user does:

| Behavior | How callers see it |
|---|---|
| The object key is the real path | Any S3 tool can see it directly |
| Maintaining the directory index | It is just listed |
| Computing sha256, recording audit | Visible in the account page's "recent activity" |
| Repair on read (in the bucket but not the index) | Fixed automatically on the first HEAD/GET |

Sharing, quotas, and account management live on the **website** and stay out of this API.

---

## 6. Status codes

| Code | Meaning |
|---|---|
| 200 / 201 / 204 | Success (201 = created, 204 = deleted, no body) |
| 202 | Accepted; check `Location` for progress |
| 206 | Range |
| 302 | Download redirect |
| 304 | Not modified |
| 400 | `invalid_path`, malformed request body, `meta=1` removed |
| 401 | Missing or invalid token |
| 403 | Outside the prefix, or a read-only token trying to write |
| 404 | Not found |
| 409 | Name conflict, non-empty directory, too many objects to move/copy |
| 412 | Conditional write failed |
| 413 | Over 32 MiB, use `/upload` |
| 429 | Rate limited, with `Retry-After` |
| 500 | Server error; `job_failed` carries an `id`, so you can `POST /{action}/{id}/retry` |
| 507 | Quota exhausted |

---

## 7. Implementation plan (Go)

| Phase | What | Status |
|---|---|---|
| P1 | `api_tokens` table, Bearer auth, `/list` `/api/files` `/move` `/me`, presigned downloads, CORS | ✅ |
| P2 | `/upload` + `/upload/complete` (single part) | ✅ |
| P3 | `/search`, directory moves (async), quota and rate limiting, audit | ✅ |
| P4 | Real multipart (> 5 GiB) | ✅ |
| P5 | `mkdir`, `copy`, `Prefer: respond-async` + cancel, `/upload/{id}` resume, `/capabilities` | ✅ |
| P6 | — (explicitly not doing version history, trash, `id`, `/tasks`) | — |
| P7 | Save links `/s/<code>`, account page links and recent activity | ✅ |

Implementation details:

- Tokens are `eam_<12-char id>_<secret>`; the database stores only `sha256(secret)` and verification uses a constant-time comparison.
- Permissions: `prefix` (empty = the whole drive) + read-only/read-write; going outside returns `403` rather than trusting the caller to behave.
- Directory listing uses keyset pagination (`is_dir desc, lower(name), id`) with an opaque base64 cursor.
- `PUT` supports `If-Match` / `If-None-Match: *`; over 32 MiB returns `413` and points you at `/upload`.
- Move/copy is "copy, then delete the source, then rewrite the index": if the copy fails the source is still there, so no data is lost.
- Large files go through upload sessions: `POST /upload` creates one (5 TiB cap) and hands out presigned PUT addresses;
  a new upload to the same path invalidates the old session; `complete` verifies the real size with `HeadObject`.
- Long operations do not build a "task center": each action carries its own progress address (`/move/{id}`, `/copy/{id}`),
  and there is only one job table (`fs_jobs`, with an `op` column), purely an internal detail.
- Search filters on `q` (filename/path), `path`, `mime`, and `type`, with cursor grouping identical to `/list`.
- Quota: `USER_QUOTA_BYTES` (0 = unlimited); before a write it checks "used − old file + new file" and returns `507` when exceeded.
- Rate limiting: 600 per minute per token (per IP when there is no token); over the limit returns `429` + `Retry-After`, `X-RateLimit-Limit`.
- Audit: writes are recorded in `audit_log` (action / path / status / ip / token / detail); a failed audit write only logs.
- Multipart upload: sliced automatically above the threshold, one presigned URL per part; at `complete` the `{partNumber, etag}` pairs go to COS to be combined.
- Error body: `{"error":"not_found","message":"..."}` with a matching HTTP status; `message` is always English (lowercase, no period) and `error` is a stable machine-readable code.
- Save links: a 16-character base32 short code; GET yields the token + prompt, and saving reuses `PUT /api/files` and `/upload` rather than adding a new write entrance.

The web-facing `/api/fs/*` stays as it is, untouched. The public API lives in `backend/internal/api/api_*.go`,
auth in `backend/internal/auth/token.go`, and jobs in `backend/internal/jobs`.

---

## 8. Four deliberate tradeoffs

### 8.1 No version history, no trash

Overwriting takes an explicit `?overwrite=1`, and deleting is permanent. A folder should be a folder: one less
layer of "hidden state", and one less concept for both the user and the agent to understand.

### 8.2 No version number in the URL

This is a stable interface for programs: adding fields breaks nothing, removing fields gets announced ahead of
time. If we really must break, use the `Sunset` response header plus an announcement, not a different path.

### 8.3 No SDK needed

About a dozen endpoints in total, all HTTP verbs and JSON; twenty lines of `requests` wrap the whole thing. A CLI
or an MCP server later goes on top of this layer, without changing the protocol.

### 8.4 No ids exposed, no "tasks" exposed

Both come down to "keep implementation details in the implementation":

- **id**: S3 has no object id, WebDAV has no id, and the ones that do (Drive's `fileId`, Dropbox's `id:`) mint it
  themselves. Internally we have a row primary key (unchanged by rename/move), but it **never enters JSON**. The
  external identity is the path, the content identity is `sha256`. There is exactly one cost: an app that keeps
  its index locally can no longer recognize "the same batch of files" after the user reorganizes directories — so
  let it keep the index alongside the data (stored inside the directory being analyzed) rather than referencing
  across the tree. That also removes a class of 404 semantics where "the path exists but the id does not".
- **Tasks**: what users want is "is this directory done moving", not "what is an async task". So there is no
  `/tasks`, no queue name, no `type` field, only each action's own `{id}`.

When looking at OpenList (the AList fork) we **copied capabilities, never shape**: we took the semantics of its
`fs/mkdir`, `fs/copy`, `fs/multipart/*`, `fs/task/*`; its `POST everything`, its `{code,message,data}` envelope,
its `page/per_page` offset pagination, and its 84 task sub-routes were copied not at all.
