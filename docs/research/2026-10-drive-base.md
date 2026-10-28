# Personal data platform: drive base selection research

> Researched: 2026-10 | Goal: use a drive as the base, keep layering custom features on it long term, and carry "all of my personal data"

## 0. One-line conclusion

There is no "most capable" drive, only two shapes of base:

- **A product-shaped base** (Cloudreve / Nextcloud / Seafile): take an off-the-shelf drive as the file layer and bolt extra features onto it.
- **A thin self-built base** (S3 + Postgres + your own API): total freedom, but a lot of work.

For a personal project the default recommendation is **Cloudreve 4 as the file base + your own service as the business layer**;
if you want "cross-platform two-way sync + a full photos/calendar/contacts suite + plain files you can migrate away with", choose **Nextcloud**.

---

## 1. "A drive as the base" actually means choosing three layers

| Layer | Responsibility | Options |
|---|---|---|
| Storage layer | Where the bytes live | cloud OSS/S3, a local disk, SeaweedFS / Garage / RustFS |
| File layer | Directory, versions, sharing, permissions, sync, search | Cloudreve / Nextcloud / Seafile / Pydio |
| Business layer | Your "extra features" | write it yourself (a separate service is recommended; don't cram it into the drive) |

**The trap that is easiest to fall into**: stuffing business logic into the drive. The drive handles "files + permissions + sharing"; structured data (ledger, health, places, contacts, timeline) belongs in your own Postgres, correlated through file URIs.

---

## 2. Evaluation dimensions (redefined for "base + long-term extension")

An ordinary drive review compares "is it pleasant to use"; a base compares:

1. **Extension points**: is the API complete, are there events, can you replace the frontend, can you write plugins
2. **Metadata and search**: everything later depends on "attach attributes to files + query attributes"
3. **On-disk shape**: plain files (rsync-able, you can walk away any time) or blobs (meaning depends on the DB)
4. **Client coverage**: Web / desktop sync / mobile / WebDAV
5. **Operational cost**: how many processes, how many middlewares, how often it upgrades
6. **Licensing and commercial risk**: Pro paywalls, is the company stable, the frontend source license

---

## 3. Candidate comparison (measured data, 2026-10)

| Project | Version | Stack | On-disk | Extension | Metadata/search | Clients | License | Ops |
|---|---|---|---|---|---|---|---|---|
| **Cloudreve** | 4.19.1 | Go + React | blob (S3/local/OneDrive/remote nodes), original filenames not stored | REST `/api/v4` + SSE events + WebDAV + WOPI + replaceable frontend | metadata namespace `key:value`, directly usable as a query condition; Meilisearch full text + Tika body extraction + AI semantic search | Web, iOS, Windows (Pro), WebDAV | GPL-3.0 + paid Pro | ★ single binary + DB (+ optional Redis/Meilisearch) |
| **Nextcloud** | 35.0.2 | PHP + Vue | **plain files** under `data/<user>/files` | PHP App + **AppAPI ExApp (backend in any language, containerized microservices)** | tags/comments/versions; full-text search via fulltextsearch + the ES plugin | official clients on every platform (Win/mac/Linux/iOS/Android) + WebDAV | AGPL | ★★★ PHP-FPM + nginx + DB + Redis + cron |
| **Seafile** | 13.0 (14 in testing) | C + Go | block storage (library/commit model), encrypted libraries | REST API + WebDAV; weak plugin ecosystem | "file attributes + views" since 13; full-text search is mostly Pro | all platforms, **the strongest sync** (block-level delta, resumable transfer, conflict copies) | AGPL / dual commercial | ★★ needs seafile + seahub + DB + cache |
| **Pydio Cells** | 5.1.0 | Go microservices + gRPC | local/S3 abstraction | REST / gRPC / multi-language SDKs | yes | Web + a sync client | AGPL | ★★★ many microservice processes, enterprise-leaning |
| **FileBrowser Quantum** | active | Go, single binary | maps a directory directly (plain files) | essentially no extension points | indexed search | Web | Apache-2.0 | ★ extremely light |
| **OpenList** (AList's successor) | active | Go | virtual FS aggregating 30+ drives | API / WebDAV | inherits the drive's metadata | Web + WebDAV | AGPL | ★★ |

Community popularity for reference: Nextcloud ~37.2k★, SeaweedFS ~35.7k★, Cloudreve ~28.9k★, OpenList ~25.2k★, Seafile ~15.4k★, FileBrowser Quantum ~8.9k★, Pydio Cells ~2.2k★.

---

## 4. One by one

### Cloudreve 4 — the "API-first" modern drive, the best fit for building your own layer on top

**Strengths**
- Clean data model: `File` / `File Blob` (immutable) / `Metadata` / `Storage Policy`; files have versions, thumbnails, Live Photo;
- **Metadata is a query condition**: `meta_music:album=Deluxe`, `exact_meta_*`, plus custom `props:<key>` — effectively a free "attach structured attributes to files" capability;
- Full-text search (Meilisearch + Apache Tika extracting PDF/Office bodies) + **AI semantic search** (embeddings sent to OpenAI/HF);
- File Apps: built-in PDF/ePub/video/music/Markdown, plus WOPI (OnlyOffice) and embedded web apps;
- SSE file-change events, OIDC login, slave nodes, a matrix of storage policies (S3/OSS/COS/OneDrive/R2/B2/local);
- Deployment is one binary + one database, an order of magnitude less ops work than Nextcloud;
- The frontend is a separate React repo and can be swapped out outright (Pro users get the frontend source).

**Hard problems**
- Files in the bucket are **blobs (hashed paths), not original filenames**; without Cloudreve you need the DB to reconstruct the directory tree → a backup must snapshot the DB and the bucket at the same point in time;
- The official desktop sync client is **Windows only, and needs Pro**; on Mac/Linux you have WebDAV or rclone;
- No backend plugin system; extensions can only "go through the API";
- The community-edition frontend repo has no LICENSE file; confirm the licensing before a deep UI rewrite;
- Free switching between storage policies, per-directory WebDAV policies, and the like sit behind Pro.

### Nextcloud 35 — full suite + plain files + clients everywhere, but heavy

**Strengths**
- **The files are just plain files** under `data/<user>/files/`; rsync/restic can back them up directly, and you can walk away any time — for "personal data kept for decades" this is the biggest advantage;
- The largest ecosystem: Photos, Calendar, Contacts, Mail, Talk, Office, Notes, Deck… a lot of "extra features" install straight away;
- Extensibility is actually fine: PHP Apps, plus **AppAPI ExApp** — the backend can be a Python/Go/Node container microservice and the frontend Vue, hung into the Nextcloud menu;
- No gaps in client coverage (Win/mac/Linux/iOS/Android), and the WebDAV implementation is mature.

**Hard problems**
- Heavy: PHP-FPM + nginx + DB + Redis + cron, frequent upgrades, and the occasional trap on major upgrades;
- With a lot of files (tens of thousands in one directory) listing/scanning gets slow; the DB filecache is the bottleneck;
- Changing the core means changing PHP, a big mental load; AppAPI needs a Docker environment.

### Seafile 13/14 — the sync king, the weakest extensibility

**Strengths**
- Block-level delta sync, resumable transfer, conflict copies, encrypted libraries; multi-device sync is the best of this bunch;
- Since 13 it has "file attributes + flexible views", a step toward "files as a database";
- Mature desktop/mobile clients.

**Hard problems**
- Data is block storage + a commit model, **the most closed**; leaving Seafile requires dedicated export tooling;
- Extension points are REST/WebDAV with no plugin system, so new features basically have to be bolted on;
- A clear gap between the community edition and Pro (full-text search, online Office, advanced permissions are Pro).

### Pydio Cells 5 — modern architecture, small community

Go microservices + gRPC, Cells Flows automation, complete SDKs; but multi-process operations are complex and the community is small, so the value for a personal project is poor.

### FileBrowser Quantum — for "good enough is fine"

A single binary, mapping a real directory, indexed search; it runs in minutes. No versions, no decent sharing-permission system; it is only a lightweight file browser, not a base.

### OpenList — not a base, an "entry point"

AList was forked by the community into OpenList after a commercial dispute (the original is no longer trustworthy). It mounts 30+ drives as a unified virtual file system + WebDAV, which is a great fit for "aggregating external clouds", but **it has no versions, no consistency guarantees, and is not primary storage**. Its role: a second entry point running alongside, not the foundation.

---

## 5. Three recommended architectures

### A. Cloudreve at the core (recommended: you can write code, you want an API, you want light ops)

```
Web / iOS / WebDAV
        │
   Cloudreve 4  ── files · sharing · permissions · versions · metadata · full-text/semantic search
        │                    ▲ REST + SSE
   Postgres / S3 (or local)  │
                        your business service  ── its own Postgres + pgvector
```

- The business service does not touch Cloudreve's source: it lists directories, gets temporary direct links, writes `props:*` metadata, and subscribes to SSE for automatic processing;
- Extra features (albums, timeline, AI Q&A, auto-archiving) are all separate processes; if they break, the drive is unaffected.

### B. Nextcloud at the core (recommended: you want the full suite + multi-device sync + the ability to walk away)

```
Win/mac/Linux/iOS/Android official clients
        │
   Nextcloud 35  ── Files/Photos/Calendar/Contacts + AppAPI ExApp
        │
   data/<user>/files  ←── WebDAV / OCS API ── your business service
```

- Files stay plain files; ten years from now they are still readable without Nextcloud;
- Extra features become ExApps (any language) or a separate service reading WebDAV.

### C. A thin self-built base (the long-term play, the most work)

```
your frontend → your API → Postgres (metadata/ACL) + S3 (RustFS/cloud OSS)
                              ↑ optional: FileBrowser as an ops backdoor UI (read-only mount)
```

- Only you define the data model, and mobility is maximal;
- Note: **one bucket can only have one writer**. Cloudreve/Nextcloud blob naming and your own naming must not be mixed, or they will collide.

---

## 6. Pitfall checklist

- **MinIO's GitHub repo is archived (2026-04)**; the community edition is effectively unmaintained, so don't pick it for a new project. For the S3 layer use cloud OSS, SeaweedFS, or Garage; RustFS is very new (be careful).
- **The original AList is no longer trustworthy** (commercial dispute); use the community fork OpenList.
- **ownCloud has been in turmoil at the company level for the last two years** (the business was acquired/restructured); oCIS is not recommended as the base for a new project.
- **Backups of blob-style drives (Cloudreve/Seafile) must snapshot the DB and the bucket at the same point in time**, or what you restore is a pile of unexplainable hashes.
- **Self-hosting ≠ backup**: at least 3-2-1, pushing to another object store with restic; never let the self-hosted drive be the only copy.
- **Don't stuff everything into one Seafile library**: operations slow down noticeably once a library grows; split libraries by topic.
- **Cloudreve's desktop sync is Windows + Pro only**; if you live on Mac/iPhone and want "syncing like Dropbox", that is a veto.

---

## 7. A one-week validation plan (more reliable than reading reviews)

| Day | Action |
|---|---|
| D1 | bring up `postgres + meilisearch + cloudreve` with docker compose and get upload/share working |
| D2 | load 50k files (a script generates the directory tree); test listing, search, thumbnails, resumable large-file upload |
| D3 | iPhone client + rclone WebDAV mount + external link sharing; write down where the experience breaks |
| D4 | start a Nextcloud AIO instance on the same machine on another port; repeat D2/D3 for comparison |
| D5 | write a 200-line script against the Cloudreve API: list a directory → get a direct link → write `props:*` → subscribe to SSE |
| D6 | backup drill: `pg_dump` + a bucket snapshot → restore into a fresh container; verify files and the directory tree are intact |
| D7 | decide: score on "sync experience / ease of extending / ops pain" |

---

## 8. References

- Cloudreve docs (concepts / File URI / Metadata / File Apps / semantic search): https://docs.cloudreve.org/
- Cloudreve source: https://github.com/cloudreve/Cloudreve
- Nextcloud ExApp development: https://docs.nextcloud.com/server/32/developer_manual/exapp_development/development_overview/ExAppOverview.html
- Nextcloud Server: https://github.com/nextcloud/server
- Seafile 14 preview: https://blog.seafile.com/seafile-14-0-is-ready-for-testing-620a335e5668
- Seafile 13: https://blog.seafile.com/seafile-13-0-is-ready-470c5e75f6fc
- Pydio Cells: https://github.com/pydio/cells
- OpenList: https://github.com/OpenListTeam/OpenList
- FileBrowser Quantum: https://github.com/gtsteffaniak/filebrowser
- MinIO (archived): https://github.com/minio/minio
- RustFS: https://github.com/rustfs/rustfs
- SeaweedFS: https://github.com/seaweedfs/seaweedfs
