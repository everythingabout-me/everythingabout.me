# Transfer protocol

> Target contract for how file bytes move between clients and storage. Identity, authorization, quota,
> indexing, receipts, change facts and the publication ledger are shared by every class and are defined in
> [storage](storage.md), the [driver contract](storage-driver.md) and the [file API](../api/api.md).
>
> Implementation status: not yet implemented. The current runtime relays bytes through the application or,
> for COS, issues per-part `authorize`/`confirm` grants; both are replaced by this document.

## 1. Three classes

| Class | Kind | Byte path | Typical backends |
|---|---|---|---|
| **A** | `presigned-parts` | Client ↔ object store, through presigned requests | COS, S3, R2, B2 and other S3-compatible stores |
| **B** | `resumable-session` | Client ↔ cloud drive, through a provider upload session or download link | OneDrive, Google Drive, Dropbox, Box |
| **C** | `relay` | Client ↔ application ↔ backend | Local filesystem; any backend the client cannot reach directly |

A driver declares which classes it can serve. Each storage connection **selects exactly one** of them when it
is configured, and every session and read grant records it. COS/S3 drivers can serve A or C; cloud-drive
drivers serve B or C; Local serves only C. Whether a client can reach a provider is a property of the
deployment, so the choice belongs to the connection, not to the driver.

**No fallback.** A transfer never switches class. An unavailable direct route fails with a typed error; it is
not retried through the application. Changing a connection's class is an explicit administrative
reconfiguration, allowed only while the connection has no open sessions, and it never happens per transfer.

## 2. Invariants

1. **The application never carries file bytes in classes A and B.** Only control requests do. Derived
   representations (thumbnails, converted previews) are application output and are not covered by this rule.
2. **Only the server publishes.** A client transfers bytes into unpublished provider state (a native multipart
   upload, a staging file or an open provider session); only a server commit makes them a file at the
   destination path. This holds by construction wherever the client holds only a session-scoped capability
   (classes A and C, OneDrive, Google Drive, Box staging). On Dropbox the client holds a provider token and
   publication by the server is a client contract (§5). A client success response is never evidence.
3. **One ledger.** All classes converge on `open → committing → committed` with the same guards, tombstones,
   idempotency records and change facts.
4. **Every grant is scoped to one session or one file**, time-limited and recorded before it is issued. Upload
   grants also bind the exact part length.
5. **No whole-file pass before upload.** No class requires the client to read the whole file before the first
   byte is sent.

## 3. Upload lifecycle

| Method | Path | Classes | Purpose |
|---|---|---|---|
| `POST` | `B/uploads` | all | Open a session; returns the class payload |
| `POST` | `B/uploads/{id}/parts/sign` | A | Sign a batch of part URLs (next window or renewal) |
| `PUT` | `B/uploads/{id}/parts/{number}` | C | Send one part to the application |
| `GET` | `B/uploads/{id}/parts` | A | The provider's own part listing (number, size, provider ETag) |
| `GET` | `B/uploads/{id}/parts` | C | Durable receipts in acknowledgement order with a fixed watermark |
| `GET` | `B/uploads/{id}` | all | Authoritative state and counters |
| `POST` | `B/uploads/{id}/commit` | all | Verify provider state and publish |
| `DELETE` | `B/uploads/{id}` | all | Abort an open session |

`POST B/uploads` carries the destination or replacement target, size, media type and condition. The response
envelope is the same for every class:

```json
{
  "id": "u_...",
  "class": "A",
  "state": "open",
  "size": 18234122311,
  "partSize": 33554432,
  "partCount": 544,
  "maxParallelism": 16,
  "expiresAt": "2026-11-06T04:00:00Z",
  "commitUrl": "/api/files/uploads/u_.../commit",
  "transport": { }
}
```

`transport` is the only class-dependent field and is opaque to everything except the client's transfer
executor. Part numbers start at 1, offset is `(number-1)*partSize`, all parts but the last are exactly
`partSize`, and a zero-byte session has no parts and commits immediately.

## 4. Class A — presigned parts

### Opening and signing

The session is a native multipart upload created by the server. The open response carries the first window of
signed part URLs; further windows and renewals come from one batch endpoint:

```json
{
  "kind": "presigned-parts",
  "parts": [
    { "number": 1, "size": 33554432, "url": "https://...", "expiresAt": "2026-11-05T05:00:00Z" }
  ],
  "signUrl": "/api/files/uploads/u_.../parts/sign"
}
```

