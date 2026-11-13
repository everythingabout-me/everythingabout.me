# Storage driver contract

> Native-tree contract. Read-only Local/COS/S3 trees, mutation/publication interfaces, class A presigned
> uploads/read grants and class C relay writes exist. Limited recorded COS ETag observations do not establish
> every operation or deployment capability, so default writes stay disabled without admission. Class B has
> interfaces and configuration but no cloud-drive driver yet. Preserving the readable tree is
> mandatory. Stable IDs are implemented with canonical parent/name links and direct selectors; disposable
> integration/browser tests and targeted race checks passed.
> See [identity](file-identity.md) and [first-phase changes/audit](file-changes.md).
> A driver declares the [transfer classes](transfer-protocol.md) it can serve and each connection selects one;
> the transfer protocol owns byte paths, geometry, provider rules and deployment requirements.
>
> [Architecture](storage.md) requires original names, complete native folder structure and directly usable
> file bytes. [The file API](../api/api.md) remains same-origin and backend-neutral. This replaces the
> anonymous Blob/Open/Put/Probe/Drop design; IDs may be native locators, never the only stored filenames.

## 1. Boundary and path model

Each owner is bound to a content root and a separate system/staging root. Paths below the content root map
to the same relative native folder/name structure. S3 uses ordinary prefix keys and empty-folder markers;
cloud-drive adapters resolve paths to native IDs internally while preserving actual folders/names. A known
native ID can accelerate lookup but cannot replace the visible destination tree.

The API owns access control, index/search, stable service IDs and parent/name mappings, quota and durable
operation journals. All clients select existing files/folders by service ID or direct path at the same API.
The service resolves IDs under owner/current path scope, binds admitted identity/location, and passes native
paths/evidence to the driver. Native IDs and provider keys remain private; they are not service IDs. Drivers implement real
file operations, native authentication, naming constraints, version evidence and backend recovery. Drivers
must enumerate their content root, including existing files, so indexing and direct migration need no data
rewrite. They do not own a separate application database, and they expose provider URLs to clients only
through the single-session or single-file grants of their transfer class.

