# Storage driver contract

> Native-tree contract, 2026-10-30. P1 read-only Local/COS/S3 trees are implemented;
> mutation/publication interfaces and an opt-in Local create-only writer are implemented in P3.
> P4 adds admitted Local actions and conservative COS/S3 staged adapters with explicit capability evidence.
> Real COS/S3 probes remain unverified; default writes stay disabled. Preserving the readable tree is mandatory.
> Implementation/configuration limits: [P1](storage-p1.md), [P3](storage-p3.md), [P4](storage-p4.md),
> [object mutation admission](storage-p4-objects.md), [P5 durable ingress](storage-p5.md) and
> [native multipart/receiver deployment](native-upload-deployment.md). Local parts use durable local staging;
> admitted COS/S3 parts use unpublished native multipart, independent of small content-PUT policy.
>
> [Architecture](storage.md) requires original names, complete native folder structure and directly usable
> file bytes. [The file API](../api/api.md) remains same-origin and backend-neutral. This replaces the
> anonymous Blob/Open/Put/Probe/Drop design; IDs may be native locators, never the only stored filenames.

## 1. Boundary and path model

Each owner is bound to a content root and a separate system/staging root. Paths below the content root map
to the same relative native folder/name structure. S3 uses ordinary prefix keys and empty-folder markers;
cloud-drive adapters resolve paths to native IDs internally while preserving actual folders/names. A known
native ID can accelerate lookup but cannot replace the visible destination tree.

The API owns access control, index/search, private database keys, quota and durable operation journals.
All clients address files/folders by path at the same API; neither database keys nor native IDs are public
file handles. Drivers implement real
file operations, native authentication, naming constraints, version evidence and backend recovery. Drivers
must enumerate their content root, including existing files, so indexing and direct migration need no data
rewrite. They do not own a separate application database or expose provider URLs to clients.

The interfaces below are intended Go declarations. Root bindings can be cheap views over shared pooled
clients; do not allocate a separate HTTP pool permanently for every user. Configuration generation identifies
the actual backend/root; credential rotation alone need not change that identity. Use typed options per adapter
at construction, validate them there, and never construct provider clients in an HTTP handler.

## 2. File and folder interfaces

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
    Version    []byte // native change/pinning evidence, opaque and possibly unavailable
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
    Version     []byte // requested native version/validator, not an application revision
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
    ExpectedVersion []byte // conditional mutation of a native revision, exclusive with Absent
}

