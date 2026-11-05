# Stable file identity and direct selectors

> Implemented and verified on disposable Postgres/client/built-SPA Chrome fixtures, 2026-11-04;
> targeted race checks passed.
> Canonical parent/name identity, direct ID-or-path selection and eligible Local native tree movement are
> present. The [API](../api/api.md) defines request forms. [Change facts, markers, snapshots, audit and SSE](file-changes.md) are implemented
> as a separate first-phase layer; webhook delivery and asynchronous snapshot construction remain deferred.

## Identity and location

Every file and folder, including the bound account root, has a persisted stable service `id`. Public IDs use
canonical UUID strings, reused from existing node UUIDs where their lifecycle is valid. Clients treat IDs as
opaque values: they never derive native keys, paths or authorization from UUID structure. Root identity is
persisted in `file_identity_roots`, separate from `nodes`; it does not require a fake native file or root node
row. Parent links may refer to that owner/root identity or another node under the same owner/root.

Metadata includes `id`, `parentId`, `name`, `path` and `parentPath`. The root has null parent fields. For a
scoped caller whose parent is concealed, both parent fields are null; the entry's own authorized absolute path
remains visible. No hidden parent handle is disclosed. A root ID must remain stable across reads, import and
restart; it cannot be generated per response or derived from a mutable path.

| Effect | Stable service identity |
|---|---|
| Same-owner rename/move | Retain entry ID and all descendant IDs |
| Service-mediated file content replacement | Retain ID even if native inode/object changes |
| Copy | Allocate a fresh ID for every destination entry, including folders and descendants |
| Delete, then create at the same path | Retire old ID; allocate a fresh ID |
| Move of an ancestor | Retain descendant IDs/contentRevision; returned paths and metadata revisions change |
| Repeated upload commit or idempotent replay | Return the original saved result and ID |
| Ambiguous external rename/recreation | Require native/journal evidence; do not infer continuity from equal bytes or names |

Public ID, metadata revision, contentRevision, provider locator, native ETag, publication generation,
upload ID and Operation ID have separate purposes. A content replacement can change native evidence while
preserving file identity. An upload or Operation ID never becomes a file ID. Deleted/retired IDs are never
reassigned to another entry.

## Selection and destinations

Existing-resource JSON uses `EntrySelector = {"id":"<UUID>"} | {"path":"/Notes/a.md"}`. Query endpoints accept
exactly one nonempty `id` or `path`. Both, duplicates, empty fields and missing required selectors return
`400 invalid_selector`, including when the two fields happen to identify the same entry. Metadata, list,
content, thumbnail and delete require a selector. Search and capabilities default to the authorized root
only when both selectors are absent. Malformed UUID selectors return `400 invalid_selector`; invalid path
syntax returns `400 invalid_path`.

A path request resolves directly on the server in the same HTTP call; callers need no preliminary ID lookup
or ID cache. An ID request selects its current authorized location after completed moves. Unknown, retired,
foreign-owner and concealed selectors return non-disclosing `404 not_found`. An unknown ID cannot create.

Creation/move/copy destinations use one of:

```json
{"path":"/Notes/new.md"}
```

```json
{"parent":{"id":"1f12cfd6-b93b-41ad-ae71-52a4299c3421"},"name":"new.md"}
```

The parent may instead use `{"path":"/Notes"}`. It must resolve to an existing authorized folder; `name` is
one accepted native filename segment. Destinations cannot select or assign an existing file ID. Raw streaming
creation uses full `path`, or exactly one of `parentId`/`parentPath` plus `name`, exclusive with `id` and the
other destination form. Parent-and-name raw PUT is create-only; ID/path replacement requires the existing
contentRevision precondition. `parents:true` folder creation requires a full-path destination.

Current path request shortcuts are deliberately retained: folder `{kind:"folder",path,parents?}`;
move/copy string `from` and/or `to`; upload top-level `path`. They obey the same rules as their object forms.
Folder `path` is exclusive with `destination`. Upload `path` is exclusive with `destination` and `target`:
create/default mode means `destination:{path}`, replace mode means `target:{path}`. Canonical upload create
uses `destination:Destination`; replacement uses `target:EntrySelector` plus the contentRevision condition.
This acceptance does not restore retired routes or JSON envelopes.

## Unique native paths and authorization

Files and folders share one sibling namespace. Enforce owner/root/parent/comparison-name uniqueness with
backend case and Unicode rules; equal names in different parents are valid. Preserve original accepted
spelling and raw contents. Display names, DB uniqueness, native admission, resolution and scope checks must
use compatible versioned comparison rules: nfc-sensitive-v1 or nfc-fold-v1. The latter applies full Unicode
case folding with the same generated mapping in Go and Postgres, including expansions that lower() cannot
represent. Providers permitting duplicate equivalent sibling names require
explicit conflict resolution, even if their native IDs could distinguish them. Do not silently rename or
expose an ambiguous native tree only through IDs.

