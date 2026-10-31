# P1: native trees, configuration and safe index import

P1 implements the read side of the [driver contract](storage-driver.md) and the backend/configuration
phase of the [implementation plan](storage-implementation-plan.md). Local, COS and generic S3 provide
`Stat`, immediate-child paginated `List` and streaming `Open` with explicit byte ranges. Their native
trees retain exact accepted names, folders and ordinary bytes. These interfaces can be used without
Postgres; accounts, search, annotations and events still require application metadata.

The replacement `/api/files` HTTP contract remains P2 work. This phase does not enable new production
writes, staged publication or resumable ingress. COS keeps its existing runtime routes until cutover;
Local/S3 run with index/account reads, internal native reads and maintenance import. Their legacy file
byte/mutation/grant/job routes return 501 before changing the index, legacy capabilities are unavailable,
list/search report `writable:false`, and legacy job recovery/timeline organizer are not started.
No new provider URL, credential response or browser storage SDK is added.

## Native roots and factories

`storagebackend.New` selects one connection at startup and shares its transport/factory across owners.
`Backend.Open(ctx, ownerID)` returns a closeable native tree. Every binding carries owner, connection,
configuration generation, content root and separate system root. Connection identity includes the actual
backend endpoint/bucket/root/name settings; credentials are excluded, so rotation does not change it.
Changing root configuration changes the identity. Persist/back up these bindings independently before
migration; this phase does not infer ownership from an arbitrary existing folder.

| Backend | Owner content root | Owner system root | Read/version policy |
|---|---|---|---|
| Local | `<LOCAL_CONTENT_ROOT>/<owner-UUID>/` | `<LOCAL_SYSTEM_ROOT>/<owner-UUID>/` | Native section reads; requested version unsupported; no immutable-byte claim from an open FD |
| COS | Existing `drive/<owner-UUID>/` keys | `__eam_system__/<owner-UUID>/` prefix | Native ranges; non-null native version IDs may select exact retained versions |
| S3 | `<S3_CONTENT_PREFIX><owner-UUID>/` keys | `<S3_SYSTEM_PREFIX><owner-UUID>/` | Explicit endpoint/region/path style/static or session credentials; native ranges/version IDs |

All native adapters advertise mutation capabilities false. Generic `PinnedRead` is conservatively false:
requested genuine version IDs are checked when available, but neither ETag nor size/mtime becomes an
application revision or unconditional strong-pinning promise. Streams validate native range status,
Content-Range and exact length; truncation fails instead of silently acknowledging a short read.
Callers must close streams and drivers; cancellation/driver close cancels their active reads. Factories
reuse bounded transport pools, not one HTTP pool per user. COS uses 64 maximum connections per host,
32 idle per host and 128 total idle connections, with bounded dial/TLS/header timeouts. No provider
redirect is followed by the new S3 client. Public HTTP Range/ETag behavior is not activated by this phase.

Local operator base directories must already exist, be absolute and be physically disjoint. Owner children
must also exist; read binding does not create directories. The multi-user factory opens owners relative
to held base handles, requires exact stored spelling and rejects symlink owner aliases. Per-component
no-follow opens/identity checks reject escaping or swapped symlinks, FIFOs and other special entries.
An already opened owner remains attached to its verified directory inode across rename. `os.Root` is
not a mount namespace: administrators must trust mount topology, hard links and root configuration.
The low-level `localstorage.Factory.Open` accepts explicitly trusted root paths; multi-owner startup
uses its stricter `OpenOwner` entry point.

Local name equivalence is conservatively case-folded by default; `LOCAL_CASE_SENSITIVE=true` asserts
actual root semantics. NFC is used for collision comparison only, never to rewrite existing names.
Local limits are 255 bytes per segment, 4096 relative path bytes and 256 levels. Object-store roots and
paths together must fit 1024 key bytes. Both preserve spaces and literal `+`, `#`, `?`, `%` and decomposed
Unicode where native names permit them; ambiguous file/folder or normalization collisions fail explicitly.