type Mutation struct {
    ID string // durable unique intent; not a claim of native idempotency
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

type Remover interface {
    Remove(context.Context, Mutation, Checkpoint) (MutationResult, error)
    // Removes a file or an empty folder only. The service must explicitly enumerate
    // and journal recursive deletion; this call never recurses implicitly.
}

type MoveRequest struct {
    ID string
    Source Path
    Target Path
    SourceVersion []byte
    TargetConditions Conditions
}

type NativeMover interface {
    Move(context.Context, MoveRequest, Checkpoint) (MutationResult, error)
}

type NativeCopier interface {
    Copy(context.Context, MoveRequest, Checkpoint) (MutationResult, error)
}

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

P4 may add a private `ContentIdentity` to Entry when the admitted driver proves a stable native identity
independent of pathname. It does not expose a public file ID. Evidence-bound application content-generation
mappings may retain contentRevision across a proven copy/delete move, but must reject a changed native
fingerprint. WriteIntent additionally captures optional source modification time, original client condition,
and parent path/version so recovery cannot retarget a replaced ancestor.

Missing entries return a typed not_found error; not nil success. List is mandatory even on read-only drivers.
The service recurses paginated child listings or uses an optional inventory optimization. Native IDs/versions
remain private and are not file contents or public app IDs. Read callers must close Body and handle context
cancellation. The driver must not silently serve a different version or ignore a requested byte range; it
returns changed/unsupported and the API chooses an explicit fallback. Version pinning may use native versions,
conditional reads, or coordinated exclusion of conflicting application mutations for the read duration.

NameRules are not enough to describe every provider restriction: adapters also validate all reserved names,
normalization collisions, punctuation and trailing-character rules before mutation. Unsupported names return
invalid_name/name_conflict, never an anonymous or escaped native filename. Duplicate native names that make a
path ambiguous return ambiguous_path. Any destination conversion during export to a different filesystem must
be explicit, reversible when offered, and must not silently overwrite.

AtomicCreate/AtomicReplace describe tested single-file native visibility, not arbitrary directory transactions.
NativeConditionalMutation describes real backend-enforced version/absence checks, not Stat then write.
Conditions that cannot be implemented with native support or a declared coordinated-exclusive writer model
return unsupported before an unsafe effect. Conflicting external writes are outside an application lock;
do not claim they are fenced. An atomic no-replace primitive is needed to guarantee create-only against them.
A native MOVE that is atomic only within a filesystem must not be advertised as atomic across volumes.

NativeMover/Copier optimize a known supported operation. Capability failure before effects can fall back to
journaled copy/verify/delete. Failure after possible effects must be reconciled first. A move cannot become a
copy followed by unconditional source deletion; validate the source revision again before deleting it.
NativeCopier must leave an independently usable destination file; no API-only reference alias or mutable hardlink.

## 3. Staged writes and publication

Receiving bytes separately from the final native path is still valuable. A staged file is temporary, not the
permanent representation of committed user data. The final successful file must exist at its original path.

```go
package storage

import (
    "context"
    "io"
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

type StagedWriter interface {
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
    SourceVersion []byte
}

type EffectRecovery interface {
    InspectEffect(context.Context, EffectIntent, []byte) (Inspection, error)
}
```

Stage writes exact bytes to isolated temporary storage/native state, checks declared size and any accepted
checksum, and returns only durable complete staging. Checkpoint persists native IDs, intermediate phases and
receipt/evidence under the intent fence. Stage retries cannot overwrite a different in-flight attempt. Lost
allocation handles require discoverability/expiry/reconciliation, not an exactly-once allocation claim.

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
Applied also requires all outstanding final-path requests/retries to be terminal or fenced before the guard
is released. Seeing the desired file while an older unfenced retry can still arrive is not terminal success;
retain the guard and a nonterminal recovery state even when the visible application is already known.

**A database fence alone cannot fence final-path I/O.** While publication is uncertain, persist an unresolved
path/subtree guard and reject/queue conflicting mutations with operation_in_progress. Lease loss must not
release that guard. Native generation conditions covering every delayed request can provide an alternative;
otherwise require proof of quiescence or explicit operator recovery. This applies equally to delayed Move,
Copy and Remove requests. Unsupported automatic recovery must never be represented as a safe rollback.

Native success followed by index failure leaves real changed files. Recover/index that result from the journal,
return the same recorded client outcome, and settle quota; do not delete a final file just because its database
acknowledgement is missing. After any final effect starts, cancellation is best effort until a definite outcome.
Discard removes temporary resources only, using known checkpoint IDs; it never blindly deletes the Target path.

A writable connection must supply EffectRecovery (even when its only safe answer is OutcomeUnknown) and a
bounded operational plan for discovering/expiring temporary allocations. P3 coordinators additionally require
`BoundStagedWriter`/`BoundEffectRecovery` construction identity (`StorageBinding`, `StorageNameRules`) and
reject a mismatched owner/connection/generation/root before native I/O; the identity must come from the
actual admitted driver, with credentials excluded. Trusted action callbacks must use that same bound executor.
List/scan content and staging areas
is an operational requirement, with inventory extensions where useful. Inventory does not itself prove garbage
is safe to delete: a restored database may be behind real storage, and old native requests may still be active.

## 4. Optional native upload acceleration

Keep the public numbered-part protocol unchanged. Internally a native upload extension may offer
Begin / Query / WritePart / SealStage / Abort with versioned opaque state and checkpoint updates on every
state-changing step. SealStage produces a Staged result, not an anonymous committed object. Publication must
still satisfy §3 and end at the intended readable native path.

Native multipart may stream to the final key while unpublished; native offset providers may upload to a
sibling temporary file then rename. If the provider cannot separate byte reception from safe final visibility,
use durable gateway spool or decline the requested write semantics. Public nextPart/concurrency/part geometry
must fit native alignment/count/ingress limits. ACK only durable bytes and durable service receipts.

Retain per-part server hashes; don't infer them from opaque native ETags. Different payloads cannot race into
the same native slot; bind identity before unsafe native writes or isolate/abandon the native upload. Verify a
chunk before irreversible offset append if native checksum rejection is not atomic. Query half-written offsets
after ambiguity and keep bounded spool as needed. Credential refresh does not extend native session retention;
cap public expiry or retain enough data to recreate it. Explicitly fail unrecoverable native session loss.

The official COS delivery uses signed UploadPart requests carried through EdgeOne ordinary forwarding. The
application binds size, client SHA-256 assertion and Content-MD5 before issuing a short-lived exact request;
only UploadPart is authorized, never completion, deletion or arbitrary writes. The browser receives an
origin-relative transfer URL and required headers, with no signing secret or broad temporary credentials.
COS verifies the signature and Content-MD5; the application independently lists native parts and checks exact
size and admitted ETag=MD5 semantics before accepting the service receipt. The client SHA-256 remains an
assertion, not an application-computed checksum. Receipts expose checksumSource accordingly. Do not relabel
COS ETags as SHA-256 or claim cryptographic verification that the provider did not perform.

Self-hosted embedded reception computes SHA-256 while receiving and keeps the existing native multipart or
Local staging path. Both transports retain the same immutable service-part identity and staged publication
journal. Signed grants can survive client cancellation/revocation until their actual expiry, and provider
requests may outlive it; cleanup retains unresolved physical liability and final-path guards as required.
Complete is still server-controlled and native success is required before index/result settlement. No full
file is downloaded or assembled at commit. Native allocation and part metadata are owner/root-bound database
records, while the application's final-publication ledger retains its admitted durability requirements.

Other provider adapters remain optional future integrations. Do not introduce a universal offset/token
abstraction merely to anticipate them, or expose broad STS credentials. The user-confirmed same-origin signed
part transport supersedes earlier blanket no-presigning/no-native-parameter language for official COS only.

## 5. Backend mappings and conformance

| Backend | Native readable representation | Main effects/constraints |
|---|---|---|
| Local filesystem | Ordinary relative folders/files | Stage in same filesystem; atomic rename/no-replace where supported; fsync file and containing directories; root/symlink isolation; cross-volume moves need fallback |
| S3/COS | Original path keys below owner prefix; standard empty-folder markers | Single-object publish can be atomic; verify conditional finalization support per operation; folder move is paginated copy/verify/delete, not metadata-only |
| WebDAV | Native folders and original names | Stream to temp then native MOVE if its semantics suffice; plain final PUT can expose partial data; ETags/atomic conditions vary |
| Cloud drive | Real folders/names, native IDs used internally | Duplicate names and unsupported characters require checks; native move/replace/version guarantees vary; upload sessions/credentials expire independently |
| NAS/Synology | Native filesystem, WebDAV, or vendor API tree | Prefer native rename and LAN byte path; connector cannot invent remote atomicity |

Tests must prove direct exit and no-database scanning as well as CRUD: original names/bytes, case/normalization
collisions, empty folders, large listings, owner isolation, ranges/version checks, native copy independence,
partial folder mutations, lost publication responses, stale delayed final writes, unknown-outcome blocking,
cleanup after database rollback, and native-success/index-failure recovery. Never remove ordinary user files
as garbage just because an index is missing. Tests and capabilities must describe the actual writer model.
