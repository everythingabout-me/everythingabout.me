# P2: unified authenticated file reads

P2 exposes the read side of the [file API](../api/api.md) at one `/api/files` base for Cookie and
Bearer callers. Native bytes stream through the configured application origin on Local, COS and S3.
This phase adds no native file mutations. It builds on [P0 boundary evidence](storage-p0.md) and
[P1 native trees](storage-p1.md); durable mutation coordination, publication and resumable ingress
remain P3–P5, and Web/media client migration remains P6.

## Active resources and rollout boundary

| Method | Resource | P2 behavior |
|---|---|---|
| GET | `/api/files?path=/...` | Native-authoritative path metadata |
| GET | `/api/files/list?path=/...&limit=&cursor=` | Immediate native children, public keyset pagination |
| GET | `/api/files/search?q=&path=&kind=&mediaType=&limit=&cursor=` | Authorized indexed search checked against native directory observations |
| GET | `/api/files/me` | Principal kind, scope, observed indexed usage and cookie CSRF bootstrap |
| GET | `/api/files/capabilities?path=/...` | Effective read/name/range limits; writes/uploads/thumbnails disabled |
| GET, HEAD | `/api/files/content?path=/...` | Same-origin raw content, safe headers, exact single ranges |

JSON resources are plain objects/collections. Errors use `application/problem+json` with a stable
code and request ID, without provider messages, locators or secrets. Metadata/lifecycle responses
are `no-store`; byte responses use `private, no-cache` with no shared private cache.

The previous wildcard `/api/files/{path...}` family is removed, including its raw PUT/DELETE routes.
It cannot capture names such as `list`, `content`, `uploads` or `operations`; file paths are query values
and decoded exactly once. Unsupported wildcard/file-ID/version routes return 404, unsupported
methods 405 with Allow. The specified mutation/upload/operation/thumbnail resource paths are reserved
and return authenticated 501 until their phases land; cookie mutations must pass CSRF before that response.
No native write or new grant is issued by a placeholder.

Legacy `/api/fs/*`, root-level metadata/actions, account/event APIs and COS signing paths remain separate
until their controlled migration. They retain existing response parsers and must not be confused with the
new read guarantees. Existing browser pages continue their legacy calls in this phase. Removing the old
wildcard breaks old external content URLs intentionally; the new origin-relative content route is the
replacement. Issued native write authority still requires P0 drain/reconciliation before production
write activation. A legacy writer is not fenced by this read release.

## Authentication, CSRF, trusted origins and CORS

Explicit Authorization presence always selects Bearer. Empty, whitespace, unsupported, malformed,
expired or revoked credentials return 401 and never fall back to an ambient cookie. Valid Bearer ignores
cookies. Verification reuses existing token/save-link/session services; selecting a source is not enough
to authenticate it. Both sources open the same owner-bound tree and use the same read authorization.

Scoped tokens use absolute account paths. Segment boundaries and backend name equivalence prevent
`/Photos` from authorizing `/Photos-old`; moving content does not retarget a token. Out-of-scope paths
are concealed as 404. A known visible path with insufficient read permission returns 403, including
write-only and events-only credentials. Neither an index row nor a native locator grants access.

Cookie mutations at this base require both an approved Origin and a session-bound `X-CSRF-Token`.
The token comes from authenticated `GET /api/files/me`, is an opaque application-keyed MAC of the
existing cryptographically random session credential, and changes across sessions or application-secret
rotation. A token's presence is never considered validation. It is not a storage credential and is never
sent to a provider. Read responses never expose the session cookie itself.

Configure explicit browser/API origins:

```dotenv
# Comma-separated HTTP(S) origins, no paths, wildcard, userinfo or trailing slash.
FILE_COOKIE_ORIGINS=https://everything.example
```

Development defaults are `http://localhost:5173` and `http://127.0.0.1:5173`. Production has no default:
with an empty list, cookie mutations and credentialed cross-origin access are denied. Cookie same-origin
reads and explicit Bearer reads remain available. Proxy Host/Forwarded/X-Forwarded-* values cannot
expand the origin allowlist. Keep reverse-proxy forwarding policy explicit at the deployment boundary.

Third-party Bearer CORS permits the necessary file headers without credentialed wildcard access.
Allowed cookie origins are echoed individually with credentials; wildcard origins never accompany
`Access-Control-Allow-Credentials: true`. Preflight supports Range, strong conditions, Content-Digest,
Idempotency-Key, Prefer, source modification time and CSRF headers. Browser-readable response headers
include ETag, Accept-Ranges, Content-Range/Length/Disposition, Location, Retry-After and request ID.
Unsupported methods/headers or unapproved cookie origins fail explicitly. Account/event CORS is not
silently migrated as part of this file-only boundary.

## Metadata, native authority and pagination

Names retain exact native spelling, including accepted leading/trailing spaces, decomposed Unicode,
and literal `+`, `#`, `?`, `%` and percent-encoded-looking segments. Use a normal query builder; a
logical `%2F` filename is not decoded twice. Existing ambiguous names are reported as conflicts.

Metadata/list items contain no public file IDs. Native paths, kind, size and available MIME/modification
metadata are authoritative; matching index rows supply application creation/update times and unknown
MIME fallback. Index-only missing entries do not become native files. Without an indexed creation time,
`createdAt` is `0001-01-01T00:00:00Z` (unknown); `updatedAt` falls back to native modification time, or the
same zero value if unavailable. `sourceModifiedAt` is null when unknown. These are observation responses,
not a backfill of application history.

