package storage

import (
	"context"
	"io"
	"time"
)

// Conditions must be enforced at the native effect. Stat followed by an
// unconditional mutation is not a native conditional implementation.
type Conditions struct {
	Absent bool
	// ExpectedRevision requires the native state to still be this exact revision.
	ExpectedRevision []byte
}
type Mutation struct {
	ID         string
	Path       Path
	Conditions Conditions
}
type MutationResult struct {
	Entry    *Entry
	Evidence []byte
}
type Checkpoint func(context.Context, []byte) error

type WriteIntent struct {
	EntryID          string `json:",omitempty"`
	ParentID         string `json:",omitempty"`
	Selection        string `json:",omitempty"`
	ID               string
	Target           Path
	Conditions       Conditions
	Size             int64
	MediaType        string
	SourceModifiedAt *time.Time
	ClientCondition  string
	ParentPath       Path
	ParentRevision   []byte
	Digest           *Digest
}
type Staged struct {
	Intent WriteIntent
	Handle []byte
	Size   int64
	Digest *Digest
}
type PublishState string

const (
	StagedReady    PublishState = "staged_ready"
	Applied        PublishState = "applied"
	NotApplied     PublishState = "not_applied"
	OutcomeUnknown PublishState = "outcome_unknown"
)

type Inspection struct {
	State    PublishState
	Stage    *Staged
	Result   *MutationResult
	Evidence []byte
}
type StagedWriter interface {
	Stage(context.Context, WriteIntent, io.Reader, Checkpoint) (Staged, error)
	Publish(context.Context, Staged, Checkpoint) (MutationResult, error)
	Inspect(context.Context, WriteIntent, []byte) (Inspection, error)
	Discard(context.Context, WriteIntent, []byte) error
}

// BoundStagedWriter exposes only immutable non-secret construction identity.
// Coordinators require it so an accidentally mismatched writer cannot mutate
// another owner's root while charging this operation's budget/index.
type BoundStagedWriter interface {
	StagedWriter
	StorageBinding() Config
	StorageNameRules() NameRules
}

type EffectIntent struct {
	ID, Action     string
	Source, Target Path
	Conditions     Conditions
	SourceRevision []byte
}
type EffectRecovery interface {
	InspectEffect(context.Context, EffectIntent, []byte) (Inspection, error)
}
type BoundEffectRecovery interface {
	EffectRecovery
	StorageBinding() Config
	StorageNameRules() NameRules
}

type FolderWriter interface {
	Mkdir(context.Context, Mutation, Checkpoint) (MutationResult, error)
}
type Remover interface {
	Remove(context.Context, Mutation, Checkpoint) (MutationResult, error)
}
type MoveRequest struct {
	ID               string
	Source, Target   Path
	SourceRevision   []byte
	TargetConditions Conditions
}
type NativeMover interface {
	Move(context.Context, MoveRequest, Checkpoint) (MutationResult, error)
}
type NativeCopier interface {
	Copy(context.Context, MoveRequest, Checkpoint) (MutationResult, error)
}

// NoFinalEffect may be returned only before native final-path I/O is issued. A
// timeout, lease loss or failed checkpoint after I/O is never this proof.
type NoFinalEffect struct{ Cause error }

func (e *NoFinalEffect) Error() string { return "native final effect did not start" }
func (e *NoFinalEffect) Unwrap() error { return e.Cause }
