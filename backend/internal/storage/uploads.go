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
