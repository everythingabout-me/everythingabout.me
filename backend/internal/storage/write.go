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
	// ImplicitFolder states that the addressed folder has no native marker object:
	// the provider makes a folder from its children, so the folder exists only while
	// they do (see Descriptor.ImplicitParents). It is valid only for Remove, and only
	// when the caller observed no revision. It asks the driver to settle removal of a
	// marker-less folder rather than to delete anything native.
	//
	// It is explicit because the alternative — treating any revision-less Remove as
	// this case — is indistinguishable from a caller that forgot the revision, and a
	// delete that silently settles without a native effect looks identical to one that
	// removed the object. A driver whose folders always have a marker (a filesystem)
	// refuses it as unsupported instead of pretending it did something.
	ImplicitFolder bool
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

// IntentRetention is how long a driver keeps a settled intent ledger record after the
// operation reaches a terminal phase. It is the minimum 24-hour window the operation
// journal guarantees for a terminal operation, so a record disappears only after every
// client retry it could still answer has itself expired. A record is recovery evidence, not user
// data: keeping it forever made a disposable spool block the automatic baseline after an
// index loss, while removing a non-terminal one would forget a possible effect. Cleanup
// therefore applies this duration only to terminal records and never to any other state.
const IntentRetention = 24 * time.Hour

type WriteIntent struct {
	EntryID    string `json:",omitempty"`
	ParentID   string `json:",omitempty"`
	Selection  string `json:",omitempty"`
	ID         string
	Target     Path
	Conditions Conditions
	// Parents records that the destination's missing ancestors were accepted at admission
	// and are materialized when the file lands, rather than existing before it. Only a
	// connection whose provider makes a folder with its first child sets it.
	Parents          bool `json:",omitempty"`
	Size             int64
	MediaType        string
	SourceModifiedAt *time.Time
	ClientCondition  string
	ParentPath       Path
	ParentRevision   []byte
	Digest           *Digest
	// AssertedDigest is the content digest the owner's client computed for these bytes.
	// Nothing in this layer verifies it — a class A publication never sees the bytes — so it
	// is recorded, never trusted: the only decision it feeds is whether a creation for the
	// same path may skip its transfer because the library already holds this content.
	AssertedDigest *Digest `json:",omitempty"`
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

// StagingBound is implemented by a staged writer whose one-entry staging ceiling must
// be computed from the work rather than fixed when the writer was built.
//
// A relay write stages one entry at a time, so the bound the writer needs is the
// largest entry it will be handed — for a snapshot, the largest file the copy admits.
// A fixed interactive-request policy (32 MiB by default on an object connection)
// instead fails a whole-tree copy at the first larger file, after the copy has already
// written part of the tree; the bound it was given described a different piece of work.
//
// AdmitStagedBytes raises the ceiling to size for the lifetime of this writer and
// returns a named error when the provider cannot publish an entry that large in one
// request. It never lowers the ceiling, and a writer without this interface simply
// keeps its own bound.
type StagingBound interface {
	AdmitStagedBytes(size int64) error
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

// MarkerReassertor moves an existing folder marker's native revision past the one the
// caller observed, without removing content. It exists for one case: a create that
// re-created a folder while a delete of the same path was being purged. The row is already
// back in the index, but the purge listed the marker before that row committed; an ordinary
// conditional delete would still match the unchanged marker and remove the folder the user
// just re-created. A marker whose revision moved makes that delete refuse (`changed`) and
// the purge adopt the newer intent (docs/design/deletes.md).
//
// The caller passes the revision it observed in Mutation.Conditions.ExpectedRevision; an
// empty value means the caller created the marker in this operation and only needs its
// revision to move. An implementation must:
//   - change the native revision a conditional delete compares (object ETag, or the local
//     directory identity), never merely rewrite identical bytes;
//   - refuse (`changed`) when the native state no longer matches the expected revision;
//   - refuse (`not_empty`) rather than remove or replace a folder that now holds children —
//     user content is never deleted to reassert a marker;
//   - when the marker is already gone, leave a marker behind rather than report success
//     with none (the purge won the race; the create must still end native);
//   - be safe to repeat: a lost reply is resolved by the caller retrying the fence.
type MarkerReassertor interface {
	Reassert(context.Context, Mutation, Checkpoint) (MutationResult, error)
}

// Remover removes a file or an empty folder. It never recurses: the service
// explicitly enumerates and journals a recursive deletion.
//
// The native state to remove is named, not guessed. A file or a folder with a marker
// object is removed with Conditions.ExpectedRevision set to the revision the caller
// observed. A folder the driver reported with no revision has no marker object (its
// provider makes it from its children), so removing its children removed it; that case
// is expressed with Conditions.ImplicitFolder instead, and the driver must verify that
// no marker and no child appeared. A removal with neither is refused with a named
// error (`missing_revision`) rather than settled as a no-op: a delete that reports
// success without issuing the conditional effect is indistinguishable from a real
// deletion, and the caller then records a state that the tree does not have.
type Remover interface {
	Remove(context.Context, Mutation, Checkpoint) (MutationResult, error)
}

// Purger deletes one native object whose removal the caller has already made final,
// using an observation it already holds. It exists for a purge and for nothing else.
//
// Remove cannot serve that case: it re-observes the object twice, journals a receipt and
// fsyncs a ledger before the one request that matters, because a mutation whose reply is
// lost must be attributable later. A purge has no such question. The index already
// recorded the deletion, the conditional request carries the identity, and a lost reply
// is answered by the next walk finding the object gone or still there — which is the same
// re-walk that resumes a crashed purge. So a purge may skip both, and one object costs
// one request instead of five to seven.
//
// An implementation must still refuse to delete an object whose revision moved: the
// caller adopts that as a newer intent, and a delete that fails loud and quiet is better
// than one that fails destructive. `changed` is that refusal.
type Purger interface {
	Purge(context.Context, Path, Kind, []byte) error
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
