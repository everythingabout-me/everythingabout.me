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

The parent may instead use `{"path":"/Notes"}`. It must resolve to an existing authorized folder, unless
the creation asks for `parents` (a full-path creation then creates the missing chain first, with the same
folder identity, change facts and audit an explicit mkdir produces); `name` is
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
use compatible versioned comparison rules: nfc-sensitive-v1 or nfc-fold-v1. The latter applies NFC, full
Unicode case folding, then NFC again, with the same generated mapping in Go and Postgres, including
expansions that lower() cannot represent. The trailing NFC is part of the version, not a detail: folding can
leave a base letter and a following combining mark that then compose, and a pipeline that stops after the
fold treats two spellings as different names that the unique sibling index merges. Go has exactly one
implementation, `internal/namecompare.Key`; a call site that re-derives the pipeline is a defect, because it
can admit a write the index rejects or deny an index-equal sibling. Providers permitting duplicate
equivalent sibling names require explicit conflict resolution, even if their native IDs could distinguish them. Do not silently rename or
expose an ambiguous native tree only through IDs.

Stored `comparison_path` caches were written by an older release that stopped after the fold. Migration 052
rewrites them, because the canonical key is exactly the legacy key with one NFC applied. The release that
applies it also matches stored values through `normalize(comparison_path, NFC)`, so a row the old binary keyed
with the legacy spelling is still found: `eam migrate` runs before the service restart, and the old binary
keeps writing legacy keys for those seconds. Without that read shim, a read looking for the canonical spelling
could miss an open upload session or a credit claim. The following release normalizes the rows written during
the window once more, drops the shim's expression index, and deletes the shim.

Resolve both selectors under owner isolation and check current native-aware path scope. IDs grant no access
by possession. Moving outside scope removes access; moving a scope root does not retarget a path grant.
Move/copy checks source and destination; queued effects and pages recheck current ancestry/rights. Hidden
parents stay redacted. Existing Bearer precedence, cookie Origin/CSRF and storage admission remain in force.

Long-running writes bind selected ID, parent, admitted native path/name, ancestor namespace state and root
configuration. Moving/recreating a target or ancestor after admission fails `412 target_changed`; an upload
or queued mutation never silently follows a later move. The converse is equally binding: a failure to
observe the destination (a throttled listing, a provider 5xx) is not evidence that it changed, so it stays
retryable (`502 storage_failure`) and the session keeps its staged bytes instead of ending as the same
terminal 412. A new ID request admitted after a completed move
may use its new current location. Idempotency and cursors bind selector mode and intent as well as scope.

## Folders without a marker

An empty folder is a state only a person can ask for. An import passes through "this folder has no
children yet" on its way to writing thousands of them, and today both states are made with the same
native marker object: `parents:true` runs the mkdir action for every missing ancestor of every
destination. On a connection where one provider round trip costs 240 ms and a marker costs six of
them, an 11-year health export pays roughly two hours for directories that exist only because a file
is about to land in them.

Keep the index equal to the tree instead of inventing a second kind of folder:

**Admission creates nothing.** `parents:true` stops materializing ancestors; it only says that a
missing ancestor is acceptable. An upload into a folder that does not exist yet binds a parent path
and no parent ID, so the fence does not apply to that ancestor (`CheckUploadBinding` already binds a
parent that exists by row, ID and current path).

**Settlement materializes.** When the file lands, its settlement creates the ancestor rows its path
implies, in the same transaction as the file, parent first. Every row in the index is still backed by
the tree — the prefix exists exactly because the file that carries it landed — so the invariant does
not grow a "promised" state, a flag, an expiry sweep or a reconcile special case.

**A row the tree lost is settled away, not left standing.** A deletion whose target is already
absent natively — removed by another tool, or a folder that only ever existed as its children's
prefix — settles the index rows instead of answering `404`: the bytes are gone, so the row the caller
can still see is the only thing left to remove. Two limits make that safe. It reads `not_found` as
"absent" and nothing else: a transient error, a refusal or a wrong bucket fails the delete rather than
settling anything. And a non-recursive deletion still refuses a path the index holds children under,
so the absence of bytes never turns one click into a subtree. A path with no rows at all is still
`404`. The cost of the trust is worth naming: a provider that answered `404` for everything would let
a delete strip rows for files whose bytes still exist — index loss, not data loss, because the tree is
authoritative and the next reconcile re-adopts them.

