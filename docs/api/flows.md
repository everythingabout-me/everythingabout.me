# Are WebDAV / S3 actually enough? A walkthrough of real scenarios

> Status: historical analysis draft | the current API reference is [api.md](api.md)
>
> **Updated decision, 2026-10-30:** [storage and the unified file API](../design/storage.md) defines two layers,
> pluggable storage, and one public origin for both control requests and file bytes. The earlier argument
> against proxying bytes and the inference that presigned direct upload is mandatory are superseded.
>
> We retain our own public API rather than a WebDAV or S3 facade. WebDAV and S3 remain planned storage
> integration targets; this is separate from exposing either protocol to downstream applications.
>
> This document is a historical record of scenario analysis. Its conclusions and examples are not the current
> architecture specification; the inferences in §5 (nodeId, kind, change feed) and plans in §6–§9 were not adopted.

## 0. The conclusion up front

1. **WebDAV is enough, and for "AI-native + loosely organized data" it is the most suitable door.**
   It natively covers read/write, listing, moving, locking, and quota; what it lacks is not "read/write" but
   **discovery, identity, and lifecycle** — and all of those can be solved with a thin layer on top.

2. **S3 is enough, but it points the wrong way.** It assumes "you already know which key you want".
   Bulk movement, backup, and a mature SDK are strong; but the model where "the user reorganizes directories as
   they please and apps adapt" cannot be expressed in S3 — no directories, no rename, no queryable metadata, and
   change only through event notifications.

3. **There are only five places where neither is enough**, and none of them should be solved by the protocol
   layer:

   | Gap | Why the standard cannot solve it | What we add |
   |---|---|---|
   | Search | DASL (RFC 5323) is effectively unimplemented | `/v1/search` |
   | Change feed | RFC 6578 exists, but server/client support is nearly nonexistent | implement REPORT + `/v1/changes` |
   | Stable identity | Both the standards and S3 use path/key as identity | `nodeId` |
   | Lifecycle | Trash, versions, and sharing do not exist in S3; DeltaV is dead | `/v1/trash`, `/v1/versions`, `/v1/shares` |
   | Human-facing authorization | WebDAV only has Basic+ACL, S3 only SigV4 | token + scope + prefix |

4. **So it is not "three protocols side by side"; it is: WebDAV is the door, JSON is the patch, and S3 is only
   for bulk movement.**

---

## 1. How to judge "enough" and "convenient"

"Enough" = can this flow be expressed at all.
"Convenient" = how much client code it takes, how many requests one flow costs, how many edge cases you have to
handle.

The two often diverge: S3 is expressive but signing is painful; WebDAV is less expressive but the ecosystem
already exists. So each item below is rated separately.

---

## 2. The big table

| # | Scenario | Operations needed | WebDAV | S3 | Verdict |
|---|---|---|---|---|---|
| 1 | Connect and get credentials | auth + probe capabilities | `OPTIONS` + Basic | SigV4 + keys | neither is human-facing |
| 2 | First discovery | list the whole tree | `PROPFIND` level by level (N requests) | `ListObjectsV2` flat (N/1000 requests) | WebDAV poor, S3 fair |
| 3 | Incremental discovery | only what is new | `REPORT sync-collection` | none (needs event notifications) | neither works well |
| 4 | Semantic lookup | "badminton videos" | none | none | **not enough** |
| 5 | Read a whole file | download | `GET` | `GetObject` | enough |
| 6 | Read a byte range | Range | `GET` + `Range` | same | enough |
| 7 | Read metadata | size/mtime/etag | `HEAD` / `PROPFIND` | `HeadObject` | enough |
| 8 | Write a small file | upload | `PUT` | `PutObject` | enough |
| 9 | Write a large file | resumable upload | no standard | `MultipartUpload` | WebDAV is not enough |
| 10 | Overwrite update | conditional write | `If-Match` | conditional Put | enough |
| 11 | Concurrency protection | locking | `LOCK/UNLOCK` | none | WebDAV is enough |
| 12 | User reorganizes directories | move/rename | `MOVE` | none (copy+delete) | WebDAV yes, S3 no |
| 13 | Delete and restore | trash | `DELETE` (hard delete) | `DeleteObject` | neither is enough |
| 14 | Versions / recover an old draft | versioning | DeltaV (dead) | `Versioning` | S3 yes, WebDAV no |
| 15 | Share with someone else | temporary link | none | presigned URL | neither is human-facing |
| 16 | Many apps reading | read across directories | read freely once mounted | prefix listing | both enough, but authorization is hard |
| 17 | Backup / exit | move the whole drive out | `rsync` once mounted | `rclone sync` | both enough |
| 18 | An app in the browser | CORS + upload | barely usable | signing in the frontend | **not enough** |
| 19 | An AI agent browsing | traverse + read + write | just mount it as a directory | needs a wrapper of its own | WebDAV works surprisingly well |
| 20 | Audit "who wrote this" | attribution | none | none | **not enough** |