To detect collisions before returning a partial page, each adapter validates a complete immediate
directory, bounded to 100000 entries. Object adapters additionally cap native pages at 1000 per listing.
Oversized directories fail with `resource_limit`. This is a P1 operational limit, not an unbounded
terabyte-scale inventory implementation. Public pages default to 100 with a maximum of 1000. Opaque
cursors bind owner, root/configuration and parent, and are authenticated with a factory-lifetime key;
restart may invalidate them. Local cursor scope also includes held root identity. Listings are not snapshots
and pages may re-enumerate the directory. Use coordinated scans for migration evidence.

## Configuration and identity upgrade

Before starting an upgraded server, preserve the old save-link derivation seed explicitly:

1. Set `APP_SECRET` to the exact previous `COS_SECRET_KEY` used for signing. If the old deployment
   had no COS key, its fallback seed was the exact previous `DATABASE_URL`. Securely save this value
   before changing storage credentials or database URLs. Do not print it into logs or commit it.
2. Save-link derivation remains `SHA-256("eam/save-link/v1:" + APP_SECRET)`, so this explicit migration
   retains existing codes. New installs should generate a random application secret and back it up.
   Missing `APP_SECRET` refuses server startup; no implicit storage/DB fallback or random reset occurs.
3. Set `SES_SECRET_ID` and `SES_SECRET_KEY` explicitly. They no longer borrow COS credentials.
   Production startup requires these plus the sender/template configuration. Development without SES
   retains the existing local verification-code flow.
4. Choose `STORAGE_BACKEND=cos|local|s3`. `STORAGE_CONNECTION_ID` defaults to `default` and
   `STORAGE_GENERATION` to 1. Keep the generation stable on ordinary credential rotation; change it
   deliberately when replacing the connection/root. Other maintenance commands validate only the
   configuration they actually use. COS-specific `claim`/baseline remain explicitly COS-specific.

Local example (directories prepared separately by the operator):

```dotenv
APP_SECRET=<persisted-application-secret>
STORAGE_BACKEND=local
LOCAL_CONTENT_ROOT=/srv/everything/content
LOCAL_SYSTEM_ROOT=/srv/everything/system
LOCAL_CASE_SENSITIVE=false
```

Both bases need an existing child named exactly the intended owner's UUID. Global content/system
base aliases or nesting are rejected, as are owner children symlinked to another owner's files.
Local startup and native tree operations require no COS settings. Account registration can create an
application account, but this phase does not provision its native root automatically.

Generic S3 example:

```dotenv
STORAGE_BACKEND=s3
S3_ENDPOINT=https://s3.example.com
S3_REGION=us-east-1
S3_BUCKET=everything
S3_PATH_STYLE=true
S3_ACCESS_KEY_ID=<server-side-access-key>
S3_SECRET_ACCESS_KEY=<server-side-secret>
# S3_SESSION_TOKEN=<optional-server-side-session-token>
S3_CONTENT_PREFIX=drive/
S3_SYSTEM_PREFIX=__eam_system__/
```

The adapter uses the official AWS SDK v2 with explicitly supplied credentials; it does not search
ambient profiles, instance metadata or COS configuration. COS uses its native SDK/authentication.
S3 endpoint scheme/region/bucket/path style are deliberate operator choices. Global content/system
prefixes must have trailing slashes and be disjoint at segment boundaries, including NFC equivalence;
per-owner separation alone does not prevent one user's system area from entering another's content.
Use HTTPS outside an operator-controlled local test network. No S3/COS write guarantee is inferred
from API compatibility or these read conformance tests.

## Safe scan and import

```bash
cd backend
# Owner email or UUID is required. Dry-run performs no database writes.
go run ./cmd/eam reconcile --owner you@example.com --dry-run
# Apply metadata import after inspecting the report and coordinating native writers.
go run ./cmd/eam reconcile --owner you@example.com
# New backend configuration/root checks, without a write probe:
go run ./cmd/eam doctor --owner-id 00000000-0000-0000-0000-000000000001
```