**A filesystem is not an object store.** A provider that makes a folder when its first child is
written (COS/S3) needs no marker here; one that does not (Local) keeps creating the chain at
admission, before the session binds it. That is one driver capability, not a state machine.

What this gives up: an upload into a not-yet-existing folder binds no parent ID, so a folder created
and deleted again at that path during the upload's window is not a `412 target_changed`. The file
lands at the path the client asked for, which is what it promised. Everything else keeps its fence.

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
pinned, operation-specific native conditions, length-bound class A part URLs, explicit receipt provenance and
server-only publication in every transfer class. Stable IDs do not strengthen native publication conditions.

## Write-path audit

A review described the implementation as "the canonical identity still goes through a derived-cache column":
the trigger derives `nodes.parent_id` from the path string (one Go repair statement writes the column directly,
`upload_observation.go:100`), and the `file_entries` view falls back to the cached path when ancestry resolution
fails. This section traces that dependency, records
an executable probe matrix (`backend/internal/nodes/canonical_identity_audit_test.go`), and decides what it is.
The audit was done on 2026-11-13 at `7c22410`; line references are to that tree.

### The trace

`file_entries` is a view, not a table. Nothing inserts into it; every row is created by `insert into nodes(…)`
and the view computes `path`, `parent` and `key` on read (`sql/027_canonical_file_tree.sql:66-73`). A row's
creation therefore has two halves: the settlement writes the legacy columns it has proven, and the BEFORE
trigger derives the canonical ones in the same transaction.

| Write path | SQL site | Columns the application supplies | Canonical source |
|---|---|---|---|
| Upload commit (staged publication) | `backend/internal/nodes/file_write.go:79` | owner, key, path, parent, name, is_dir, size, mime, source_modified_at | native entry is checked against `intent.Target`, then the trigger |
| Implicit upload ancestors | `file_write.go:57` → `backend/internal/nodes/upload_observation.go:89` | ancestor path/parent/name | trigger; the only direct Go `parent_id` write is the dangling-link repair at `upload_observation.go:100` |
| Upload observation / confirmation | `backend/internal/nodes/upload_observation.go:147,191` | path/parent/name/key and observed metadata | trigger |
| mkdir / copy / move settlement | `backend/internal/nodes/file_actions.go:367,397` | target path/parent/name/key from the proven mutation result | trigger |
| Eligible native folder move | `backend/internal/nodes/file_tree.go:54` | one row's new path/parent/name/key | trigger; descendants are untouched |
| Scan / reconcile import | `backend/internal/nodes/scan.go:234` | scanned path/parent/name/key | trigger, including implicit folders |
| Deletes | `file_actions.go:169,213,248,347,502`, `scan.go:381`, `backend/internal/nodes/nodes.go:237` | identity or path only | none (rows leave) |
| Legacy claim | `backend/cmd/eam/claim.go:134` | owner and key only | trigger re-derives from the unchanged path |
| Legacy `nodes.Service` writers | `nodes.go:145,182,248,269` | path/parent/name | trigger; no production caller is left (grep at this tree), but tests and fixtures still use them |

What sets each canonical fact:

- `parent_id` and `comparison_name` are set only by `file_canonical_node_write`
  (`sql/027_canonical_file_tree.sql:75-158`), a BEFORE INSERT OR UPDATE trigger on `nodes`. It takes a shared
  advisory lock for the owner's name rules, reads the root and `name_comparison_version`, walks the path's
  segments resolving each against `nodes(owner_id, parent_id, comparison_name)`, creates missing ancestor rows
  with `on conflict (owner_id, parent_id, comparison_name) do nothing`, rejects an explicitly supplied parent
  that disagrees with the path-derived one, rejects cycles, then writes `NEW.parent_id` and
  `NEW.comparison_name := file_identity_comparison(NEW.name, version)`.
- The comparison pipeline is SQL's `file_identity_comparison` (`sql/026_file_identity.sql:1693-1700`) and Go's
  `internal/namecompare.KeyString`; the trigger is the only production writer of the column, and
  `file_identity_root_rules_changed` (`sql/026_file_identity.sql:1812-1821`) rewrites every row's
  `comparison_name` when the root's rules change.