---

## 3. Walking through each case

### 3.1 Connecting

An app first needs to know "what can I do". WebDAV hides the answer in `OPTIONS`:

```http
OPTIONS /dav/
→ DAV: 1, 2
  Allow: OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK, REPORT
```

S3 has no "capability negotiation", only the docs and poking at `ListBuckets`/`GetBucketLocation`.
Our `/v1/me` returns a `capabilities` array directly, which is clearer than either.

**Convenience**: WebDAV's Basic auth is unfriendly to scripts (you have to deal with the 401 challenge first) and
friendly to finished clients (the libraries already wrap it). That is why a separate door for scripts has to
exist.

### 3.2 First discovery: scanning a whole drive

Say a user has 50,000 files and 3,000 directories.

- **WebDAV**: `PROPFIND Depth: 1` gets one level, so recursion costs 3000 requests.
  `Depth: infinity` could in theory get everything at once, but RFC 4918 explicitly allows the server to refuse
  (`403 propfind-finite-depth`), and in practice nobody dares turn it on — it returns tens of MB of XML in one go.
- **S3**: `ListObjectsV2?prefix=&max-keys=1000`, done in 50 pages of flat listing, with no per-directory
  recursion. **S3 is clearly stronger here.**
- **Ours**: `/v1/list?recursive=1&prefix=Videos/` queries Postgres directly, 1000 rows a page, keyset paginated,
  flat just like S3, but with no request fees and no dependence on the bucket.

**Conclusion**: this actually mixes three comparisons; let's separate them first:

1. **WebDAV vs S3** (both are "protocol-level listing"): S3 wins. It lists flat, done in 50 pages;
   WebDAV can only `PROPFIND` level by level, so 3000 directories means 3000 requests.
2. **Protocol listing vs querying an index** (we "have a database"): the index wins by a mile. One SQL query,
   1000 rows a page, and you can filter by name along the way. So "neither is ideal" means: **compared with the
   index, both are slow.**
3. **Should we build something special for it in the end**: no. It is not that we "cannot scan", it is that we
   "scan a little slower" — for a real folder, that is simply the speed. So no `recursive=1`, no cursors, no
   change feed; level-by-level `PROPFIND` is the answer. People who need speed should split their directories
   smaller, or just search.

> The lesson here: conflating "can be done" with "worth building specially" grows an API nobody uses.
> The final version ([api.md](api.md)) has no interface just for scanning: `GET /list` lists a directory, and
> you page through it.

### 3.3 Incremental discovery: only handle what is new

This is what a "data-analysis app" (badminton analysis, ledger import, album cleanup) has to do every day.

- **WebDAV**: the standard really has it — RFC 6578 `REPORT sync-collection`:

  ```http
  REPORT /dav/Videos/
  <D:sync-collection xmlns:D="DAV:">
    <D:sync-token>opaquelocktoken:...</D:sync-token>
    <D:sync-level>1</D:sync-level>
    <D:prop><D:getetag/><D:getcontentlength/></D:prop>
  </D:sync-collection>
  ```

  The server answers `207 Multi-Status` plus a new token. **The semantics are exactly right, but in practice
  very few servers or clients implement it.** Implementing it ourselves is a bonus (a handful of clients such as
  rclone use it), but you cannot count on apps knowing it.
- **S3**: no API. Either poll `ListObjectsV2` and diff yourself, or wire up Bucket Notification to push into
  SQS/SCF — which is unaffordable for a personal app.

**Conclusion**: incremental discovery has to come from us, and it has to come in both shapes:
the standard shape (`REPORT sync-collection`) and the simple shape (`GET /v1/changes?since=`).
It is missing from both WebDAV and S3, and it is the single most essential capability for "aggregating data".

### 3.4 Semantic lookup: "find every badminton video"

WebDAV's search standard is DASL (RFC 5323), which almost nobody implements; S3 has none at all (only prefixes).

Users organize their data **however they feel like it** — it might be in `/运动/羽毛球/2026/`, or dumped straight
at `/手机相册/VID_20261024.mov`. The filename might be `VID_20261024.mov`.

