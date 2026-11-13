// Package fileops coordinates native mutations through durable intent, guards and
// explicit recovery. It is not connected to public write routes before P4/P5.
package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

type Root struct {
	OwnerID         string
	ConnectionID    string
	Generation      int64
	ContentRoot     string
	SystemRoot      string
	CaseSensitive   bool
	RetainsVersions bool // native delete/replace retains previous object generations
}

// Namespace is the native-tree identity a change stream and its content
// generations are bound to. It covers the connection digest, generation and both
// roots, so a bucket, region, prefix or generation change produces a new
// namespace instead of silently continuing an old one.
func (r Root) Namespace() string {
	return fmt.Sprintf("%s:%s:%d:%s:%s", r.OwnerID, r.ConnectionID, r.Generation, r.ContentRoot, r.SystemRoot)
}

type Actor struct {
	Label         string `json:",omitempty"`
	Kind          string `json:",omitempty"`
	PolicyVersion int64  `json:",omitempty"`
	CanChanges    bool   `json:",omitempty"`
	CanSubscribe  bool   `json:",omitempty"`
	CanEvents     bool   `json:",omitempty"`
	RequestID     string `json:",omitempty"`
	ID            string // stable creator principal, e.g. owner:<UUID> or token:<private-token-id>
	Scope         storage.Path
	CanRead       bool
	CanWrite      bool
	ManageOwner   bool // explicit first-party owner recovery; not implicit shared-owner token access
}
type Guard struct {
	Path    storage.Path
	Subtree bool
}
type ManifestEntry struct {
	Number           int
	Action           string
	Source           storage.Path
	Target           storage.Path
	SourceRevision   []byte
	SourceEntry      *storage.Entry
	TargetConditions storage.Conditions
	Size             int64
}
type ReservationRequest struct {
	NewBytes      int64
	OldBytes      int64
	CreditPath    storage.Path
	CreditVersion []byte
	PhysicalBytes int64
}
type BeginRequest struct {
	Root           Root
	Actor          Actor
	Kind           string
	IdempotencyKey string
	Intent         json.RawMessage
	Guards         []Guard
	Manifest       []ManifestEntry
	Reservation    ReservationRequest
	RetainFor      time.Duration
}
type State string

const (
	Queued    State = "queued"
	Running   State = "running"
	Unknown   State = "outcome_unknown"
	Succeeded State = "succeeded"
	Failed    State = "failed"
	Cancelled State = "cancelled"
)

type Phase string

const (
	Admitted      Phase = "admitted"
	Receiving     Phase = "receiving"
	StagedReady   Phase = "staged_ready"
	EffectStarted Phase = "effect_started"
	NativeApplied Phase = "native_applied"
	Settled       Phase = "settled"
)

type Lease struct {
	OperationID string
	Token       string
	Epoch       int64
	ExpiresAt   time.Time
}
type EntryEffect struct {
	ManifestEntry
	State      string // pending, started, applied, not_applied, outcome_unknown
	Checkpoint []byte
	Evidence   []byte
	Result     json.RawMessage
}
type Operation struct {
	Attribution      Actor `json:",omitempty"`
	ID               string
	Root             Root
	PrincipalID      string
	Kind             string
	State            State
	Phase            Phase
	Intent           json.RawMessage
	Fingerprint      string
	Checkpoint       []byte
	RecoveryEvidence []byte
	Stage            *storage.Staged
	NativeResult     *storage.MutationResult
	Result           json.RawMessage
	Problem          json.RawMessage
	CancelRequested  bool
	Guards           []Guard
	// Entries is the manifest's per-entry state. For an operation with
	// EntriesLayoutTable it is not part of the operation document: the rows in
	// file_operation_entries are the record, and a reader that needs the list asks
	// for it. Settlement is the one reader that must ask inside its own transaction
	// (docs/design/journal-entries.md §2.3), and it fills this field on the copy it
	// commits with. saveOperation pins Entries to null for a table-backed row, so
	// this field never puts the list back into that document; `omitempty` keeps the key
	// out of a table-backed document entirely, and a document-backed row that has an
	// empty list has always been equivalent to one without the key. The field stays
	// decodable because every row written before Release B spells its list here and
	// must keep reading until the last one is pruned.
	Entries []EntryEffect `json:",omitempty"`
	// EntriesLayout is the durable layout of this operation's entries (sql/049).
	// It is read from the parent row, never serialized into the document, and it is
	// immutable after the manifest freezes.
	EntriesLayout EntriesLayout `json:"-"`
	// EntryCount is the number of manifest entries, set once at freeze. It is
	// authoritative only for EntriesLayoutTable; a document-backed row's count is
	// the document's list. Progress reports it as `total` instead of loading rows.
	EntryCount int `json:"-"`
	// EntryCompleted is how many entries are decided (applied or not_applied). It is
	// filled by the progress read only, never by a transition.
	EntryCompleted     int `json:"-"`
	CreatedAt          time.Time
	FinishedAt         *time.Time
	RetainUntil        time.Time
	Reservation        ReservationRequest
	CleanedAt          *time.Time
	CleanupAttemptedAt *time.Time
	ChargedLogical     int64
	ChargedPhysical    int64
	Lease              Lease
	// Purge marks a recursive deletion whose index settlement already happened: the
	// subtree left the index when the deletion was accepted, and what remains is the
	// native purge of bytes. It carries what the purge needs to decide, in one place,
	// because a re-walk after a restart must reach the same answer as the first pass.
	Purge *PurgeState `json:",omitempty"`
}