- `membership_revision` is bumped by the AFTER trigger `file_tree_membership_changed`
  (`sql/027_canonical_file_tree.sql:168-185`) on the old and new direct parents; it does not walk descendants.
- Ancestry is `nodes.parent_id`, ending at `file_identity_roots.id` for the owner root. There is no foreign key
  on the column: the trigger plus `nodes_owner_parent_comparison_uniq` are the guards.
- The view's `parent` is `regexp_replace` of the resolved path, not the stored `parent` column; the stored
  `path`/`parent`/`key` are caches with their own unique indexes (`nodes_owner_path_uniq`, `nodes_key_key`).

Where the application could write the canonical columns instead: every settlement already has the proven path,
and most have the bound parent identity (`storage.WriteIntent.ParentID`, `fileops.ActionRequest.ParentID`).
The missing pieces are not the values but the invariants: ancestor materialization before resolving the parent,
cycle rejection, sibling-conflict rejection, the old-path cache cleanup that keeps the two legacy unique
indexes writable, and the lazy root row. A Go writer would need one shared helper called before all nine
insert/update statements above; `ensureUploadParents` is the existing precedent for the parent-link half.

### Probe matrix

Run with `EAM_TEST_DATABASE_URL=… go test -run TestCanonicalIdentityWritePathAudit ./internal/nodes`. The test
applies every migration twice to a scratch database and writes rows exactly as the settlement paths do.

| Probe | Identity | Ancestry | Authorization input | Where the trigger is load-bearing |
|---|---|---|---|---|
| Rename in place | ID retained; `comparison_name` follows the new spelling; the old spelling stops resolving | `parent_id` unchanged | current path is the new spelling, so a path scope is judged on the new name | without it the row keeps the old `comparison_name`, the unique sibling index still sees the old key, and identity does not resolve the new name at all |
| Move to another folder | ID retained; `parent_id` becomes the destination folder | new parent link; old path stops resolving | current path is the destination path | `parent_id` is only ever the trigger's `expected` value; without it the link stays at the old parent |
| Subtree move (one root row) | root ID retained; descendant IDs and `parent_id` untouched | descendants resolve through the moved root; their cached `path`/`parent` stay stale by design | current path is the new location, so a scope excludes the old subtree | the whole derivation is the trigger: without it the view would fall back to the descendants' stale cache |
| Delete, then create at the same path | a fresh ID; the old ID never resolves again | the re-created row gets new ancestor links | the new ID owns the path | the trigger's `update nodes set path=null,parent=null,key=null where path=NEW.path` (`sql/027:149-151`) clears the moved descendants' stale caches, so `nodes_owner_path_uniq` and `nodes_key_key` do not collide; without it the re-creation fails |
| NFD spelling (`cafe\u0301.txt`) | accepted spelling preserved in `path`/`name`; `comparison_name` is the NFC form | one row, no ambiguity | NFC and NFD spellings both resolve to the same ID | without `comparison_name` neither spelling resolves by identity; only an exact cached-path match would find the row |
| Case-insensitive sibling (`nfc-fold-v1`) | `README` and `readme` are one identity; the second insert raises `23505 canonical sibling spelling conflicts`; `Straße`/`STRASSE` collide through full folding, not `lower()` | same parent | mixed-case requests resolve to the one ID | the trigger computes the fold key and rejects the conflict; without it both spellings coexist with NULL keys and the unique sibling index cannot merge them |
| NULL path cache | metadata-only update nulls `path`/`parent`/`key`; ID and ancestry still answer | `file_tree_path` derives the current path from `parent_id` | current path unchanged | the trigger's metadata-only branch (`sql/027:99-104`) keeps the link instead of re-deriving or clearing it; a relink through a NULL cache is refused (`23514`) |
| Stale cache with broken ancestry | ID still resolves, but to the stale cached path | `file_tree_path` returns NULL because an ancestor row is missing; the view `coalesce` returns the old cache | the scope check sees the old location (inside a scope the entry has left) | nothing repairs the chain here; the fallback is the only thing that answers, which is why it is debt |
| No BEFORE trigger | `parent_id`/`comparison_name` are NULL; `file_tree_resolve` returns NULL; the view falls back to the path cache for every row | no ancestry at all | only the exact cached path works | every production upsert targets `(owner_id,parent_id,comparison_name)`; with NULL identity the conflict target cannot match: a same-path retry fails on the still-unique key instead of updating, and a different key or path would insert a duplicate because NULL conflict keys never match |
| Explicit canonical columns | accepted when `parent_id` agrees with the path | same as normal | same as normal | the trigger overwrites `comparison_name` with its own value, so a Go spelling disagreement is silently corrected today; a validation-only trigger would have to reject it |