So "finding" cannot rely on paths; it can only rely on:

1. **Fuzzy filename/path search** (pg_trgm is already there);
2. **Tags** (applied by the user or by an app);
3. **Content** (later: thumbnails, covers, body extraction, embeddings).

`/v1/search?q=羽毛球&mime=video/&tag=sport` is the only entrance to this layer.
**Conclusion: search is the first entrance in an AI-native scenario, not a nice-to-have.**
AI does not traverse a TB of directories; it searches first, then reads.

### 3.5 Reading

```http
GET /dav/Videos/羽毛球/a.mov
Range: bytes=0-10485759
→ 206 Partial Content
```

Both WebDAV and S3 support Range natively. **Enough, and it is the most solid part of either protocol.**

The one thing to watch: the WebDAV door must **proxy bytes** (clients connect to our domain with Basic auth) and
cannot 302 to COS — otherwise the interaction between the Range header and presigned signatures goes wrong
easily. So the storage layer needs a `GetRange`, not just a `GetURL`.

### 3.6 Writing small files / writing large files

Small files (< 32 MiB): `PUT` / `PutObject`, both enough.

Large files are the watershed:

- WebDAV **has no standard resumable upload**. A broken `PUT` starts over. A 2 GB editing export fails often
  enough on a home network, and Finder retries the entire file.
- S3's `MultipartUpload` is designed for exactly this, mature and stable.

**Conclusion**: the WebDAV door suits "everyday read/write once mounted" and does not suit "uploading a 20 GB
video". Large files go through `/v1/uploads` (presigned multipart), which is the only place S3 is clearly better
than WebDAV. When a user loads terabytes of data, they should be putting it straight into the bucket with
`coscli`/`rclone` plus `reconcile` anyway.

### 3.7 Users reorganizing directories: this one decides the whole architecture

Today a user keeps videos in `/Videos/羽毛球/`; next week they tidy it into `/运动/羽毛球/2026/`.

**WebDAV**:

```http
MOVE /dav/Videos/羽毛球/a.mov
Destination: /dav/运动/羽毛球/2026/a.mov
```

One call, invisible to the client. The server has to rewrite the index and copy the COS object from the old key
to the new key (because our key is the path). A single file is O(1); a directory of 1000 files is 1000 copies —
that is the real cost the WebDAV door faces, and it needs a policy (see §7).

**S3**: no MOVE. A client can only do `CopyObject` + `DeleteObject`.
To an app that "watches for changes", what it sees in the listing is **one key disappearing and another
appearing**, and it cannot tell whether that was a "move" or "deleted and uploaded a new one".

The consequences are serious: the app re-analyzes everything, generates a second set of results, and fills the
user's drive with duplicates.
**That is why S3 cannot be the door for an app's everyday reads and writes.**

**Where we beat both**: give every file a stable `nodeId` (`nodes.id` is already a UUID); the id survives `MOVE`,
and the change feed emits one entry:

```json
{"seq": 1288, "op": "move", "id": "nod_7f3a…", "from": "Videos/羽毛球/a.mov", "to": "运动/羽毛球/2026/a.mov"}
```

The app's index points at the id; when the path changes it updates the path and never recomputes.
**"The path belongs to the user; the identity is stable"** — that is the precondition for loosely organized data
to be reusable by several apps.

### 3.8 Delete, restore, versions

- WebDAV's `DELETE` is **permanent** by spec; so is S3's `DeleteObject` (unless versioning is on).
- We implement it as a soft delete, invisible to the client (still 204), but the data goes to the trash.
- Restoring, listing the trash, and cleaning up by age all need an extra interface: `/v1/trash`.
- Versions: S3's bucket versioning is available as-is; WebDAV's DeltaV is basically a paper standard.
  Our suggested implementation copies the old object to `_versions/<id>/<seq>` before overwriting and exposes it
  through `/v1/versions`.

**Conclusion**: neither of these should be left to the protocol; we must add them ourselves. And they are exactly
the core promise of "long-term value".

### 3.9 Sharing, audit, quota

- **Sharing**: S3's presigned URL is half a product (it lets someone else look, but you send the link yourself
  and manage expiry yourself); WebDAV has no human-facing sharing at all. `/v1/shares` fills the gap.
- **Audit**: "which app wrote this file" — neither has it. Write `audit_log`.
  It is also the only reason a user dares hand a token to a third party.