Metadata revision hashes include public representation/path, native evidence and direct folder child
membership. Ancestor rename changes the returned path/revision. Folder revision is not a recursive
subtree snapshot. Content revision is null for folders and when no trustworthy native version is available.
It never manufactures an immutable content identity from an index counter, size or mtime.

List/search sort folders first, then NFC name bytes, then normalized full-path bytes. Default public page
size is 200; maximum 1000. Cursors encrypt and authenticate their keyset and bind principal/scope,
connection/root generation, route, query and sort. They expose no native/database identity and survive
restart with a stable application secret. Reusing a cursor with another query, credential/scope or root
fails; ordinary concurrent writes do not globally invalidate it. Concurrent rename/move can still omit
or repeat paths; this is keyset pagination, not a snapshot or backup inventory.

Search reads owner/subtree-scoped index candidates and verifies current native entries before authorization,
filtering and pagination. Missing native paths can be skipped as stale index; native ambiguity/invalid names
are surfaced rather than silently hidden. Search is case-insensitive substring matching on visible path/name,
with optional kind and mediaType filters. `/me` reports scoped observed indexed usage, never another scope's
totals. It is not the complete native quota baseline: unindexed files may exist. Quota and reservation fields
are null until durable P3 accounting is implemented, and capabilities advertise writes/uploads false.

Read requests have finite budgets: at most 100000 indexed candidates/observed entries, bounded native
listing pages and a 30-second overall deadline for metadata/index/lifecycle requests. Search/usage reuse
request-local immediate-directory observations rather than re-stat every file's siblings. Budget exhaustion
returns 503; timeout returns a Problem without partial JSON. These are engineering admission limits,
not benchmarked performance claims. Large estate inventory and performance expansion remain explicit work.

## Streaming, conditions and media safety

Content GET/HEAD checks authorization before native metadata. The handler selects an actual native
`Open` representation and uses its size/type, rather than combining stale Stat headers with different
GET bytes. HEAD has matching representation headers and no body, and ignores Range. Root/folder
content returns `409 not_a_file`. Zero-byte files are ordinary valid responses.

Single ranges support explicit, open-ended and suffix bounds. Satisfiable ranges return 206 and exact
Content-Range/Length; unsatisfiable ranges return 416 with `bytes */N`. Multi-range requests are ignored
and return full 200. Conditions are evaluated before ranges. If-Range mismatch returns full 200.
`Accept-Ranges: bytes` is emitted only when the effective native route supplies exact ranges.

A strong ETag is emitted only after a successful native version-selected Open and equality checks of the
actual returned version. Its opaque content revision hashes root identity, exact native path, private native
reference and version, with length-framed inputs. Native Version must select immutable bytes within that
identity; a reusable arbitrary counter or echoed request is not proof. Native object-local versions are not
assumed globally unique. Local exposes no version and therefore no strong ETag: strong value conditions
fail safely, If-Range falls back to full 200, while existence wildcards still have ordinary HTTP semantics.
P1 descriptors conservatively advertise no universal pinned reads; some object files can still supply an
individually verified native version. The capabilities field does not promise universal validators.

P2 content revisions are tied to the observed native path/identity. An external native rename may change
them. API move/rename is not implemented in this phase; P4 must persist appropriate generation anchors
before promising the target contract's rename-stable contentRevision. Hashing root plus an opaque local
version alone cannot supply that guarantee.

Ranges may open/close a full native stream before opening the exact native range to establish selection,
conditions and truthful geometry. The body is never buffered as a full file. Retries of read selection are
bounded to three attempts; changing versions/size/range geometry never silently mix representations.
File streaming has no metadata request's 30-second cutoff; caller cancellation closes native bodies and
propagates backpressure. Errors after headers abort the connection; no JSON is appended to file bytes.
ResponseWriter wrappers preserve Unwrap/flush behavior. No compression changes byte offsets.

Content defaults to attachment with an encoded accepted filename and `nosniff`. Explicit inline disposition
is honored only for a narrow plain-text/raster-image/audio/video allowlist. HTML, SVG and other active
content remain attachments, and PDF/Office inline conversion is not enabled by this release. Thumbnail
conversion is unavailable and advertised false; it is not emulated with a provider redirect.

## Validation and progress

Tests cover actual Local trees and the real S3 SDK against an isolated native HTTP fixture, plus native
COS/S3 adapter conformance from P1. Native failures include malformed/ignored range status, actual GET
metadata changes, retained version selection, short streams, cancellation and response abort through the
outer middleware. Cookie/Bearer request parity, exact names, scoped opaque cursors, read denial, CORS and
CSRF are exercised through actual unified routes.

An opt-in integration suite uses a disposable Postgres 17 database, full migrations, real session/token
services, two owner roots and native scan/index import. It verifies the same JSON/bytes for both credential
sources, permission/scope isolation, metadata/list/search/me/capabilities, content ranges and media safety,
CSRF sessions and preflight. Full index JSON and native bytes remain unchanged after requests. No live
customer database, provider credentials or root was used; containers and test databases are removed.

```bash
make test
cd backend
go test -race ./internal/api ./internal/auth ./internal/config
# Actual DB integration requires a disposable CREATEDB-capable EAM_TEST_DATABASE_URL.
```

Progress: P0/P1 complete; P2 read API implemented; P3 durable mutations, P4 writes/actions, P5 resumable
ingress, P6 client cutover and P7 release/performance acceptance remain. Live COS capability proofs,
production baselines, old-grant draining and actual deployment verification are still separate gates.
