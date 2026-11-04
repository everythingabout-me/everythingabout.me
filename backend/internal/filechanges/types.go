// Package filechanges persists confirmed index transitions and access audit in
// the caller's settlement transaction. It never performs native I/O. Raw storage
// rows and positions are internal; authenticated APIs must use scoped projection
// and opaque cursors rather than serialize Change or Head directly.
package filechanges

import (
	"context"
	"errors"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"github.com/jackc/pgx/v5"
)

const DefaultRetention = 90 * 24 * time.Hour

var (
	ErrInvalidChange  = errors.New("invalid file change")
	ErrEffectConflict = errors.New("file change effect key has different intent")
	ErrInvalidAudit   = errors.New("invalid file audit")
	ErrRootChanged    = errors.New("file change root binding changed")
	ErrRootBusy       = errors.New("file change root has unfinished operations or guards")
)

// Queryer is implemented by pgx.Tx and pgxpool.Pool. Reads always require an owner.
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Head is a committed stream boundary, not a public cursor. RetainedAfter is the
// last removed position; a resume position below it requires a new baseline.
type Head struct {
	OwnerID       string    `json:"-"`
	RootEpoch     string    `json:"-"`
	RootNamespace string    `json:"-"`
	Position      int64     `json:"-"`
	RetainedAfter int64     `json:"-"`
	RetainedSince time.Time `json:"-"`
}

// Actor contains identifiers and a captured policy, never credential material,
// request headers, provider state, raw error bodies or file contents.
type Actor struct {
	Kind             string  `json:"kind"`
	PrincipalID      string  `json:"principalId,omitempty"`
	AppID            string  `json:"appId,omitempty"`
	GrantID          string  `json:"grantId,omitempty"`
	Label            string  `json:"label,omitempty"`
	Scope            fc.Path `json:"scope,omitempty"`
	CanRead          bool    `json:"canRead,omitempty"`
	CanWrite         bool    `json:"canWrite,omitempty"`
	CanChanges       bool    `json:"canChanges,omitempty"`
	CanSubscribe     bool    `json:"canSubscribe,omitempty"`
	CanEvents        bool    `json:"canEvents,omitempty"`
	PolicyGeneration string  `json:"policyGeneration,omitempty"`
}

// Ancestor is captured root-to-parent ancestry at effect settlement. Retention
// does not depend on any ancestor or entry remaining in the live nodes table.
type Ancestor struct {
	ID   string  `json:"id"`
	Path fc.Path `json:"path"`
}

type Change struct {
	ID              string        `json:"id"`
	OwnerID         string        `json:"-"`
	Position        int64         `json:"-"`
	EffectKey       string        `json:"-"`
	Kind            string        `json:"kind"`
	Coverage        string        `json:"coverage"`
	EntryID         *string       `json:"entryId"`
	Before          *fc.Metadata  `json:"before"`
	After           *fc.Metadata  `json:"after"`
	BeforeAncestors []Ancestor    `json:"-"`
	AfterAncestors  []Ancestor    `json:"-"`
	AffectedFolders []fc.Metadata `json:"-"`
	// ImpactPaths are locations to refresh, not a list of every changed ancestor
	// aggregate. Only invalidations match these in ReadScoped.
	ImpactPaths []fc.Path `json:"-"`
	Actor       Actor     `json:"-"`
	OperationID string    `json:"-"`
	RequestID   string    `json:"-"`
	Origin      string    `json:"origin"`
	ObservedAt  time.Time `json:"observedAt"`
	RecordedAt  time.Time `json:"-"`
	ExpiresAt   time.Time `json:"-"`
}

// Projection exposes only authorized endpoints; actor and internal progress are
// intentionally absent. A one-sided move becomes remove/upsert. An ancestor
// subtree effect becomes a bounded invalidation instead of exposing hidden IDs.
type Projection struct {
	ID              string        `json:"id"`
	Type            string        `json:"type"`
	Coverage        string        `json:"coverage"`
	EntryID         *string       `json:"entryId,omitempty"`
	Before          *fc.Metadata  `json:"before,omitempty"`
	After           *fc.Metadata  `json:"after,omitempty"`
	Path            fc.Path       `json:"path,omitempty"`
	ImpactPaths     []fc.Path     `json:"impactPaths,omitempty"`
	AffectedFolders []fc.Metadata `json:"affectedFolders,omitempty"`
	Origin          string        `json:"origin"`
	ObservedAt      time.Time     `json:"observedAt"`
}

type Marker struct {
	OwnerID       string    `json:"-"`
	EntryID       string    `json:"entryId"`
	Position      int64     `json:"-"`
	Token         string    `json:"changeToken"`
	LastChangedAt time.Time `json:"lastChangedAt"`
}

// Audit records access/attempt/outcome separately from successful file changes.
// EffectKey is optional for a request, but supplied for idempotent worker effects.
type Audit struct {
	ID        string `json:"id"`
	OwnerID   string `json:"-"`
	EffectKey string `json:"-"`
	Action    string `json:"action"`
	Outcome   string `json:"outcome"`
	Allowed   *bool  `json:"allowed"`
	Status    int    `json:"status"`
	// BytesWritten is nil at admission; completion records exact server-written bytes, including zero.
	BytesWritten *int64    `json:"bytesWritten,omitempty"`
	ReasonCode   string    `json:"reasonCode,omitempty"`
	EntryID      *string   `json:"entryId,omitempty"`
	Path         *fc.Path  `json:"path,omitempty"`
	Actor        Actor     `json:"actor"`
	OperationID  string    `json:"operationId,omitempty"`
	RequestID    string    `json:"requestId,omitempty"`
	Origin       string    `json:"origin"`
	ObservedAt   time.Time `json:"observedAt"`
}