```http
POST B/uploads/u_.../parts/sign
Content-Type: application/json

{"numbers":[65,66,67,68]}
```

- Each URL binds `PUT`, the exact object key, `uploadId`, `partNumber` and the exact part length
  (`Content-Length`). It binds **no content digest**, so the client never hashes before sending.
- The server chooses the window size: enough parts to keep `maxParallelism` busy for several minutes, and the
  whole file when it is small. The client asks for the next window when fewer than a quarter of its unsent URLs
  remain, and re-signs any part whose URL expired or was rejected with 403. Signing the same number again
  returns a fresh URL for the same bound length; it cannot change the part's size or object.
- URLs live about one hour. A leaked URL can write only one part of an unpublished upload owned by that
  session, and aborting the session invalidates every URL at once.
- A sign request counts as session activity for idle expiry.

If the size would need more parts than the provider allows, the server raises `partSize` up to the provider
maximum or rejects the session before any bytes move.

### Writing

The client issues parallel `PUT`s up to `maxParallelism`, with no per-part acknowledgement to the
application. It may send `Content-MD5` as an unsigned header; a provider that checks it rejects a part
corrupted in transit, and the server never relies on it. Until commit, anyone holding a part's URL may rewrite
that part by sending it again; commit freezes whatever the provider lists. Part content is not otherwise
verified against the client, which is an accepted trade-off.

`GET B/uploads/{id}/parts` returns the provider's own part listing (number, size, provider ETag), so a client
that lost track of progress resumes from what the provider actually holds.

### Commit

The server lists native parts, verifies that every number is present with its exact bound length, performs
`CompleteMultipartUpload` once at the final key and settles the index and quota. The client reports no ETags,
and the receipt records "provider-listed length" as its evidence.

## 5. Class B — cloud-drive sessions

The server creates the provider upload session and the client only sends bytes. Wherever the provider allows
it, the session targets a **staging folder** of the connection, so even a client that finishes the provider
upload itself can only produce a staging file; the server commit then moves it to the destination. Create and
replace follow the same path and differ only in the final move.

| Provider | Upload target | Client writes | Server commit | Create (no replace) | Replace |
|---|---|---|---|---|---|
| OneDrive | `createUploadSession` for a temporary name in the staging folder | Ordered `PUT`s to the pre-authenticated `uploadUrl`; the final fragment creates the staging file | Move into the destination folder | Move with `conflictBehavior: fail` | Move with `conflictBehavior: replace` |
| Google Drive | Resumable session in the staging folder under a temporary name, initiated with the browser's `Origin` | Ordered `PUT`s to the session URI; the final chunk creates the staging file | `files.update`: rename and move into the destination folder | Not atomic: Drive allows duplicate names, so only the application guard prevents them | Move the new file in, then delete the old one (briefly two siblings, never none) |
| Box | `POST /files/upload_sessions` in the staging folder under a temporary name | `PUT` parts with the per-part SHA-1 `Digest` Box requires | Box session commit with the part list and the whole-file SHA-1, then move into the destination | Move; Box rejects name conflicts | Rename the old file aside, move the new one in, delete the old one |
| Dropbox | `upload_session/start` (no bytes) | `upload_session/append_v2` with the session ID and offset | `upload_session/finish` directly at the destination path | `mode: add`, `autorename: false` | `mode: update` with the observed `rev` |

Dropbox chooses the destination in the `finish` call itself, so a staging folder cannot bind it, and its token
can write anywhere in the app folder. On Dropbox, server-only publication is therefore a client contract, backed
by the rule that such a token is only issued to a principal whose scope already covers the whole connection.

Consequences, stated rather than worked around:

- **Replacement through staging gives the file a new provider identity** on OneDrive, Google Drive and Box: the
  provider's own version history, file ID and share links do not carry over. Dropbox `update` keeps them. The
  service ID, path and contract are unaffected.
- **Google Drive has no atomic create.** Its driver reports `AtomicCreate=false` and `GET B/capabilities`
  reports `atomicCreate:false`; uniqueness rests on the application guard and does not hold against outside
  writers.
- An abandoned session leaves at most a staging file or an unfinished provider session, never a file at the
  user's path; the expiry sweeper deletes it.

