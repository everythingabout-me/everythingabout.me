# Transfer protocol

> Replaces the earlier `embedded` / `cos-signed` transport split and its EdgeOne signed-part deployment
> notes. Every storage connection now belongs to exactly one transfer class, and the class determines how
> bytes move. Identity, authorization, quota, indexing, receipts, change feed and the publication ledger
> are shared by all three classes and are not redefined here.

## 1. Three classes

A connection's transfer class is declared by its driver, fixed for the life of the connection, and recorded
on every session it creates.

| Class | Protocol | Storage backends |
|---|---|---|
| **A** | `presigned-parts` | COS, S3, R2, B2 and other S3-compatible object stores |
| **B** | `resumable-session` | Google Drive, OneDrive, Dropbox, Box |
| **C** | `relay` | Local filesystem |

The class is a property of the storage layer, not of a deployment and not of an individual transfer. A
small object on class A is class A served by a single part; it is not class C.

**No implicit downgrade.** A connection that cannot serve its class fails the transfer with a typed error.
It never silently falls back to another class, and no deployment switch can move an existing connection
between classes. A class-A origin that is unavailable is an outage, not a reason to relay bytes through
the application.

## 2. Invariants

1. **Bytes do not traverse the application** in classes A and B. Only control requests and receipts do.
2. **Control round trips are constant.** Opening a session and committing it are two round trips,
   independent of file size and part count. A session's part grants are issued once, at open.
3. **One ledger.** All three classes converge on `open → committing → committed` with the same guards,
   tombstones, idempotency records and change facts.
4. **The application must account for bytes it never saw.** Every grant is scoped to one exact transfer,
   bound to exact geometry, and short-lived.
5. **Nothing is acknowledged before the provider durably holds it.** A client success response is never
   sufficient evidence.

## 3. Session lifecycle

Four endpoints are shared by all classes:

| Method | Path | Purpose |
|---|---|---|
| `POST` | `B/uploads` | Open a session and receive the class payload |
| `POST` | `B/uploads/{id}/commit` | Verify provider state and settle the publication |
| `GET` | `B/uploads/{id}` | Authoritative counters and state, for recovery |
| `DELETE` | `B/uploads/{id}` | Abort, subject to the commit rules below |

`POST B/uploads` carries the destination, size, media type, condition and an optional client digest
assertion. Its response is the same envelope for every class, with a class-specific payload:

```json
{
  "id": "u_...",
  "class": "A",
  "state": "open",
  "size": 18234122311,
  "partSize": 33554432,
  "partCount": 544,
  "maxParallelism": 16,
  "expiresAt": "2026-10-31T04:00:00Z",
  "commitUrl": "/api/files/uploads/u_.../commit",
  "transport": { }
}
```

`transport` is the only class-dependent field. It is opaque to everything except the client's transfer
executor; no other component branches on class.

## 4. Class payloads

### A — `presigned-parts`

```json
{
  "kind": "presigned-parts",
  "uploadId": "<native multipart id>",
  "parts": [
    { "number": 1, "size": 33554432, "url": "https://...", "headers": { "Content-MD5": "..." } }
  ],
  "partCount": 544
}
```

