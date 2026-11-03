// Package fileops coordinates native mutations through durable intent, guards and
// explicit recovery. It is not connected to public write routes before P4/P5.
package fileops

import (
	"context"
	"encoding/json"
	"errors"
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
type Actor struct {
	ID          string // stable creator principal, e.g. owner:<UUID> or token:<private-token-id>
	Scope       storage.Path
	CanRead     bool
	CanWrite    bool
	ManageOwner bool // explicit first-party owner recovery; not implicit shared-owner token access
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
	SourceVersion    []byte
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
	ID                 string
	Root               Root
	PrincipalID        string
	Kind               string
	State              State
	Phase              Phase
	Intent             json.RawMessage
	Fingerprint        string
	Checkpoint         []byte
	RecoveryEvidence   []byte
	Stage              *storage.Staged
	NativeResult       *storage.MutationResult
	Result             json.RawMessage
	Problem            json.RawMessage
	CancelRequested    bool
	Guards             []Guard
	Entries            []EntryEffect
	CreatedAt          time.Time
	FinishedAt         *time.Time
	RetainUntil        time.Time
	Reservation        ReservationRequest
	CleanedAt          *time.Time
	CleanupAttemptedAt *time.Time
	ChargedLogical     int64
	ChargedPhysical    int64
	Lease              Lease
}

func (o Operation) Terminal() bool {
	return o.State == Succeeded || o.State == Failed || o.State == Cancelled
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
}

func (e *Error) Error() string           { return "file operation " + e.Code }
func (e *Error) Unwrap() error           { return e.Cause }
func E(code string, cause error) error   { return &Error{Code: code, Cause: cause} }
func IsCode(err error, code string) bool { var e *Error; return errors.As(err, &e) && e.Code == code }

// Authorize rechecks credential validity/current captured path access before native
// effects and recovery. The journal alone cannot know token revocation/ancestry.
type Authorize func(context.Context, Operation) error