Payload:

```json
{
  "kind": "resumable-session",
  "url": "https://...",
  "chunkSize": 10485760,
  "maxParallelism": 1,
  "credential": { "scheme": "bearer", "value": "...", "expiresAt": "..." }
}
```

`credential` is present only for providers whose write endpoint is not self-authenticating (Dropbox, Box);
OneDrive and Google Drive session URLs carry their own authority. Such a provider token usually covers the
connection's whole app folder rather than one transfer. This is an accepted trade-off with one hard limit:
**a provider credential is never issued to a principal whose own scope is narrower than the area the credential
can reach.** A scoped application token on such a connection gets `403 transfer_scope_exceeded`; serving scoped
applications requires configuring the connection as class C.

Box needs the whole-file SHA-1 at commit. The client computes it with an incremental hasher in a sequential
read that runs alongside the part uploads, so it never delays the first byte, and sends it with the commit
request; Box verifies it.

## 6. Class C — relay

```json
{ "kind": "relay", "url": "/api/files/uploads/u_.../parts" }
```

The client `PUT`s numbered parts to the application, in parallel up to `maxParallelism`. The application
streams each part to the backend (unpublished native multipart for object stores, isolated staging for Local)
while computing SHA-256, and acknowledges only after the bytes and a durable receipt exist. An identical
replay returns the existing receipt; a different body for an acknowledged part returns `409 part_conflict`.
Commit verifies coverage from the receipts and publishes.

## 7. Geometry

The server chooses `partSize`, `chunkSize` and `maxParallelism`; the client follows them exactly.

| Route | Part size | Non-final parts | Final part | Parallelism |
|---|---|---|---|---|
| A | server-chosen, default 32 MiB | exactly `partSize`, ≥ 5 MiB | any size | up to 16 |
| B — OneDrive | ≤ 60 MiB, recommended 5–10 MiB | multiple of 320 KiB | any size | 1 (ordered) |
| B — Google Drive | server-chosen | multiple of 256 KiB | any size | 1 (ordered) |
| B — Dropbox | server-chosen, ≤ 150 MB per call | any | any size | 1 (offset-ordered) |
| B — Box | dictated by Box in the session | exactly Box's part size | any size | parallel |
| C | server-chosen, default 32 MiB | exactly `partSize` | any size | up to 16 |

## 8. Reading

`POST B/downloads` returns grants in batches, so a gallery page needs one request:

```json
{
  "items": [{
    "id": "f7ca1104-12f0-4bbf-8c77-41d341f7d120",
    "kind": "url",
    "url": "https://...",
    "headers": {},
    "expiresAt": "2026-11-05T05:00:00Z",
    "size": 18234122311,
    "contentType": "video/mp4",
    "acceptRanges": true,
    "etag": "\"...\"",
    "conditions": ["if-range"]
  }]
}
```

| Route | Grant kind | Source |
|---|---|---|
| A | `url` | Presigned `GET` with `response-content-type` and `response-content-disposition` set from the index |
| B — OneDrive | `url` | `@microsoft.graph.downloadUrl` (pre-authenticated, short-lived) |
| B — Dropbox | `url` | `files/get_temporary_link` (no token needed) |
| B — Box | `url` | The pre-authenticated location from `/files/{id}/content`, read by the server without following the redirect |
| B — Google Drive | `bearer` | `files/{id}?alt=media` plus an `Authorization` header |
| C | `url` | Application-relative `GET B/content` |

- **`url` grants work anywhere**, including `<img>`, `<video>` and download links.
- **`bearer` grants need the request header.** Native clients and JavaScript `fetch` set it directly, which
  covers downloads and images (shown through a `blob:` URL). Media elements cannot add headers, so in-browser
  streaming of Google Drive audio and video is an open question, to be settled when a Google Drive connection is
  built. The scope limit of §5 applies: a bearer grant is issued only to a principal whose scope covers the
  token's reach.
- **Expiry is renewed, not avoided.** A client that receives 403 or passes `expiresAt` requests a new grant
  and continues from the bytes it already has; long downloads and video seeking keep working this way.
- **Cacheable URLs.** For class A the server rounds `expiresAt` up to the next window boundary, so repeated
  grants within that window return the identical URL and the browser cache stays effective. A URL therefore
  lives between the minimum grant lifetime and that minimum plus one window. Responses the browser cached from
  the provider remain viewable on that device after logout; this is accepted.