Resolve both selectors under owner isolation and check current native-aware path scope. IDs grant no access
by possession. Moving outside scope removes access; moving a scope root does not retarget a path grant.
Move/copy checks source and destination; queued effects and pages recheck current ancestry/rights. Hidden
parents stay redacted. Existing Bearer precedence, cookie Origin/CSRF and storage admission remain in force.

Long-running writes bind selected ID, parent, admitted native path/name, ancestor namespace state and root
configuration. Moving/recreating a target or ancestor after admission fails `412 target_changed`; an upload
or queued mutation never silently follows a later move. A new ID request admitted after a completed move
may use its new current location. Idempotency and cursors bind selector mode and intent as well as scope.

## Native effects and identity settlement

Native files remain ordinary readable files at their accepted paths. Adding service IDs never permits
anonymous authoritative blobs, database-only moves or shared mutable copy aliases. Browse metadata/list/
search/usage stays indexed in Postgres without synchronous native calls. Content and mutations still verify
the real backend; outside changes may leave indexed metadata stale until explicit synchronization.

Reuse the existing mutation journal, native evidence and guarded settlement. A non-atomic move reserves
source IDs; it must not present one ID at two independent locations. Until coherent identity settlement is
possible, retain affected guards and report the same Operation. On terminal partial failure with both source
and destination present, source keeps its ID and destination gets a fresh ID. If only a proven moved
destination survives, the original ID follows it. Unknown outcomes retain guards and recovery evidence.

Migration 027 makes parent/name links canonical. The file_entries view derives current paths through ancestry;
legacy path/parent/key columns are nullable caches. Metadata-only writes cannot silently retarget a canonical
link through a stale cache. Old-path recreation resolves current identity and receives a new ID.

Eligible Local folder moves use one native root effect and update root parent/name linkage without mandatory
per-descendant identity writes. The fast path requires a destination prefix no longer in UTF-8 bytes and no
deeper in segments than the source, avoiding unproven descendant path-limit assumptions. Longer/deeper targets
use the finite manifest for preflight. The native Local fixture covers 30,000 descendants; executed integration
evidence is recorded by the parent. Generic flat object moves still copy/verify/delete actual entries.
Dynamic path/search evaluation can cost O(candidates × ancestry depth), and derived cache maintenance can
scale with descendants. Do not claim universal O(1) behavior, native snapshots or global external-writer fencing.

Recursive lastChangedAt/changeToken and ancestor namespace markers are separate: a subtree move changes
effective descendant paths/metadata while sibling edits affect only the relevant aggregate chain. A complete
visible ID/parent tree can apply one subtree linkage change from the feed; narrower/incomplete views refresh
through an immutable snapshot. These markers do not strengthen native content validators.

Current ETag and upload guarantees remain those of the [driver](storage-driver.md) and
[transfer protocol](transfer-protocol.md): no bucket versioning requirement, object reads not version-
pinned, operation-specific native conditions, exact signed EO part requests, explicit receipt provenance and
server-only native completion. Stable IDs do not strengthen native publication conditions.

## Backup and acceptance

Native files remain readable without Postgres or an export interpreter. A scan can rebuild paths and basic
metadata, but cannot reconstruct arbitrary historical public IDs or prove ambiguous outside renames. Back up
identity/root mappings with application metadata; restore them to preserve IDs. Mapping loss/rebuild assigns
explicit new identities rather than claiming continuity. Additive import must not retain a stale ID for a
proven recreated/copied file merely because the destination path already has an old row.

Migration 032 also durably binds the owner's change stream to the current root namespace. First use binds
once; later mismatches do not silently reuse history. After database/mapping restore or every root rebind,
including A → B → A, stop writers, settle accepted work and run file-changes-reset with the current config
and --writers-quiesced. Even a same-namespace restore requires epoch rotation. Reset refuses pending guards/
nonterminal Operations, expires snapshots/subscriptions and clears change markers while retaining IDs,
positions, deduplication receipts, audit and native bytes. It does not recover lost historical file IDs;
backup of the identity mapping remains necessary. See the [runbook](file-changes.md#durable-root-binding-and-restorerebind-procedure).

Implementation acceptance needs repeated migrations/import, existing event references, persisted root IDs,
ID/path equivalence, selector errors, parent destinations/redaction, copy/recreation freshness, move/replace
continuity, ancestor races, scope/owner isolation and unknown-outcome recovery. Existing path/upload/browser
checks passed in the integrated disposable test suites; evidence is recorded in the implementation plan.
Targeted race checks passed. These tests do not establish live deployment or provider admission.