// PurgeState is the durable intent of an accepted prefix deletion. The index rows are
// already gone; this is the only surviving description of what still has to leave the
// provider, and it is deliberately not a per-object manifest: a re-walk is idempotent,
// and an object that appeared after AcceptedAt belongs to somebody else's newer intent.
type PurgeState struct {
	Path       storage.Path
	AcceptedAt time.Time
	// Deleted and Adopted are cumulative over the whole purge, not over one attempt: a
	// retry resumes from them instead of restarting at zero, because a counter that
	// starts over is the one shape a client cannot tell apart from a stall.
	Deleted int64
	Adopted int64
	// Total is the denominator the purge is working against: what was settled when the
	// current walk began plus the objects that walk found. It is persisted for the same
	// reason the counters are, so a client and a restarted worker divide by the same
	// number. It shrinks between walks (one that dies after deleting objects leaves a
	// smaller tree to count) but never falls below what is already settled. Zero means no
	// walk has looked at the prefix yet, which is the only moment the extent is unknown.
	Total int64
}

// PrefixDeleteCommit settles one accepted prefix deletion inside the accepting
// transaction: it removes the subtree's index rows, records the change facts and
// returns the bytes it removed so the journal can refund them in the same commit.
// Returning without error means the user's deletion is final.
//
// It returns no result document on purpose. The operation's `result` is its answer, and
// for a deletion the answer is only true once the bytes are gone; publishing a settled
// `{deleted:true}` at acceptance made a running operation look finished, which is exactly
// the confusion a client has to be able to rule out. The purge writes the result when it
// settles.
type PrefixDeleteCommit func(context.Context, pgx.Tx, Operation) (int64, error)

// LivePaths reports which of these paths the index still holds as live entries. The
// purge uses it to leave alone anything a user re-created after accepting the
// deletion: the provider's own clock cannot be trusted to tell those apart.
type LivePaths func(context.Context, Root, []storage.Path) ([]bool, error)

func (o Operation) Terminal() bool {
	return o.State == Succeeded || o.State == Failed || o.State == Cancelled
}

// EntriesLayout names where an operation's manifest entries live. It is the one
// switch every reader of the list must branch on, never "does the document happen
// to carry Entries": a row's layout is durable, the document's shape is not a fact
// about the operation (docs/design/journal-entries.md §2.1).
//
// Forward-only: a binary from before Release B reads only EntriesLayoutDocument, so
// an operation carrying EntriesLayoutTable is invisible to it and can never settle
// (§2.6). There is no runtime downgrade path; the drain alternative is named in that
// section, and the column comment from sql/051 says the same thing to an operator who
// is about to redeploy an older commit.
type EntriesLayout string

const (
	// EntriesLayoutDocument: entries are in the operation document's Entries key.
	// Every row written before Release B has this layout, including terminal rows
	// that are never upgraded; they are pruned with their document.
	EntriesLayoutDocument EntriesLayout = "document"
	// EntriesLayoutTable: entries are in file_operation_entries and the document
	// carries none.
	EntriesLayoutTable EntriesLayout = "table"
)