Application page metadata, directory listing, search and indexed usage read Postgres directly. Driver
Stat/List/Open are used by content, actual mutations and explicit scans, not by interactive browsing.
A native readable tree remains authoritative and independently usable even though its database view may
lag external changes. See the [indexed browsing decision](storage.md#3-storage-facts-and-database-responsibilities).

The interfaces below are intended Go declarations. Root bindings can be cheap views over shared pooled
clients; do not allocate a separate HTTP pool permanently for every user. Configuration generation identifies
the actual backend/root; credential rotation alone need not change that identity. Capability evidence describes
exactly one connection, so a bucket, region or endpoint change invalidates it: the server re-probes the newly
configured connection and records the result rather than carrying the old measurement across. Use typed options per adapter
at construction, validate them there, and never construct provider clients in an HTTP handler.

## 2. File and folder interfaces

Three words cover state in this service. They are not interchangeable.

- **revision** — `Entry.Revision`, `Conditions.ExpectedRevision`, `MoveRequest.SourceRevision`. The driver's
  opaque token for the exact native state of one entry. It is what a conditional effect compares against and
  what proves an effect applied to the node we observed. Its derivation belongs to the driver: the object ETag
  on S3/COS/R2, a generation number on GCS, a revision id on a cloud drive, or the rename-stable inode metadata
  of a local file. It is internal and never crosses the API boundary. A revision is a native state token, not a
  content hash: a driver must not read a file's bytes just to derive it, and a comparison against it proves the
  state the driver can observe, which is all a conditional effect can be compared with.
- **version** — `Descriptor.RetainsVersions`, `Descriptor.PinnedRead`, `ReadOptions.PinnedVersion`,
  `objecttree.Object.VersionID`. A retained historical copy held by the provider. Version history is a product
  feature this service does not use: every deployed driver reports `RetainsVersions=false` and
  `PinnedRead=false`, and no API exposes a version list, restore or pin.
- **index revision** — `nr_…` in a file resource, `If-Revision`, `ifSourceRevision`. Our own token over the
  indexed metadata. It gates application mutations and says nothing about the native state.

A revision is not a version. The current state has a revision even where the provider keeps no history, and a
version id never names the current state. Byte spellings that are hashed or persisted keep their historical
form (`native-etag:`, `native-version:`, the `Version` key inside content-revision evidence) so existing
records stay comparable; only the Go vocabulary changed.

```go
package storage

import (
    "context"
    "io"
    "time"
)

type Path string // normalized, root-relative; "" is the bound root

type Kind string
const (
    File Kind = "file"
    Folder Kind = "folder"
)

type Entry struct {
    Path       Path
    Kind       Kind
    NativeRef  []byte // internal native ID/locator, optional
    Revision   []byte // opaque native state token; see the vocabulary above
    Size       int64  // file only
    ModifiedAt time.Time
    MediaType  string
    Digest     *Digest
}

type Digest struct {
    Algorithm string
    Value     []byte
}

type PageRequest struct {
    Cursor string
    Limit  int
}

type Page struct {
    Entries    []Entry
    NextCursor string
}

type ReadOptions struct {
    Offset      int64
    Length      *int64 // nil means remaining bytes; non-nil exact positive range length
    PinnedVersion []byte // provider version id to read; empty means the current state
}

type Read struct {
    Body  io.ReadCloser
    Entry Entry // describes the actual selected representation, including full size
    Offset int64
    Length int64 // bytes returned
}

type Tree interface {
    Stat(context.Context, Path) (Entry, error)
    List(context.Context, Path, PageRequest) (Page, error) // immediate children
    Open(context.Context, Path, ReadOptions) (Read, error)
}

type Conditions struct {
    Absent          bool // atomic create-only at the destination
    ExpectedRevision []byte // conditional mutation of a native revision, exclusive with Absent
    // ImplicitFolder states that the addressed folder has no marker object: the provider
    // makes the folder from its children, so it exists only while they do. Valid only for
    // Remove and only when the caller observed no revision; see Remover below.
    ImplicitFolder bool
}

type Mutation struct {
    // Durable unique intent; not a claim of native idempotency. An id names one accepted
    // operation, never a path alone: a driver retains a settled receipt for IntentRetention,
    // so a path-keyed id would answer a later operation from an earlier one's receipt.
    ID string
    Path Path
    Conditions Conditions
}

type MutationResult struct {
    Entry *Entry // set for publish/mkdir/move/copy; absent for remove
    Evidence []byte // operation-specific outcome evidence persisted by the API
}

type FolderWriter interface {
    Mkdir(context.Context, Mutation, Checkpoint) (MutationResult, error)
}

type MarkerReassertor interface {
    // Reassert moves an existing folder marker's native revision past the one the
    // caller observed, without removing content. Used only to fence a create against
    // a delete that is still purging the same path.
    Reassert(context.Context, Mutation, Checkpoint) (MutationResult, error)
}

type Remover interface {
    Remove(context.Context, Mutation, Checkpoint) (MutationResult, error)
    // Removes a file or an empty folder only. The service must explicitly enumerate
    // and journal recursive deletion; this call never recurses implicitly.
    //
    // The native state to remove is named, never guessed. A file or a folder with a
    // marker object is removed with Conditions.ExpectedRevision set to the revision the
    // caller observed. A folder reported with no revision has no marker object, so
    // removing its children removed it; that case is expressed with
    // Conditions.ImplicitFolder, and the driver must verify that no marker and no child
    // appeared since. A removal with neither is refused with `missing_revision`.
}

type MoveRequest struct {
    ID string
    Source Path
    Target Path
    SourceRevision []byte
    TargetConditions Conditions
}

type NativeMover interface {
    Move(context.Context, MoveRequest, Checkpoint) (MutationResult, error)
}

type NativeCopier interface {
    Copy(context.Context, MoveRequest, Checkpoint) (MutationResult, error)
}
```

`Reassert` exists for one ordering, not as a general rewrite: a mkdir can adopt a folder
marker that a delete has already listed but not yet removed. The index row is live again,
yet the purge's snapshot and its one conditional delete still name the old marker. Moving
the marker's native revision makes that delete fail with `changed`, which the purge adopts
as the user's newer intent instead of removing the folder.

The contract is narrow and testable:

- It must change the revision a conditional delete compares. On an object store the
  revision is the marker's ETag, and the ETag of a zero-byte object is the empty-content
  MD5 — the same for every marker and every rewrite. A reasserted marker therefore carries
  a short unique token in its body; the key still ends in `/`, `Stat` reports the folder's
  `Size` as zero, and listings treat it as an ordinary folder marker. On a local filesystem
  the revision is the directory identity, so the directory inode is replaced by renaming a
  freshly prepared empty directory over the target.
- It refuses with `not_empty` rather than remove or replace a folder that now holds
  children. User content is never deleted to move a marker; a folder with children is
  already kept by the purge's survivor rule.
- It refuses with `changed` when the marker no longer matches
  `Conditions.ExpectedRevision`. An empty expected revision means the caller created the
  marker in this operation and only needs its revision to move.
- When the marker is already gone, it leaves a native marker behind (a conditional create
  on an object store, a fresh directory locally) instead of reporting success with none:
  the purge won that race and the create must still end with a native folder.
- It is safe to repeat. It is a fence, not an authoritative content mutation, so a lost
  reply is answered by the caller retrying the create; the effect only rewrites an empty
  marker or replaces it with another empty one.

A driver with a conditional marker write (object store) implements it with that write; a
driver whose folder revision is its inode replaces the directory and shares a per-path lock
with removal, because an unconditional unlink would otherwise race the check.

`Copy` must not stream source bytes through the gateway when the driver can copy
inside the provider. A provider-side copy binds the exact source revision with a
native condition, refuses an occupied target natively, records an intent marker
that attributes a lost reply, and publishes an object above the provider's
single-request limit (5 GiB on COS and S3) through conditional part copies whose
completion stays one request. A driver without that capability may relay through
private staging only below the configured single-PUT limit; the service never
degrades a large copy into a read/put pair it cannot fence.

```go
type Checkpoint func(context.Context, []byte) error

type Driver interface {
    Tree
    Describe() Descriptor
    Close() error
}

type Factory interface {
    Open(context.Context, Config) (Driver, error)
}

type Config struct {
    ConnectionID string
    Generation int64
    OwnerID string
    ContentRoot string
    SystemRoot string
    Credentials SecretSource
}

type SecretSource interface {
    Load(context.Context) ([]byte, error)
}

type Descriptor struct {
    MaxFileBytes int64
    RangeRead bool
    PinnedRead bool
    AtomicCreate bool
    AtomicReplace bool
    NativeConditionalMutation bool
    NativeTreeMove bool
    TransferClasses []string // subset of "presigned-parts", "resumable-session", "relay"
    Names NameRules
}

type NameRules struct {
    CaseSensitive bool
    MaxSegmentBytes int
    MaxPathBytes int
    MaxDepth int
    ReservedNames []string
}
```

A driver may expose a private `ContentIdentity` on Entry when it proves a stable native identity independent
of pathname. It is native continuity evidence, separate from the public stable service UUID.
The service, not the driver, owns the public ID lifecycle and persisted root mapping. Same-owner moves and
content replacement preserve service IDs; copies/recreation allocate fresh IDs. Evidence-bound application content-generation
mappings may retain contentRevision across a proven copy/delete move, but must reject a changed native
fingerprint. WriteIntent additionally captures optional source modification time, original client condition,
and parent path/version so recovery cannot retarget a replaced ancestor. ID-selected work captures file/parent
ID and relevant ancestor namespace state as well: an intervening move/recreation fails target_changed rather
than silently following a stable ID. Parent redaction and selector validation are API responsibilities.

### Reference-addressed reads

Content delivery must not re-derive the namespace on every transfer. `Stat` and `Open` answer path-addressed
questions; an object adapter settles one path with the byte-exact probes described under *a path costs what
the path names* and never walks the ancestor chain, before it can touch one object. A driver may therefore
implement the optional capability

```go
type ReferenceReader interface {
    StatReference(context.Context, Path, []byte) (Entry, error) // one native HEAD
    OpenReference(context.Context, Path, []byte, ReadOptions) (Read, error) // one ranged GET
}
```

where the `[]byte` is the native key the owner-scoped index already stores for that path. The index only says
where the file is; bytes and validators still come from native responses, and README-level behaviour is
unchanged because `Stat`/`Open` remain the authority whenever no reference is available. Implementations must
compare the reference with the key they derive for the path and return `stale_reference` for anything else:
a stale index is not authority to read a different object or another root generation.

Reference equality is byte-exact: a key from an older root generation, or one whose spelling the requested
path cannot reproduce byte for byte, deliberately falls back to path-addressed traversal instead of guessing
that two spellings denote the same object.

### A path costs what the path names

Path-addressed `Stat` stays constant-cost: an object-store adapter settles one path with at most three
byte-exact probes — the file key, the folder marker key (`key + "/"`), and one bounded listing of the
folder's own prefix for a prefix folder that has no marker — and never walks the ancestor chain. The listing
is of the path's own prefix, not of its parent: a parent listing returns every sibling, so it would cost a
function of the directory instead of a function of the entry being observed, which is the cost this rule
exists to forbid. `Open` makes the same observation before its one transfer, and `List` uses one prefix
listing as both the existence proof and the enumeration. Depth is a property of the tree, not of the
cost of observing one entry; a manifest rewrite that re-observes every captured identity before and after
each entry would otherwise pay the depth again for every step. The consequences are explicit: a path whose
ancestors include a file is not rejected during observation, and a key that exists both as a file and as a
prefix folder is observed as its file, because that is the key that can carry bytes. Duplicate spellings
that normalize to one name are the same case: the observation returns the exact key asked for and only a
listing can report the sibling. Keeping the index free of these shapes is the index's job — the complete
scan fails the listing instead of indexing an ambiguous directory — and an outside writer that produces
them is a reconciliation finding, not a read error.

Callers order the calls as observation → condition → transfer, so a 304 or a HEAD costs one native request and
nothing is opened. `OpenReference` transfers exactly the requested range once; it must not open a full object
and discard it to reposition. Because native object reads are validated but not version-pinned, the returned
`Entry.Revision` is the revision the caller observed with `StatReference`, exactly as `Open` already reports the
revision from its own HEAD.

Missing entries return a typed not_found error; not nil success. List is mandatory even on read-only drivers.
The service recurses paginated child listings or uses an optional inventory optimization. Native IDs/versions
remain private and are not file contents or public app IDs. Read callers must close Body and handle context
cancellation. The driver must not silently serve a different version or ignore a requested byte range; it
returns changed/unsupported and the API chooses an explicit fallback. Pinned reads may select a native version where
one is retained, otherwise the driver refuses with unsupported.

Files and folders share one native sibling namespace. Service uniqueness uses owner/root/parent and the
versioned backend comparison-name rules; original accepted spelling stays visible. Different native IDs do
not justify duplicate equivalent names. Root identity is persisted separately from a native root locator.

NameRules are not enough to describe every provider restriction: adapters also validate all reserved names,
normalization collisions, punctuation and trailing-character rules before mutation. Unsupported names return
invalid_name/name_conflict, never an anonymous or escaped native filename. Duplicate native names that make a
path ambiguous return ambiguous_path from the listing that materializes the directory; a single path
observation answers for the exact key it names instead, because only a listing can see the sibling that
collides. Any destination conversion during export to a different filesystem must
be explicit, reversible when offered, and must not silently overwrite.

AtomicCreate/AtomicReplace describe tested single-file native visibility, not arbitrary directory transactions.
NativeConditionalMutation describes real backend-enforced version/absence checks, not Stat then write.
Conditions that cannot be implemented with native support or a declared coordinated-exclusive writer model
return unsupported before an unsafe effect. Conflicting external writes are outside an application lock;
do not claim they are fenced. An atomic no-replace primitive is needed to guarantee absence against them.
A native MOVE that is atomic only within a filesystem must not be advertised as atomic across volumes.

### Why a listing does not establish a revision

A revision is only ever taken from an observation that returns one *for that exact entry*, and only from the
channel every revision it will be compared against comes from. Two facts force that:

- The listing API this adapter uses does not carry a per-object version id at all. S3 `ListObjectsV2` and the
  COS bucket listing have no version field; the tree's revision is a version id, so a listing cannot supply
  it even in principle.
- Both listings do return an ETag, but whether a listing's ETag is byte-identical to the one a HEAD returns
  is a per-provider fact, not a portable guarantee. Mixing the two would make `sameNative` compare tokens
  from different sources and report `target_changed` for an object that never moved — a false failure, and
  the expensive kind: it stops a move that was going correctly.

So each channel stays self-consistent: the tree takes a revision from HEAD/GET, the coordinator takes one
from HEAD. This is not a claim that ETags are useless — the same fixture also serves one ETag for every
object, which guards a different rule: an ETag is not an *identity*. Identity is the key (`NativeRef`) plus
what the driver proves about the content; a revision is only what a conditional effect compares.

Trusting a listing's ETag is therefore a per-connection decision, taken on evidence for that endpoint,
exactly like the other admission capabilities — not a default that a provider is assumed to satisfy. That
decision is `listingETag` in the admission evidence, and it changes nothing about the rule above: the tree
still takes its own revision only from HEAD/GET. What it admits is narrower — the *coordinator* may build
its revision from the ETag a listing already reported instead of heading each listed child, which it does
only after a probe compared the two channels byte for byte on that bucket (multipart and zero-byte markers
included, since those are the ones a provider is most likely to spell differently).

The cost that justifies asking for the evidence: an unproven connection pays one HEAD per listed entry, so
a capture, its consistency re-read and a per-entry check of a 103-entry folder cost roughly 430 requests,
and a folder of a thousand files costs a thousand per pass. Which is also why the flag is opt-in rather than
assumed: on an endpoint where the two spellings disagree, the same shortcut turns every captured entry into
`target_changed` — a wrong answer, not a slow one.

A driver may also implement `storage.Purger`, which is the one deletion that is not a conditional effect: the
caller has already settled the removal in the index, so it skips the receipt and the re-observation `Remove`
performs and issues one conditional delete. It exists for purges only; a mutation whose reply may be lost still
goes through `Remove`.

The object mutation driver derives `Revision` from the current object's ETag and sends every conditional
effect with native `If-Match` / `If-None-Match` where the provider enforces them. Enforcement is a per-connection
measurement, never an assumption: a PUT carrying a stale `If-Match` must be refused (412) before a **direct**
class A replacement may sign that PUT at the destination, because the client's own write is the publication and
the grant outlives the session. COS ignores `If-Match` on PUT, so such a connection routes replacement through
multipart, where the parts stay unpublished until the server completes them and the commit re-checks the ETag
under the gateway lock just before publishing. Folder markers are
normally zero-byte; a marker reasserted during a purge carries a small token instead, because the empty-content
MD5 is identical for every zero-byte marker and a rewrite of identical bytes would not move the revision that a
stale conditional delete compares. Its key still ends in `/`, and `Stat`/listings report the folder with
`Size` zero. It advertises
`AtomicCreate=true` and `AtomicReplace=true` without requiring bucket versioning. It sets `PinnedRead=false` and
`RetainsVersions=false`. Prefix folders without a marker object have nil `Revision`; they may be parents
but cannot be conditionally removed. Baseline/accounting uses the initial matched native/index observation
plus application journal changes and reservations; exact global quota enforcement across external writers
is not claimed.

NativeMover/Copier optimize a known supported operation. Capability failure before effects can fall back to
journaled copy/verify/delete. Failure after possible effects must be reconciled first. A move cannot become a
copy followed by unconditional source deletion; validate the source revision again before deleting it.
NativeCopier must leave an independently usable destination file with fresh service identity; no API-only
reference alias or mutable hardlink. Non-atomic move settlement must not assign one ID to two surviving
independent entries; partial-copy identity is settled under guards using native evidence.

The coordinator uses a verified NativeTreeMove capability for eligible Local folder moves, capturing one
root effect and relinking canonical parent/name without descendant enumeration solely to preserve service IDs.
Eligibility requires native source evidence and a destination prefix no longer in UTF-8 bytes and no deeper
in segments than the source. A growing prefix/depth uses the finite manifest to validate descendant limits.
Generic flat-key moves still need per-entry native effects. Dynamic ancestry/search and derived caches may
scale with candidates, depth or subtree size; service identity is not a universal O(1) claim.

Migration 032 durably binds the service's change stream to a root/configuration namespace. Changing driver
binding is not permission to silently reuse that stream, and the service does not adopt a new root: a bucket,
region, prefix or generation change refuses to start and names both roots. After moving the bytes, operators
stop writers, settle accepted work and run `eam storage-rebind --from BOUND --to CONFIGURED --writers-quiesced`
to move the budget binding and rotate the stream; a same-root restore uses file-changes-reset. Every
A → B → A move is a rebind, whose namespace rotation also makes the old epoch unusable.
Both preserve native bytes and reject pending guards/nonterminal Operations; they do not strengthen native
fencing or recover lost identity mappings. Producer/read/reset lock ordering is owned by the service, not the
driver.

The service publishes confirmed change facts and marker updates during index settlement, after native evidence
is established. A subtree move can be one logical coverage=subtree event without a descendant event per file.
Drivers do not emit speculative success, own public cursor positions or deliver notifications. The service
captures before/after ancestry for privacy projection and recovery; unknown native outcomes retain operation
guards. Indexed repeatable-read change snapshots freeze metadata, not native storage bytes. Explicit scans
can publish observed external updates, but partial enumeration never authorizes deletion. See
[file changes](file-changes.md) for observation, audit and first-phase SSE boundaries.

## 3. Writes and publication

Receiving bytes separately from the final native path is still valuable. A staged file is temporary, not the
permanent representation of committed user data. The final successful file must exist at its original path.

Classes A and B never hand bytes to the driver: the client writes into unpublished provider state and the
driver later verifies and publishes it. Class C stages bytes through the driver. Both end in the same
publication state and the same ledger.

The intent ledger is recovery evidence with a retention, not permanent state. A record whose operation has
settled (`applied`, `not_applied`, `discarded`, or a failed stage that never published) may be removed once
`storage.IntentRetention` has passed; it was kept for the minimum 24-hour window in which a terminal operation
can still answer a retry. A record whose operation is non-terminal — reserved, receiving, ready, staging, executing,
publishing, unknown, or unresolved — is never removed, because its native effect may still land; deletion would
forget a possible effect. Cleanup takes the per-intent lock and rereads the record under it, and removes a
payload before the record that names it, so it can be interrupted and rerun safely.

Admission proves only what it needs. A binding may contain settled records and their permanent per-intent
locks: they cannot hide a future effect, so they do not block an automatic baseline. Any non-terminal record,
or any spool entry the build cannot classify, still refuses admission exactly as an unresolved effect does.
The alternative — requiring an empty spool — made a disposable cache unrebuildable after an index loss, and
only an undocumented manual directory deletion could recover it. `eam storage-ledger-prune --owner-id UUID
[--dry-run]` reclaims expired settled records; admission does not depend on it having run.

```go
package storage

import (
    "context"
    "io"
    "time"
)

type WriteIntent struct {
    ID string // persisted before native effects; never reused for a different payload
    Target Path
    Conditions Conditions
    Size int64
    MediaType string
    Digest *Digest
}

type Staged struct {
    Intent WriteIntent
    Handle []byte // temporary native locator/session; kept server-side
    Size int64
    Digest *Digest // verified only
}

type PublishState string
const (
    StagedReady PublishState = "staged_ready"
    Applied PublishState = "applied"
    NotApplied PublishState = "not_applied"
    OutcomeUnknown PublishState = "outcome_unknown"
)

type Inspection struct {
    State PublishState
    Stage *Staged
    Result *MutationResult
    Evidence []byte
}

// PlannedWriter is the writer for classes A and B. The driver never receives the bytes; it opens
// unpublished provider state, issues the client's grants, and later verifies and publishes it itself.
type PlannedWriter interface {
    PlanWrite(context.Context, WriteIntent, Checkpoint) (TransferPlan, error)
    SignParts(context.Context, WriteIntent, []byte, []int) ([]PartGrant, error) // class A windows/renewals
    InspectWrite(context.Context, WriteIntent, []byte) (Inspection, error)
    CommitWrite(context.Context, WriteIntent, []byte, Checkpoint) (MutationResult, error)
    DiscardWrite(context.Context, WriteIntent, []byte) error
}

// TransferPlan is the class payload handed to the client executor. Handle stays server-side.
type TransferPlan struct {
    Class       string // "presigned-parts" | "resumable-session"
    PartSize    int64  // provider-mandated; the client does not choose
    MaxParallel int
    ExpiresAt   time.Time
    Handle      []byte // native upload/session identity, checkpointed before the plan is returned
    Payload     []byte // class-specific client payload, opaque to the service
}

type PartGrant struct {
    Number    int
    Size      int64
    URL       string
    ExpiresAt time.Time
}

// ReadGranter issues direct read grants for classes A and B.
type ReadGranter interface {
    GrantRead(context.Context, Path, ReadGrantOptions) (ReadGrant, error)
}

type ReadGrantOptions struct {
    MediaType   string
    Disposition string // "attachment" or allowlisted "inline"
}

type ReadGrant struct {
    Kind       string // "url" | "bearer"
    URL        string
    Headers    map[string]string
    ExpiresAt  time.Time
    Conditions []string // only conditions the provider enforces natively
}

// RelayWriter is the writer for class C. Only this class hands the application the bytes.
type RelayWriter interface {
    Stage(context.Context, WriteIntent, io.Reader, Checkpoint) (Staged, error)
    Publish(context.Context, Staged, Checkpoint) (MutationResult, error)
    Inspect(context.Context, WriteIntent, []byte) (Inspection, error)
    Discard(context.Context, WriteIntent, []byte) error
}

// Recovery also covers mkdir/move/copy/remove, not only writes.
type EffectIntent struct {
    ID string
    Action string
    Source Path
    Target Path
    Conditions Conditions
    SourceRevision []byte
}

type EffectRecovery interface {
    InspectEffect(context.Context, EffectIntent, []byte) (Inspection, error)
}
```

Implementation note: the class A driver capability is `storage.PlannedUploadWriter` (`BeginPlannedUpload`,
`SignUploadParts`, `ListUploadParts`, `SealPlannedUpload`, `DiscardPlannedUpload`). The file-operation service
owns the single publishing call and index settlement, so the semantic separation above is preserved while the
driver does not receive bytes. Class C uses the existing `StagedWriter`/`NumberedUploadWriter` relay path. The
class B configuration value exists, but no cloud-drive driver implements it yet.

`PlanWrite` creates the native multipart upload or provider upload session, checkpoints its handle under the intent fence and returns the first grants.
It never receives the bytes. `SignParts` re-signs or extends class A part URLs for the same bound lengths.
`InspectWrite` reports what the provider holds. `CommitWrite` re-establishes the binding, verifies full
coverage at the provider and performs the publishing call itself; a client success response is not evidence.

`Stage` (class C only) writes exact bytes to isolated temporary storage/native state, checks declared size and
any accepted checksum, and returns only durable complete staging. Checkpoint persists native IDs, intermediate
phases and receipt/evidence under the intent fence. Both writers obey the same rule: a retry under one intent
cannot overwrite a different in-flight attempt, and lost allocation handles require
discoverability/expiry/reconciliation rather than an exactly-once allocation claim.

A relay writer stages one entry at a time, so its staging bound is a property of the
work, not of the connection's interactive request policy. A whole-tree copy declares
the size of each entry it is about to hand over through `storage.StagingBound`
(`AdmitStagedBytes`), which raises the writer's single-entry ceiling up to the
provider's one-request publication limit and never lowers it. The 32 MiB default of
`FILE_MAX_SINGLE_PUT_BYTES` describes an interactive `PUT` a person made; using it as
the copy's bound made a backup stop at its first larger file after part of the tree had
already been written. The provider ceiling is not negotiable, so an entry above it is
refused by name (`too_large`) before the attempt rather than inside a native request.
A writer without `StagingBound` keeps its own bound.

Publish changes the actual readable destination using an admitted no-replace/replace operation. Check the
captured target conditions and record final native-effect intent before sending it. Local rename or S3 object
completion may be atomic per file; a provider that exposes partial final bytes cannot satisfy atomic replace
through ordinary direct PUT and must use staging plus a valid publication primitive or reject that capability.
Staging can sometimes reuse same-filesystem temporary files, or a native unpublished multipart upload bound to
the final key. It need not always perform a second full copy, but its visibility/recovery guarantees must hold.

Inspect consumes the latest durable checkpoint/evidence, and must distinguish definite completion, definite
non-application, ready staging and unresolved effects. Existence+size alone cannot prove which writer won or
whether a delayed native request can still arrive. NotApplied is safe to retry/release only when prior native
effects are proven unable to apply later (or native fencing makes them harmless). Otherwise OutcomeUnknown.
A missing multipart upload session is likewise not definite non-application: the provider also removes a
session when `Complete` succeeds, so the destination bound to the operation's own intent marker decides, not
the session's absence.
Applied also requires all outstanding final-path requests/retries to be terminal or fenced before the guard
is released. Seeing the desired file while an older unfenced retry can still arrive is not terminal success;
retain the guard and a nonterminal recovery state even when the visible application is already known.

**A database fence alone cannot fence final-path I/O.** While publication is uncertain, persist an unresolved
path/subtree guard and reject/queue conflicting mutations with operation_in_progress. Lease loss must not
release that guard. Native generation conditions covering every delayed request can provide an alternative;
otherwise require proof of quiescence or explicit operator recovery. This applies equally to delayed Move,
Copy and Remove requests. Unsupported automatic recovery must never be represented as a safe rollback.

### What one folder create costs, and why

A measured `Mkdir` against the COS driver spends about fifteen provider requests: nine `HEAD`s
(three of them after the write), six listings (five of the same parent), and one conditional
`PUT`. That is not all redundancy. The pre-effect check, the durable checkpoint and the revalidation
after it are three different claims ("the parent is still the one I bound", "this operation may
now publish", "the parent and target are still what I admitted"), and each must be made against
the provider by the step that relies on it; a confirmation after the write is what proves the
revision the index records. Merging those across the checkpoint would remove the property the
journal exists to provide.

The observations sit in different phases, not inside one: admission observes the target to freeze
the manifest, the run step re-observes it because a run may execute long after admission, and the
post-effect observation is what confirms the written revision. That is why a first estimate of
"under six requests by removing duplicates inside a phase" was too optimistic: most of the repeats
are these phase boundaries, and merging them would let an admission-time observation stand in for a
run-time one. Before cutting the count, attribute every request to its phase with `COS_TRACE=1`;
a request that genuinely repeats inside one phase is the only safe removal.

Native success followed by index failure leaves real changed files. Recover/index that result from the journal,
return the same recorded client outcome, and settle quota; do not delete a final file just because its database
acknowledgement is missing. After any final effect starts, cancellation is best effort until a definite outcome.
Discard removes temporary resources only, using known checkpoint IDs; it never blindly deletes the Target path.
Discard must be terminal for a numbered upload once the provider definitely accepts the abort: that
answer proves the session existed and is now gone, so no part of it can ever be completed and no
publication from that allocation can still land. A provider answer that the session does not exist is
**not** that proof. COS and S3 remove an upload session when `Complete` succeeds just as they do when an
abort wins, so `NoSuchUpload` cannot distinguish an abort from a publication that landed; discard must
not read it as "nothing applied". It observes the destination and binds an object to this operation's own
intent marker: an attributable object is recorded as published — never deleted, and left for reconcile to
adopt, because a published object with no index row is recoverable while one recorded as `not_applied` is
invisible — and anything unattributable keeps the allocation or the publication record retained. The same
rule governs recovery: a missing session is not definite non-application, and only a destination
observation that attributes the object (or an abort the provider accepted) may settle the publication
`applied` or `not_applied`; otherwise it stays `outcome_unknown`. While the abort outcome cannot be
obtained at all, retain liability and retry; that is a transport problem, not a user-visible file.

A writable connection must supply EffectRecovery (even when its only safe answer is OutcomeUnknown) and a
bounded operational plan for discovering/expiring temporary allocations. Coordinators additionally require
bound writer construction identity for `PlannedWriter` and `RelayWriter` (`StorageBinding`, `StorageNameRules`)
together with `BoundEffectRecovery`, and reject a mismatched owner/connection/generation/root before native I/O;
the identity must come from the actual admitted driver, with credentials excluded. Trusted action callbacks must
use that same bound executor.
List/scan content and staging areas
is an operational requirement, with inventory extensions where useful. Inventory does not itself prove garbage
is safe to delete: a restored database may be behind real storage, and old native requests may still be active.

## 4. Transfer classes

A driver lists the classes it can serve in `Descriptor.TransferClasses`; the connection selects one when it is
configured. [Transfer protocol](transfer-protocol.md) owns the class definitions, endpoint shapes, geometry,
provider rules, signing rules and deployment requirements; a driver supplies only what the protocol asks for.

A driver serving class A or B must provide `PlannedWriter` and `ReadGranter` and meet these rules:

- **Server commit.** Class A keeps bytes in an unpublished native multipart upload, so only `CompleteMultipartUpload`
  publishes. Class B sessions are created at the destination and the server completes them (OneDrive deferred
  commit, Box commit, Dropbox finish; Google Drive completes on the final chunk). Because the client holds what
  could also complete a class-B session, server-only publication there is a client contract; a file that
  appears without a commit is treated as an outside change and picked up by ordinary reconcile.
- **Queryable.** `InspectWrite` must report what the provider actually holds (part listing, session range or
  offset). Client writes therefore never create an ambiguous final-path effect; only `CommitWrite` can, and it
  follows the ordinary §3 guard and recovery rules.
- **Conflict rules at session creation.** Create fails on an existing name; replace targets the existing provider
  file with its native revision condition where one exists. A driver without an atomic no-replace create
  (Google Drive allows duplicate names) sets `AtomicCreate=false`, which capabilities expose.
- **Scoped grants.** Part URLs bind one part of one native upload with its exact length and no digest. Read
  grants bind one file. A provider credential that reaches beyond one file or session is reported as such so the
  service can refuse it to narrower principals.

The staged/publish/recovery semantics of §3 are shared by every class. A class changes only how bytes arrive;
it never changes what a committed file means or how an uncertain final-path effect is fenced. Drivers do not emit
speculative success, own public cursor positions or deliver notifications. A connection never changes class per
transfer: an unavailable direct route fails rather than relaying bytes.

Receipts state what actually verified. A provider digest, a provider-listed length and a client assertion are
different evidence and must be labelled separately; never relabel a provider ETag as SHA-256 or claim a
verification the provider did not perform.

Grants can survive client cancellation or revocation until their actual expiry, and provider requests may
outlive it; because no grant can publish, cleanup only discards unpublished provider state and retains
liability until that discard is confirmed.

## 5. Backend mappings and conformance

| Backend | Native readable representation | Main effects/constraints |
|---|---|---|
| Local filesystem | Ordinary relative folders/files | Stage in same filesystem; atomic rename/no-replace where supported; fsync file and containing directories; root/symlink isolation; cross-volume moves need fallback. A file's revision is its inode identity plus size/mtime/mode (rename-stable, so a move preserves it) and observation reads no bytes; an in-place rewrite that restores mtime on the same inode is invisible to it, which the coordinated-exclusive writer model already excludes |
| S3/COS | Original path keys below owner prefix; standard empty-folder markers (a reasserted marker carries a small service token so its ETag moves) | Single-object publish can be atomic; provider-side copy (one conditional request, or conditional part copies above 5 GiB) keeps bytes inside the provider; folder move is still per-entry copy/verify/delete, not metadata-only |
| WebDAV | Native folders and original names | Stream to temp then native MOVE if its semantics suffice; plain final PUT can expose partial data; ETags/atomic conditions vary |
| Cloud drive | Real folders/names, native IDs used internally | Duplicate names and unsupported characters require checks; native move/replace/version guarantees vary; upload sessions/credentials expire independently |
| NAS/Synology | Native filesystem, WebDAV, or vendor API tree | Prefer native rename and LAN byte path; connector cannot invent remote atomicity |

Tests must prove direct exit and no-database scanning as well as CRUD: original names/bytes, case/normalization
collisions, empty folders, large listings, owner isolation, ranges/version checks, native copy independence,
partial folder mutations, lost publication responses, stale delayed final writes, unknown-outcome blocking,
cleanup after database rollback, and native-success/index-failure recovery. Never remove ordinary user files
as garbage just because an index is missing. Tests and capabilities must describe the actual writer model.