The trigger is load-bearing in seven places, all in the write transaction: the upsert conflict target, the
sibling uniqueness key (case and Unicode), the parent link every read walks, ancestor materialization for
implicit folders, cycle rejection, old-path cache cleanup for the two legacy unique indexes, and the lazy root
row plus the path/parent/name consistency checks. Removing it is not a no-op that leaves paths working: the
index degrades to the pre-027 cache, and writes stop settling.

### The cached-path fallback

The view's `coalesce(file_tree_path(n.owner_id, n.id), n.path)` (`sql/027:73`) is reachable exactly when
`file_tree_path` returns NULL: the row has no `parent_id`, an ancestor row is gone, or the owner root is
missing. Normal settlements always give an owned row a `parent_id`, so a null result means index damage — or
an unclaimed legacy row (`owner_id IS NULL`), which is the fallback's legitimate case. It is not a purely
theoretical state: `ensureUploadParents` (`upload_observation.go:97-103`) exists to reattach a child whose
parent row was removed, and its comment records that a concurrent subtree deletion can produce it.

What it can return wrong: for an owned row with a broken chain, the view answers with the last cached path, and
that string is the input to `fc.WithinScope` for an ID selector (`backend/internal/api/files_identity.go:58-70`).
The entry can therefore appear inside a scope it has left (the stale-cache row of the matrix). Reads do not leak content: the read path
re-resolves the selected ID against the path (`filesCheckReadIdentity`, `files_identity.go:271-299`) and answers
404 when they disagree. Mutations are the sharper edge: upload/move/delete binding re-resolves the same ID and
gets the same stale path (`filesCheckIdentityBindings`, `files_identity.go:222-269`), so a delete by ID can be
presented as owning a path that a different entry now occupies. A NULL cache on top of a broken chain makes the
view return NULL, which `FileResolveID` cannot scan into a string: the request fails, but as an internal error
rather than a truthful 404. Change snapshots and change filters read the same view
(`backend/internal/api/files_changes.go:671`, `files_change_snapshots.go:181,432`), and path-derived timeline
facts consume it too.

The fail-closed fix does not need the view rewritten: an ID selector is already owner-scoped, so its ID→path
lookup can ask `file_tree_path` strictly for owned rows and report not-found when it returns NULL, leaving the
`coalesce` for `owner_id IS NULL` legacy display. That is one query in `resolveFileID` plus the three direct
view reads in the change API, and it turns the only wrong answer into a 404.

### Verdict

**Deliberate design, not a defect. The one piece of debt is the fallback, not the trigger.**

- It is design because the writer is the settlement, not an arbitrary caller: each of the nine statements above
  passes the path that the native effect just proved (`commitStagedFile` checks it against `intent.Target`;
  `settle*` uses the mutation result; scan uses the native walk; claim leaves the path unchanged), and the
  trigger canonicalizes it in the same transaction. After commit, `parent_id` is the identity every read uses;
  the path is not consulted again unless ancestry is already broken.