`parts` carries every signed part URL at open time, so a normal transfer never asks the service for another
grant. The request already carries `size`, so the part count is known before the first byte and the service can
sign the whole set once. There is no paging endpoint and no second grant flow: if a size would need more parts
than the connection allows, the service raises `partSize` (up to the provider's maximum) or rejects the session
before any bytes move.

Each URL binds `PUT`, the exact object key, `uploadId`, `partNumber`, the exact part length and the part
digest. One URL accepts exactly one length — the final short part therefore has its own URL and cannot
reuse an earlier signature.

### B — `resumable-session`

```json
{
  "kind": "resumable-session",
  "url": "https://...",
  "chunkSize": 10485760,
  "maxParallelism": 1,
  "completion": "implicit",
  "credential": { "scheme": "bearer", "value": "...", "expiresAt": "..." }
}
```

`credential` is present only when the provider cannot be written to or read from without one. `completion`
is `implicit` when the final chunk completes the object, and `explicit` when the provider requires separate
start/finish calls (Dropbox).

A provider credential is scoped to the application's own folder, bound to one transfer, and lives for the
provider's natural window. Do not shorten it below what a normal transfer needs: a credential that expires
mid-upload costs a restart, while the residual risk it removes is already bounded by the folder scope and the
single-transfer binding. Accept that a widened grant may outlive the transfer; treat expiry as cleanup, not as
the security boundary.

### C — `relay`

```json
{
  "kind": "relay",
  "url": "/api/files/uploads/u_.../parts",
  "partSize": 33554432,
  "maxParallelism": 16
}
```

The relay URL is application-relative and carries no third-party origin. This is the only class in which
the application holds the bytes, and the only class available to a backend that physically cannot be
written to directly.

## 5. Geometry is dictated by the provider

The server chooses `partSize` and `maxParallelism`; a client never negotiates or second-guesses them.

| Class | Part size | Alignment | Parallelism | Final part |
|---|---|---|---|---|
| A | server-chosen, ≥ 5 MiB except last | none | up to server bound | may be short |
| B — Google Drive | multiples of 256 KiB | 256 KiB | **1** (single cursor) | may be short |
| B — OneDrive | ≤ 60 MiB, recommended 5–10 MiB | **320 KiB** | **1** (ordered) | must divide by 320 KiB |
| B — Dropbox | 4–8 MiB typical, ≤ 150 MB per call | none | **1** (cursor) | may be short |
| B — Box | ≥ 5 MiB | 320 KiB | parallel parts | may be short |
| C | server-chosen | none | up to server bound | may be short |

OneDrive and Google reject out-of-order fragments; the session reports `maxParallelism: 1` for them. A
client that fans out anyway gets provider errors it cannot interpret, so the bound is authoritative.

## 6. Writing

**A** — the client issues parallel `PUT`s to the URLs it already holds. No acknowledgement is sent per part.

**B** — the client sends sequential `PUT`s to the single session URL with `Content-Range`, or the
provider's cursor equivalent. `308 Resume Incomplete` means the chunk was accepted; `416` means the range
was already received. Final-chunk success returns provider metadata that the client may discard.

**C** — the client issues parallel `PUT`s to the application, which streams them to the driver.

In all three classes the client hashes locally per part. Local hashing produces the digest that class A
binds into the signature; it never requires a whole-file pass before upload.

## 7. Reading

Reads mirror writes: the service issues one grant and the client fetches bytes itself. A read grant returns
the URL, the request headers, the expiry, and the representation facts the client needs to plan ranges.

| Class | Read grant |
|---|---|
| A | One presigned `GET` URL; any `Range` is accepted, so the client fans out ranges itself |
| B — OneDrive | `@microsoft.graph.downloadUrl`, short-lived and pre-authenticated |
| B — Google Drive | `files.get?alt=media` with a short-lived access token |
| B — Dropbox / Box | provider content URL with a short-lived access token |
| C | An application-relative `GET` content URL |

A read is one control round trip regardless of class. The client does all range parallelism and resume
itself; a failed range is retried against the same grant, not re-granted.

Conditions must be enforceable where the bytes are served. A grant advertises `If-None-Match` or `If-Range`
only when the provider enforces them natively; otherwise the grant omits them and the client revalidates
through the service instead of assuming it can detect a changed representation mid-transfer.

Delivered headers are the provider's, not the service's: `Content-Type`, `Content-Disposition`, `ETag` and
`Accept-Ranges` on a direct read come from the provider's response. A provider that cannot set a safe
disposition or `nosniff` makes that connection unsuitable for serving active content inline, and the
service must route such representations through class C or refuse the inline request.

## 8. Commit

Commit is server-controlled in every class. The client only announces completion; it never asserts it.

| Class | What commit does |
|---|---|
| A | Lists native parts, verifies exact count/length against the bound geometry, performs `CompleteMultipartUpload` once, then settles index and quota |
| B | Re-reads the provider object by bound identity, verifies length and any provider digest, then settles |
| C | Verifies the received bytes it already holds and publishes |

Class A does not require the client to report part ETags. The server lists them itself, which removes the
need to expose `ETag` through CORS and keeps the confirmation authority server-side.

`complete` is idempotent: repeating it returns the same settled result or the same pending Operation.

## 9. Recovery depends on queryability

This is the one place where the classes genuinely differ in behaviour rather than in bytes.

| Class | Can the provider be asked what it received? | Bounded by |
|---|---|---|
| A (COS/S3) | Yes — `ListParts` | — |
| B — OneDrive | Yes — upload-session status | — |
| B — Google Drive | No | expiry |
| B — Dropbox | No | expiry |
| C | Yes — the application holds the state | — |

A session that lost a reply and can be queried is resolved immediately. A session that cannot be queried
enters `unknown until expiry`: the only safe conclusion is that an outstanding provider request may still
land, so the guard is retained and **idle expiry is the resolution event**.

Bind the **idle** interval, not the total lifetime. A guard may be released once the session has accepted no
progress for longer than any outstanding provider request could plausibly survive — minutes, not hours. The
total lifetime may still be long and is extended by accepted progress, because a large upload legitimately
takes hours. Conflating the two either strands a guard for hours or kills a healthy long upload.

A provider that can be queried may advertise a long TTL and may be resolved without waiting.

Class A and class C never rely on expiry to resolve an ambiguous write.

## 10. What may be signed

The rule is not "what the client can set" but **"is the value deterministic"**.

| Category | Examples | Sign it | Send it to the client |
|---|---|---|---|
| Browser-derived | `Host`, `Content-Length` | yes | **no** — JavaScript cannot set them, and the browser derives them |
| Client-set | `Content-Type`, `Content-MD5`, `Content-Range`, `If-None-Match` | yes | yes, and allowlist them for preflight |
| Browser-chosen | `Accept-Encoding`, `User-Agent`, `Accept-Language` | **never** | no |

Consequences that are easy to miss:

- A signed `Content-Length` requires the body to be a fixed-length `Blob`/`File` of exactly that many
  bytes. A `ReadableStream` body is sent chunked and carries no `Content-Length`, so the signature fails.
- Signing a length binds one URL to one exact size. Last parts need their own grant.
- Signing gives integrity **relative to the client's declaration**, not correctness of that declaration.
  A receipt that rests on it is recorded as an assertion plus provider verification, never as an
  application-computed digest.

## 11. Deployment

Class A supports two deployments:

- **Same-origin edge relay** — the browser writes to an application-relative path, and an edge forwards
  the request to the provider. The site keeps one origin and the signature binds the provider host, so the
  edge must rewrite to exactly that host. This is the default for uploads because it removes the largest
  source of BYO-bucket onboarding failures.
- **Direct to provider** — the browser writes to the provider endpoint and the bucket must carry a CORS rule:
  exact origins, `PUT`, `GET` and `HEAD`, the client-set request headers above, exposed `Content-Range` and
  `ETag`, and an explicit `MaxAgeSeconds`. `ETag` exposure is not needed for uploads, because the server lists
  parts itself; it is needed for ranged reads.

Reads are direct in both deployments, so a class-A connection requires the bucket CORS rule regardless of how
its uploads are routed. An edge relay only spares the operator from configuring CORS for writes; it does not
remove the requirement overall. Do not build a second edge route for reads to avoid one bucket rule.

Class B needs no edge and no CORS configuration by us: provider CORS behaviour is fixed and is a
capability of the connection, not something an operator can repair.

Class C needs nothing.

When an edge sits in the byte path it must: preserve method and body, preserve every signed query
parameter byte for byte, strip application credentials and cookies, pass `OPTIONS` through unmodified,
disable caching, and perform no redirect. Rule failure must fail the upload, never re-route it to the
application.

## 12. Prohibitions

- No redirect anywhere in the byte path. A redirect response carries no CORS headers and surfaces as an
  empty error.
- Cross-origin transfer requests use `credentials: "omit"`. Provider session cookies must never be sent.
- No implicit class downgrade, and no deployment switch that moves an existing connection between classes.
- No acknowledgement from client-reported success alone.
- No signing of browser-chosen headers.
- No idle TTL so short that a healthy long transfer is killed; holding a guard longer is safe and only delays
  reuse of that path.
- No whole-file digest requirement. A whole-file hash may be accepted as an assertion and verified only
  where the provider can be read back, and it is never a precondition for a direct transfer.