- **Conditions** list only what the route enforces natively. Without `if-range` the client revalidates
  through the service before resuming instead of assuming it would detect a changed file.
- **Active content.** Direct URLs are served from the provider's domain, never the application origin, so they
  cannot reach application cookies. Class A forces `attachment` disposition except for the safe inline media
  allowlist. Thumbnails and converted previews remain application-served same-origin representations.

## 9. Recovery and expiry

Every route can be asked what it holds:

| Route | How the server learns the provider state |
|---|---|
| A | `ListParts` |
| B — OneDrive | `GET` on the upload session (`nextExpectedRanges`) |
| B — Google Drive | `PUT` with `Content-Range: bytes */<size>` returns the received range |
| B — Dropbox | An append at the wrong offset returns `incorrect_offset` with the correct one |
| B — Box | `GET .../upload_sessions/{id}/parts` |
| C | The application's own receipts |

Because clients never publish (§2), an interrupted client write leaves only unpublished bytes: the client asks
for the session status and resumes. A client request never makes the destination path ambiguous. The only
ambiguous effects are the server's own commit calls, handled by the ordinary `outcome_unknown` guard and
reconcile path.

Open sessions expire after 24 hours of inactivity (sign requests, status queries and relayed parts count as
activity), with a hard limit of seven days, capped by the provider session's own lifetime. Expiry aborts the
native upload or provider session and deletes any staging file. Committing sessions do not expire; they follow
worker leases and recovery.

Abort deletes the provider state: `AbortMultipartUpload` (A), `DELETE` on the session or staging file (B), the
staged bytes (C). Dropbox has no abort call; an unfinished Dropbox session lapses and never becomes a file.

## 10. Signing rules

Sign a header only when its value is deterministic and the client sends it exactly:

| Category | Examples | Sign | Return to the client |
|---|---|---|---|
| Browser-derived | `Host`, `Content-Length` | yes | no — the browser sets them |
| Client-set | `Content-Type`, `Content-Range`, `If-None-Match` | only when the server fixes the value | yes, and allow them in preflight |
| Browser-chosen | `Accept-Encoding`, `User-Agent`, `Accept-Language` | never | no |

- A signed `Content-Length` requires a fixed-length `Blob`/`File` body; a `ReadableStream` body is sent
  chunked and fails the signature.
- Content digests are never signed. A digest the client sends is an unsigned integrity hint.
- Sign with a dedicated provider identity that can perform only the granted actions (`UploadPart` for upload
  grants, `GetObject` for read grants). Never sign with a credential that can complete, delete or list.

## 11. Deployment

**Class A.** The bucket carries a CORS rule: the exact application origins, `PUT`, `GET` and `HEAD`, the
client-set headers above, exposed `ETag` and `Content-Range`, and an explicit `MaxAgeSeconds`. `eam doctor`
checks it. Uploads may additionally go through a same-origin edge relay: the browser `PUT`s to
`/api/files/transfer/<object-key>?<signed query>` and the edge forwards it to the provider unchanged except
for removing the prefix and setting the provider `Host`. The edge must preserve method, body and every signed
query byte, strip cookies and application credentials, pass `OPTIONS` through, disable caching and never
redirect. The application refuses that path (`503 edgeone_route_required`), so a missing edge rule fails the
upload instead of consuming application bandwidth.

**Class B.** Register the provider application once: Box requires the application origins in its CORS
allowlist; Google Drive sessions are initiated with the browser's `Origin`; OneDrive and Dropbox need no
origin configuration. OneDrive, Google Drive and Box connections keep a staging folder inside the app folder,
outside the user's content root.

**Class C.** Nothing beyond the application itself.

Provider behaviour in §5, §8 and §9 is the design basis; each row is admitted for a connection only after an
exact-connection probe confirms it.

## 12. Prohibitions

- No redirect in any byte path; a redirect carries no CORS headers and surfaces as an opaque failure.
- Cross-origin transfer requests use `credentials: "omit"`; provider cookies are never sent.
- No class fallback and no per-transfer class choice.
- No publication by a client, and no acknowledgement from a client success response alone.
- No signed content digests and no signed browser-chosen headers.
- No whole-file pass before upload.
- No provider credential for a principal whose scope is narrower than the credential's reach.