- It is not a defect because the written contract (`AGENTS.md`, "Canonical identity is
  owner/root/parent/name/comparisonName; legacy path/key columns are nullable derived caches") says where
  identity resides, not which layer writes it. Migration 027 deliberately makes the database the single writer
  ("Canonical parent/name authority permits a native tree rename to relink one row"). A Go writer on top of the
  current trigger would be overwritten by it anyway (the explicit-columns row of the matrix), so "no Go code writes it" is intended, not a
  gap between contract and code.
- It is not clean either. The trigger's write input is the path cache, and the only exception to "the
  application never writes canonical columns" is a repair statement (`upload_observation.go:100`). That is
  tolerable while every writer is a settlement; it becomes a defect the day a writer needs to set identity from
  an ID or a provider token rather than from the operation's path. The fallback is the part that is already
  wrong: it converts "the chain is broken" into a plausible cached answer.

So the review's observation is accurate but its label is not. The canonical identity does pass through a path
on the way in; it does not pass through a derived cache on the way out, except in the fallback branch.

### If the writer moves (plan, not implemented)

The dependency is real and removable, but not cheaply, and it buys no correctness while the trigger remains a
fallback. If a later change needs it — a provider that reports identity without a stable path, or a writer that
must bind an ID directly — this is the sequence:

1. **Migration 054, validation instead of derivation.** Recreate `file_canonical_node_write`: for owned rows it
   derives only when `NEW.parent_id` or `NEW.comparison_name` is NULL (the fallback), and otherwise verifies
   that the supplied parent exists under the owner/root, is not the row or a descendant, and that
   `NEW.comparison_name = file_identity_comparison(NEW.name, version)`; a mismatch raises `23514` instead of
   being silently overwritten. Keep the AFTER membership trigger and the name-rules trigger.
2. **One shared Go resolver.** `identityFor(parentID or parentPath, name)` returns `(parent_id, comparison_key)`
   using `namecompare.KeyString` and `file_tree_resolve` after materializing missing ancestors; every write site
   above passes both columns. Prefer the already-bound `ParentID` from the intent when it is present, and fall
   back to the path only for `parents:true` and scan imports.
3. **Drop the fallback, then the derivation.** After every writer is converted and a full re-index proves it, a
   later migration rejects NULL canonical columns for owned rows (a `NOT VALID` check validated after the
   re-index) and removes the derivation branch. The trigger can stay as a cheap validator; only dropping it
   entirely would also move the cycle check, the cache cleanup and ancestor materialization into Go.

Migration and re-index cost: no existing row needs a backfill — every owned row already has both columns from
the trigger — so the schema step is one function replacement. Verification is one full re-index per owner
(`eam reconcile --owner <uuid>`, the same native walk and O(entries) upserts the service already needs after a
restore), because that is the only way to exercise every write path against a real tree; the Local 30,000-
descendant fixture (`backend/internal/nodes/file_tree_test.go:12`) and a 50k-entry owner are the two useful
shapes. The real cost is the nine statements and their tests, plus keeping Go and SQL comparison pipelines in
step (already pinned by the nameprobe suite). Until step 3, the trigger still answers for direct SQL, fixtures
and any writer not yet converted, so the change is forward-compatible but its correctness is only proven by the
re-index.

Recommendation: do not do this now. Keep the trigger as the single writer; fix the fallback (strict ID→path for
owned rows) and record this reason next to the trigger. Revisit the writer split when a write path actually
needs to name an entry without a path — not to make the diagram look cleaner.

## Backup and acceptance

Native files remain readable without Postgres or an export interpreter. A scan can rebuild paths and basic
metadata, but cannot reconstruct arbitrary historical public IDs or prove ambiguous outside renames. Back up
identity/root mappings with application metadata; restore them to preserve IDs. Mapping loss/rebuild assigns
explicit new identities rather than claiming continuity. Additive import must not retain a stale ID for a
proven recreated/copied file merely because the destination path already has an old row.

Migration 032 also durably binds the owner's change stream to the current root namespace. First use binds
once; a later bucket, region, prefix or generation change makes startup refuse and name both roots. The service
does not adopt the configured tree, because it must not read, write or delete the previous one: after copying
the bytes across, the operator runs `eam storage-rebind --owner-id UUID --from BOUND --to CONFIGURED
--writers-quiesced`, which moves the budget binding and rotates the change-stream namespace without native I/O.
After a database/mapping restore on the same root, stop writers, settle accepted work and run file-changes-reset
with the current config and --writers-quiesced. Even a same-namespace restore requires epoch rotation. Both
commands refuse pending guards/nonterminal Operations, expire snapshots/subscriptions and clear change markers
while retaining IDs, positions, deduplication receipts, audit and native bytes. They do not recover lost
historical file IDs; backup of the identity mapping remains necessary.
See the [runbook](file-changes.md#durable-root-binding-and-restorerebind-procedure).

Implementation acceptance needs repeated migrations/import, existing event references, persisted root IDs,
ID/path equivalence, selector errors, parent destinations/redaction, copy/recreation freshness, move/replace
continuity, ancestor races, scope/owner isolation and unknown-outcome recovery. Existing path/upload/browser
checks passed in the integrated disposable test suites; evidence is recorded in the implementation plan.
Targeted race checks passed. These tests do not establish live deployment or provider admission.
