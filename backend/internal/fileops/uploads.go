package fileops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"path"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type UploadState string

const (
	UploadOpen       UploadState = "open"
	UploadCommitting UploadState = "committing"
	UploadCommitted  UploadState = "committed"
	UploadUnknown    UploadState = "outcome_unknown"
	UploadFailed     UploadState = "failed"
	UploadAborted    UploadState = "aborted"
	UploadExpired    UploadState = "expired"
)

// UploadOptions describes fixed ingress geometry and the original public intent.
// ClientIntent excludes captured native parent/version evidence from replay identity.
type UploadOptions struct {
	PartSize       int64
	MaxParallelism int
	ClientIntent   storage.WriteIntent
}

type Upload struct {
	ID              string
	Operation       Operation
	PartSize        int64
	PartCount       int
	MaxParallelism  int
	State           UploadState
	ReceivedParts   int
	ReceivedBytes   int64
	ExpiresAt       time.Time
	CreatedAt       time.Time
	ChunksCleanedAt *time.Time
}

// PartReceipt is inserted only after the private Handle identifies durable bytes.
// Handle never belongs in a public response. Sequence is session-local ACK order.
type PartReceipt struct {
	Number   int
	Size     int64
	SHA256   string
	Handle   string
	Sequence int64
}

type UploadJournal interface {
	CreateUpload(context.Context, BeginRequest, UploadOptions) (Upload, bool, error)
	ReplayUpload(context.Context, Root, Actor, string, storage.WriteIntent) (Upload, bool, error)
	GetUpload(context.Context, Root, Actor, string) (Upload, error)
	ListUploads(context.Context, Root, Actor, int, string) ([]Upload, error)
	AcceptUploadPart(context.Context, Root, Actor, string, PartReceipt) (PartReceipt, bool, error)
	ListUploadParts(context.Context, Root, Actor, string, int, int64, int64) ([]PartReceipt, int64, error)
	FreezeUpload(context.Context, Root, Actor, string) (Upload, error)
	AbortUpload(context.Context, Root, Actor, string) (Upload, error)
	ExpireUploads(context.Context, int) ([]Upload, error)
	MarkUploadChunksCleaned(context.Context, Root, Actor, string) error
}

func clientWriteIdentity(w storage.WriteIntent) storage.WriteIntent {
	switch w.Selection {
	case "id":
		w.Target = ""
		w.ParentID = ""
	case "parentId":
		w.Target = storage.Path(path.Base(string(w.Target)))
		w.EntryID = ""
	default:
		w.EntryID, w.ParentID = "", ""
	}
	if w.Selection == "path" {
		w.Selection = ""
	}
	return w
}

func clientUploadFingerprint(w storage.WriteIntent) (string, error) {
	if w.ID != "" || w.Target == "" && (w.Selection != "id" || w.EntryID == "") || storage.ValidatePath(w.Target) != nil || w.Size < 0 || strings.ContainsRune(w.ClientCondition, 0) {
		return "", E("invalid_request", nil)
	}
	// Native Conditions, ParentPath and ParentVersion are captured admission evidence.
	// ClientCondition is the canonical original API precondition.
	w.Conditions = storage.Conditions{}
	w.ParentPath = ""
	w.ParentVersion = nil
	w = clientWriteIdentity(w)
	if w.SourceModifiedAt != nil {
		t := w.SourceModifiedAt.UTC()
		w.SourceModifiedAt = &t
	}
	b, err := json.Marshal(w)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func uploadPartCount(size, partSize int64) (int, error) {
	if size < 0 || partSize < 1 {
		return 0, E("invalid_request", nil)
	}
	count := size / partSize
	if size%partSize != 0 {
		count++
	}
	if count > math.MaxInt32 {
		return 0, E("invalid_request", nil)
	}
	return int(count), nil
}

func uploadIntent(u Upload) (storage.WriteIntent, error) {
	var w storage.WriteIntent
	if json.Unmarshal(u.Operation.Intent, &w) != nil {
		return w, E("invalid_intent", nil)
	}
	return w, nil
}

func uploadState(u Upload) UploadState {
	if u.State != UploadCommitting {
		return u.State
	}
	switch u.Operation.State {
	case Succeeded:
		return UploadCommitted
	case Failed:
		return UploadFailed
	case Cancelled:
		return UploadAborted
	case Unknown:
		return UploadUnknown
	default:
		return UploadCommitting
	}
}

// NativeUploadWriter adapts a numbered native upload to the staged writer contract.
// Stage seals durable part receipts only; Publish and Inspect come from NativeActions;
// Discard releases the unpublished allocation. The server worker and the operator
// resolver share it so both observe exactly one writer contract.
type NativeUploadWriter struct {
	NativeActions
	numbered storage.NumberedUploadWriter
	metadata storage.UploadMetadata
	// Parts are the durable receipts a commit seals, ordered by part number.
	Parts []storage.UploadPart
}

var (
	_ storage.StagedWriter        = (*NativeUploadWriter)(nil)
	_ storage.StagedDiscardProver = (*NativeUploadWriter)(nil)
)

func NewNativeUploadWriter(native NativeActions, numbered storage.NumberedUploadWriter, metadata storage.UploadMetadata) *NativeUploadWriter {
	return &NativeUploadWriter{NativeActions: native, numbered: numbered, metadata: metadata}
}

func (w *NativeUploadWriter) Stage(ctx context.Context, intent storage.WriteIntent, _ io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	return w.numbered.SealStage(ctx, intent, w.Parts, w.metadata, cp)
}

// Discard keeps existing final-path evidence authoritative: it never aborts a native
// upload while the publication driver still reports an unknown final request.
func (w *NativeUploadWriter) Discard(ctx context.Context, intent storage.WriteIntent, evidence []byte) error {
	if err := w.NativeActions.Discard(ctx, intent, evidence); err != nil {
		return err
	}
	return w.numbered.DiscardParts(ctx, intent, w.metadata)
}

// ObserveDiscard and ProveDiscard expose the driver's provider reconciliation through
// this writer's durable metadata. A driver without that capability is unsupported
// rather than silently treated as terminal.
func (w *NativeUploadWriter) ObserveDiscard(ctx context.Context, in storage.WriteIntent) (storage.DiscardEvidence, error) {
	prover, ok := w.numbered.(storage.DiscardProver)
	if !ok {
		return storage.DiscardEvidence{}, E("unsupported", nil)
	}
	return prover.ObserveDiscard(ctx, in, w.metadata)
}

func (w *NativeUploadWriter) ProveDiscard(ctx context.Context, in storage.WriteIntent) (storage.DiscardEvidence, error) {
	prover, ok := w.numbered.(storage.DiscardProver)
	if !ok {
		return storage.DiscardEvidence{}, E("unsupported", nil)
	}
	return prover.ProveDiscard(ctx, in, w.metadata)
}
