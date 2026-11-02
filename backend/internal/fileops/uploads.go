package fileops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
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

func clientUploadFingerprint(w storage.WriteIntent) (string, error) {
	if w.ID != "" || w.Target == "" || storage.ValidatePath(w.Target) != nil || w.Size < 0 || strings.ContainsRune(w.ClientCondition, 0) {
		return "", E("invalid_request", nil)
	}
	// Native Conditions, ParentPath and ParentVersion are captured admission evidence.
	// ClientCondition is the canonical original API precondition.
	w.Conditions = storage.Conditions{}
	w.ParentPath = ""
	w.ParentVersion = nil
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