- **Quota**: WebDAV has RFC 4331 (`quota-available-bytes` / `quota-used-bytes`) and Finder shows "how much space
  is left"; S3 has nothing. **WebDAV wins this one, and it is worth implementing.**

### 3.10 Backup and exit

```bash
rclone sync eam:/ ~/backup            # WebDAV
rclone sync cos:bucket/drive/<uid>/ ~/backup   # S3 direct
```

Both work. The precondition is: **the key is the path** (not a hashed blob) and `getlastmodified` returns the
real time. We already satisfy both architecturally, and that is the confidence behind "we can walk out at any
time".

### 3.11 An app in the browser

WebDAV is basically unusable in a browser (custom methods go through CORS preflight, and Basic auth cannot be
handled gracefully). S3 needs the frontend to do SigV4 signing, which amounts to shipping the key to the
browser — unacceptable (presigning works, but it needs backend cooperation).

**There is only one way**: `/v1` + Bearer + presigned direct upload. This is the first hard reason the JSON door
exists.

---

## 4. Capability gap matrix

| Capability | Standard | Real-world support | How we fill it |
|---|---|---|---|
| List a directory | RFC 4918 PROPFIND | good | use it as is |
| Flat recursive listing | none | — | `/v1/list?recursive=1` |
| Read/write / Range | RFC 4918 / HTTP | good | use it as is |
| Move / copy | RFC 4918 MOVE/COPY | good | use it as is |
| Locking | RFC 4918 LOCK | good on the client, the server has to implement it | Postgres lock table + TTL |
| Quota | RFC 4331 | good on the client | use it as is |
| Change feed | RFC 6578 sync-collection | **rare on both ends** | implement REPORT + `/v1/changes` |
| Search | RFC 5323 DASL | **basically dead** | `/v1/search` |
| Versions | RFC 3253 DeltaV | **basically dead** | `/v1/versions` |
| Trash | none | — | `DELETE` soft delete + `/v1/trash` |
| Resumable large uploads | none | — | `/v1/uploads` multipart |
| Sharing | none | — | `/v1/shares` |
| Structured metadata | PROPPATCH dead props | **poor client support** | `PATCH /v1/fs` + project tags into dead props |
| Human-facing authorization | RFC 3744 ACL | complex, rarely used | token + scope + prefix |
| Audit | none | — | `audit_log` + UI |
| Stable identity | none | — | `nodeId` (already a UUID) |

---

## 5. Inferences for AI-native

I completely agree with that line of reasoning, and it **rewrites the interface design in reverse**. Five
specifics:

### 5.1 No schema, but discoverability

We do not dictate an app's data format — but "loose" does not mean "unreadable". The protocol layer guarantees
three things:

1. paths are **human-readable** (not `a1b2c3.json`);
2. files are **self-describing** (`manifest.json`, top-level `_type`/`_schema` in JSON, Markdown front-matter);
3. metadata is **queryable** (tags, mime, time).

With those three in place, an AI-capable app can understand unfamiliar data on its own.
Conversely, if paths are hashes and content is raw binary blobs, even the smartest app cannot read it.

> This directly supports "no format adaptation": we do not understand data on the app's behalf, but we make data
> **understandable**.

### 5.2 Stable identity, mutable paths

Users reorganize directories while apps maintain their own indexes. The only reliable anchor is `nodeId`.
Without it, "the user tidied the directories once" equals "every app's cache is invalidated".

### 5.3 Search before traversal

An AI app works like this: `search` → `read` → reason → `write`.
It does not run `find . -name` across 50,000 files. So `/v1/search` has to matter as much as `GET`, and tag and
MIME filters get used more often than path prefixes.

### 5.4 Tell "original" and "derived" apart

This is the layer that you and most drives have not thought through. Apps write a lot of **intermediate output**
into the drive: thumbnails, cut segments, index files, analysis JSON.

Treat those the same as the user's original videos and in a few years the drive is unusable.
The suggestion is to tag a `kind` on write:

| kind | Meaning | When the user cleans up |
|---|---|---|
| `primary` | the user's original data, the only copy | **never auto-deleted** |
| `derived` | computed from originals, rebuildable | one-click cleanup |
| `cache` | entirely disposable cache | clear whenever |

```http
PUT /v1/fs/Apps/badminton/analyses/nod_7f3a.json
X-EAM-Kind: derived
```

This costs almost nothing, but it keeps "several apps sharing one drive" clean years later.

### 5.5 Intermediate results stay on the app's own side