`reconcile` now uses the selected factory and recursively enumerates immediate children with explicit
entry/page/depth/time limits. It rejects non-child paths, duplicate entries, cursor loops, incomplete
native pages, invalid names, collisions and changed observed directory versions. A failed scan returns
no applyable result. A completed scan proves enumeration, not cross-system consistency or exclusion
of external writers. Coordinate writers when a stable import is required.

After validation, one owner-scoped serializable transaction imports metadata. It keeps node primary
keys, annotations, `created_at` and event references for unchanged paths, and does not bump `updated_at`
on unchanged repeat imports. Known native size/MIME/modification metadata is refreshed; unavailable
ETag/storage-class/source attributes retain prior values. Opaque native versions are never silently
stored as ETags. Private index keys retain `drive/<owner>/<path>` mapping even when a local path or
custom S3 prefix differs; only nodes/paths code constructs those index keys.

**P1 never prunes index rows or native files.** Existing index-only paths remain for reviewed resolution.
Imports that would collide with retained index spelling/type fail before writes. Native success/index
repair and ambiguous external renames are not guessed from name/size. Dry-run uses a read-only
transaction and reports would-insert/update/unchanged counts. Database failures roll back the import;
retry serialization failures after coordinating writers. No schema migration or byte rewriting is added.

Inventory input requires explicit CSV headers (`key,size`, with optional `etag,storageclass,lastmodifieddate`).
Key cells contain exact unescaped native keys; the parser never trims or URL-decodes them, and bad sizes,
timestamps, duplicate keys or scope violations fail. For COS/S3, keys are below the actual bound content
prefix. Local inventory uses explicit logical `drive/<owner>/` interchange keys. Missing parents are
synthesized in the index only. Inventory EOF is not proof that the provider snapshot was complete.
The existing legacy headerless/lenient CSV format is intentionally rejected. Scanner defaults cap input
to 1000000 entries, 100000 pages, depth 128 and inventory 256 MiB; CLI help exposes bounded overrides.
`doctor --write` remains the legacy COS probe; new backend writes are unavailable in P1.

## Validation and remaining gates

Local real-filesystem tests cover ordinary native reads without Postgres, exact names, empty folders,
copy independence, ranges, short reads, cancellation and root isolation, including concurrent user-root
symlink swaps. COS/S3 use the same SDK-backed HTTP conformance suite for native key/range/version
behavior, bad pages, collisions, cancellation, redirects and close isolation. Those tests establish client
behavior against fixtures; live provider capability probes remain the separate [P0 gate](storage-p0-capabilities.md).

The index importer was exercised against a disposable Docker Postgres 17 database: dry-run, repeat import,
private PK/metadata/event-reference preservation and transaction rollback passed with the race detector.
All 15 existing SQL migrations were applied twice successfully. A second integration test starts the
actual server subprocess with no COS/SES variables and a nonexistent environment-file path: health,
password login, session/account and imported index listing passed; legacy mutations returned 501 with
index snapshots unchanged, and ordinary reads retained original binary bytes and empty folders. It
uses `EAM_TEST_DATABASE_URL` with CREATEDB permission to create/drop a separate disposable database.
No live database, bucket or user root was used, and the disposable containers/processes were removed.
Ordinary tests skip the explicit database integration
unless `EAM_TEST_DATABASE_URL` points to a dedicated disposable database with schema-create permission.

```bash
make test
cd backend
go test -race ./internal/localstorage ./internal/objecttree/... ./internal/cos ./internal/s3storage \
  ./internal/storagebackend ./internal/treescan ./internal/nodes ./cmd/eam
```

Before production write activation, P0 grant draining/baseline evidence and P3–P5 journals, native publication,
quota and resumable receipt recovery are still required. P2 will expose the shared read API and same-origin
bytes; P6 will migrate Web/media clients. This phase does not run a production migration or claim that
Postgres loss restores accounts/permissions/events without their independent backup.
