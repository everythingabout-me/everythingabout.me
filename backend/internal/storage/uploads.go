package storage

import (
	"context"
	"io"
)

// UploadPart identifies immutable, durably stored native part bytes. Handle is
// private driver evidence and must never appear in the public receipt.
type UploadPart struct {
	Number int
	Size   int64
	SHA256 string
	Handle string
}

// UploadMetadata coordinates native allocations and immutable part identities
// across independent receivers. Binding includes owner, connection, generation
// and roots. Implementations durably save each Put before returning. Number zero
// holds the exclusive session fence; positive numbers hold a shared session fence
// plus an ingress slot and part lock. Number -1 is a shared metadata-only lookup.
type UploadMetadata interface {
	WithUploadMetadata(context.Context, string, string, int, func(UploadMetadataRecord) error) error
}
type UploadMetadataRecord interface {
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, string, []byte) error
}

// NumberedUploadWriter keeps acknowledged bytes in unpublished provider state,
// not a complete local file. ReceivePart acknowledges only after native success
// and the caller's durable receipt. SealStage performs metadata work only.
// DiscardParts fences allocations before aborting unpublished provider state.
type NumberedUploadWriter interface {
	UploadMaxFileBytes() int64
	ReceivePart(context.Context, WriteIntent, UploadPart, io.Reader, UploadMetadata, func(UploadPart) (UploadPart, error)) (UploadPart, error)
	SealStage(context.Context, WriteIntent, []UploadPart, UploadMetadata, Checkpoint) (Staged, error)
	DiscardParts(context.Context, WriteIntent, UploadMetadata) error
}

// DiscardEvidence is a read-only provider report for the allocation of one intent.
// UploadAbsent reports that the provider no longer knows the upload session at all, so
// its parts can never be completed, read, or replayed.
type DiscardEvidence struct {
	UploadAbsent bool
	Parts        int // parts the provider still reports when UploadAbsent is false
}

// DiscardProver is an explicit operator capability on a numbered upload driver. Ordinary
// discard already releases an allocation once the provider definitely accepts its abort;
// this exists only for a session whose abort answer can never be obtained. ObserveDiscard
// is read-only; ProveDiscard records provider evidence that the allocation no longer
// exists so a later discard may release it. Neither publishes bytes, completes a
// multipart upload, or replays a part request.
type DiscardProver interface {
	ObserveDiscard(context.Context, WriteIntent, UploadMetadata) (DiscardEvidence, error)
	ProveDiscard(context.Context, WriteIntent, UploadMetadata) (DiscardEvidence, error)
}

// StagedDiscardProver is the staged-writer form of DiscardProver: the same evidence,
// resolved through the writer's own durable upload metadata rather than through a
// caller-supplied identity.
type StagedDiscardProver interface {
	ObserveDiscard(context.Context, WriteIntent) (DiscardEvidence, error)
	ProveDiscard(context.Context, WriteIntent) (DiscardEvidence, error)
}