Agreed, and the protocol should state it as a **write guideline** rather than a technical constraint:

> Only things "the user will want to look at again, or will still want ten years from now" get written into EAM.
> Indexes, caches, temporary slices, model weights — those go in the app's own storage.

Technically that pairs with three things: quota (it says something when the drive fills up), audit (you can see
which app writes the most), and `kind` (derived data can be cleaned up in bulk). That is enough; no complex
policy engine needed.

### 5.6 One bonus opportunity: a tool surface for agents

If the app itself is an AI agent, the most natural interface is not REST docs but four tools:
`ls` / `read` / `search` / `write`. A few dozen lines of MCP server lets any MCP-capable agent use this drive as
its working directory. That is far cheaper than making every app implement its own client.

---

## 6. So should we do S3 at all?

My suggestion: **not in v1; leave a clear "might do later" slot.**

Reasons:

- Of S3's three strongest scenarios (bulk movement, backup, a mature SDK), `rclone` + WebDAV already covers the
  first two, and the third ("I want to use boto3") is rare in AI-native scenarios;
- S3 compatibility means either relying on COS's S3 endpoint (the index lags, you need `reconcile`) or
  implementing SigV4 plus a subset yourself (1–2 weeks of work); the payoff does not match the cost;
- The moment S3 is truly needed is "the user wants the existing backup tools / object storage ecosystem"; when
  that day comes, wiring up COS's S3 endpoint plus prefix-scoped STS credentials is a one-day job.

**Positioning**: S3 is an "ops / bulk movement channel", not an "app onboarding channel".

---

## 7. WebDAV's traps (the real answer to "convenient")

These are the details that actually decide "can it be used", not theoretical problems:

| Trap | Symptom | Handling |
|---|---|---|
| `PROPFIND Depth: infinity` | someone pulls the whole tree at once and the server blows up | return `403 propfind-finite-depth` |
| A directory `MOVE` is an O(n) copy | 1000 files means 1000 server-side copies | do it synchronously + warn at a ceiling; above the threshold return 202 + a background job (build the synchronous version first) |
| macOS sends NFD filenames | store `é` and it cannot be found again | normalize to NFC at the entrance (there is already `paths.Normalize`) |
| Finder writes `.DS_Store` / `._*` | the drive fills with junk files | accept but mark them hidden, and filter them out of listings by default |
| Case sensitivity | a macOS client can create both `A.jpg` and `a.jpg` | case-sensitive by default; with `STRICT_CASE=off`, reject case conflicts |
| ETag semantics | the ETag of a COS multipart upload is not an MD5 | treat it as an opaque string, never promise MD5 |
| Windows Explorer | refuses http + Basic by default | document that HTTPS is required; the `WebClient` service must be running |
| Opening a document in Office | without LOCK it opens read-only | implement LOCK/UNLOCK (a Postgres table + TTL) |
| A large PUT times out | a reverse proxy or client 30s timeout | raise the proxy timeout; large files go through `/v1/uploads` |
| Trailing slash | is `GET /dav/a` a file or a directory | directories must carry `/`, and a collection's href always ends with `/` |
| Auth failure format | the client shows no password prompt | return `401` + `WWW-Authenticate: Basic realm="eam"` |