// HasEntries reports whether the frozen manifest has any entry. It answers the
// question every "is this manifest empty?" check asks, from the durable layout
// rather than from the shape of a document a reader may not have loaded.
func (o Operation) HasEntries() bool {
	if o.EntriesLayout == EntriesLayoutTable {
		return o.EntryCount > 0
	}
	return len(o.Entries) > 0
}

// EntryTotal is the manifest size, from `entry_count` for a table-backed row and
// from the list for a document-backed one. It is the number a progress report shows
// as `total`, and the reason a status read never loads the table.
func (o Operation) EntryTotal() int {
	if o.EntriesLayout == EntriesLayoutTable {
		return o.EntryCount
	}
	return len(o.Entries)
}

// Baseline is trusted operator evidence, never an automatic index SUM/backfill.
// Broad mutation roots require drained native writers. Create-only roots admit a
// compared observation plus subsequent application charges. External changes do
// not update these counters; index synchronization is a separate operation.
type Baseline struct {
	LogicalBytes         int64
	PhysicalBytes        int64
	LogicalLimit         int64 // 0 means unlimited
	PhysicalLimit        int64 // 0 means unlimited
	NativeComplete       bool
	IndexCompared        bool
	LegacyWritersDrained bool
	Evidence             json.RawMessage
}
type Usage struct{ LogicalBytes, LogicalReserved, PhysicalBytes, PhysicalReserved, LogicalLimit, PhysicalLimit int64 }

// Settlement is exact known native liability for the captured finite intent.
// Unresolved liabilities cannot be released. Proven effects are settled once.
type Settlement struct {
	LogicalDelta  int64
	PhysicalDelta int64
	KeepLogical   int64
	KeepPhysical  int64
	Result        json.RawMessage
	Problem       json.RawMessage
	State         State
}
type IndexCommit func(context.Context, pgx.Tx, Operation, *storage.MutationResult) (json.RawMessage, error)

// Journal's methods must each persist their transition before returning. Lease
// expiry never removes guards or proves native effects terminal.
type Journal interface {
	Begin(context.Context, BeginRequest) (Operation, bool, error)
	Get(context.Context, Root, Actor, string) (Operation, error)
	Claim(context.Context, Root, Actor, string, time.Duration) (Operation, error)
	Renew(context.Context, Lease, time.Duration) (Lease, error)
	Release(context.Context, Lease) error
	SaveCheckpoint(context.Context, Lease, []byte) error
	SaveStage(context.Context, Lease, storage.Staged) error
	StartEffect(context.Context, Lease) error
	SaveApplied(context.Context, Lease, storage.MutationResult) error
	MarkUnknown(context.Context, Lease, []byte) error
	FinishWithoutEffect(context.Context, Lease, State, json.RawMessage) error
	ResolveNotApplied(context.Context, Lease, State, []byte, json.RawMessage) error
	Settle(context.Context, Lease, Settlement, IndexCommit) (Operation, error)
	RequestCancel(context.Context, Root, Actor, string) (Operation, error)
	BeginEntry(context.Context, Lease, int) error
	SaveEntry(context.Context, Lease, int, string, []byte, []byte, json.RawMessage) error
}

type EntryCharge struct {
	Number        int
	LogicalDelta  int64
	PhysicalDelta int64
}
type PartialJournal interface {
	AccountEntry(context.Context, Lease, EntryCharge) (Operation, error)
}

type CleanupJournal interface {
	CleanupCandidates(context.Context, int) ([]Operation, error)
	MarkCleaned(context.Context, Root, Actor, string) error
}

type Error struct {
	Code  string
	Cause error
	// OperationID identifies the existing owner-scoped guard, never a bypass of its read authorization.
	OperationID string
	// OperationKind names that guard's action (upload_commit, delete, move, copy, mkdir) so a
	// refusal can say what is holding the path instead of only which code it is.
	OperationKind string
}

func (e *Error) Error() string           { return "file operation " + e.Code }
func (e *Error) Unwrap() error           { return e.Cause }
func E(code string, cause error) error   { return &Error{Code: code, Cause: cause} }
func IsCode(err error, code string) bool { var e *Error; return errors.As(err, &e) && e.Code == code }

// Authorize rechecks credential validity/current captured path access before native
// effects and recovery. The journal alone cannot know token revocation/ancestry.
type Authorize func(context.Context, Operation) error