**How to verify**: run the four `litmus` suites (webdav.org's official conformance test) — `basic` / `copymove` /
`props` / `locks` — then walk through it by hand with `cadaver`, and finally mount it once each with Finder and
rclone. If those three steps do not pass, do not claim "WebDAV support".

---

## 8. Revisions to the v1 protocol

Based on the walkthrough above, the original design needs seven changes:

1. **Promote WebDAV to the first door**, and reposition `/v1` as "the part WebDAV cannot cover + a convenient
   channel for scripts".
2. **`nodeId` becomes a first-class citizen**: `/v1/list`, `/v1/search`, and `/v1/changes` all return `id`;
   the id survives `MOVE`/rename; an app's index should key on the id.
3. **The change feed gains a `move` event** (`from`/`to`/the same `id`), and we implement RFC 6578
   `sync-collection`.
4. **Add `kind` (primary / derived / cache)**, settable on write, used for cleanup and quota accounting.
5. **`/v1/list` supports `recursive=1`**, flat + keyset pagination, covering WebDAV's weak spot.
6. **Separate read and write scopes on tokens**: reads may be `read:*` (the user explicitly grants the whole
   drive), writes are always confined to `write:<prefix>`.
7. **Large writes go only through `/v1/uploads`**; WebDAV's `PUT` gets a cap (say 2 GiB) and returns 413 with a
   hint above it.

---

## 9. Revised implementation order (the Go version)

| Phase | What | Estimate | Output |
|---|---|---|---|
| P1 | token + `/v1` core (fs/list/search/me) + error body | 2–3 days | scripts work |
| P2 | WebDAV (`x/net/webdav` + a custom FileSystem) + LOCK table + quota props | 3–4 days | Finder can mount it |
| P3 | expose `nodeId`, change feed, RFC 6578, versions, trash | 2–3 days | incremental sync across apps |
| P4 | `/v1/uploads` (multipart), `/v1/shares`, audit UI | 2–3 days | large files and sharing |
| P5 | MCP server + CLI + the `manifest.json` convention | as needed | agents can use it directly |

The Go advantage is that P2 turns from "do we need another service" into "add one more handler to the same
binary"; `/dav`, `/v1`, and `/api` share the same storage / nodes / auth, with no cross-process consistency to
handle.

> Note: P1–P5 above is the order for the "heavy plan", and **has been superseded by the slimmed-down version in
> [api.md](api.md)**.

---

## 10. Benchmark: are Google Drive's and Dropbox's APIs enough

### What they look like

**Google Drive v3** — a "file database" with a query language:

- Every object is a `files/*` with `id`, `name`, `mimeType`, `parents[]`, `size`, `md5Checksum`, `modifiedTime`,
  `trashed`. A folder is itself a file (with a special mimeType).
- **It is not a path tree**: files are referenced by `id` and located by `parents`. Hence the fileId —
  identity and location are two different things.
- Listing and finding both go through `files.list`, filtered with a `q` query language:
  `q=name contains '羽毛球' and mimeType contains 'video/' and trashed=false`.
- Incremental sync uses `changes.getStartPageToken` + `changes.list` (pageToken).
- Large files: `resumable upload`; versions: `revisions`; permissions: `permissions`; sharing:
  `permissions.create`.
- Auth is OAuth 2.0 + scope (the narrowest tier is `drive.file`: only files the app itself created).

**Dropbox v2** — a path tree + a cursor:

- Paths are `/folder/file.txt` (case-insensitive); there is an `id` too, but day to day you use paths.
- Listing with `files/list_folder` returns a `cursor`; `files/list_folder/continue` gets the next page —
  **incremental is just "keep listing with the old cursor"**, with no separate event-feed concept.
- Large files: `upload_session` (start/append/finish).
- It has `content_hash` (not MD5), `revisions`, and shared links.
- Auth is OAuth 2.0 + scope as well.

### Enough for our goal?

**Enough for their own ecosystems; not enough for "let existing apps store data in the user's own drive".**
Three reasons:

1. **A proprietary protocol with no reuse value.**
   An app that can write to Drive does not thereby write to us. What we want is "an existing app fills in an
   address and works" — Finder, rclone, Obsidian, Zotero, and KeePassXC all already ship **WebDAV** clients and
   will not write a Drive client for us. That is the most concrete reason to choose WebDAV.
2. **More concepts, landing right on the "driver cost" you want to avoid.**
   Drive needs at least fileId, parents, `q` syntax, pageToken, resumable, and scope;
   Dropbox needs at least path, cursor, content_hash, upload_session, and scope.
   WebDAV only asks you to understand "files" and "HTTP verbs" — the training data for those two goes back
   decades.
3. **No mounting.** Both need the official client or a third-party adapter to become a "drive".
   And mounting is decisive for an AI app: once mounted it can use `ls`/`cat`/`grep`, and no tool layer has to be
   written for our interface.

| | Drive | Dropbox | WebDAV |
|---|---|---|---|
| Positioning | file database + query language | path tree + cursor | path tree + HTTP verbs |
| Must-know concepts | 6–8 | 4–5 | 2 |
| Mountable | needs an adapter | needs the official client | **native** |
| Client reuse | none | none | Finder / rclone / a pile of apps |

### Two things worth copying (neither adds a concept)

- **Dropbox's `cursor`**: if we ever do build incremental sync, "keep listing with the old cursor" is
  incremental, with no "event feed" introduced. It is the only form of incremental that adds zero concepts.
- **Drive's `drive.file` scope**: the app only sees files it created.
  For us that becomes "a token confined to `/Apps/<app>/`", and the user does not have to understand anything
  new.

In one line: **Drive/Dropbox are "wire up an SDK to their cloud"; WebDAV is "here is a folder".**
We want the latter.
