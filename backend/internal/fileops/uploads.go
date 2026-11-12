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
// ClientIntent excludes captured native parent/revision evidence from replay identity.
type UploadOptions struct {
	PartSize     int64
	ClientIntent storage.WriteIntent
	// Mode is UploadModeMultipart unless the connection admitted a conditional single PUT
	// and the file fits in one: then the client publishes at the final key and the commit
	// only verifies it. See sql/039_upload_mode.sql.
	Mode string
}

// Upload modes. Multipart stages parts under the final key and the server publishes by
// completing them; direct lets the client PUT one conditional object at the final key.
const (
	UploadModeMultipart = "multipart"
	UploadModeDirect    = "direct"
)

type Upload struct {
	ID              string
	Operation       Operation
	PartSize        int64
	PartCount       int
	Mode            string
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

// uploadIdleWindow is how long an open session may sit without server-visible activity
// before it expires and releases its quota and its native allocation.
//
// The bound comes from the protocol, not from taste: a class A part URL lives one hour
// (filesReadGrantWindow), so a transfer that is still moving must ask for a fresh
// signature at least that often. Two hours of neither a signature nor a received part
// therefore means nothing can still be arriving — "the page is gone, so the upload is
// gone". An open session fences no path (it takes its guard at commit), so this window
// decides only how long an abandoned session keeps its reservation and staged parts; it is
// not what makes the destination usable again.
const uploadIdleWindow = 2 * time.Hour

// uploadSessionLifetime caps a session that keeps trickling activity forever, so a client
// cannot hold a reservation indefinitely by signing one part a day.
const uploadSessionLifetime = 7 * 24 * time.Hour

type UploadJournal interface {
	CreateUpload(context.Context, BeginRequest, UploadOptions) (Upload, bool, error)
	ReplayUpload(context.Context, Root, Actor, string, storage.WriteIntent) (Upload, bool, error)
	GetUpload(context.Context, Root, Actor, string) (Upload, error)
	// TouchUpload records server-visible progress and slides the idle expiry. Class A parts
	// never pass through this application, so a signature request is the only evidence that
	// such a transfer is still moving.
	TouchUpload(context.Context, Root, Actor, string) (Upload, error)
	ListUploads(context.Context, Root, Actor, int, string) ([]Upload, error)
	AcceptUploadPart(context.Context, Root, Actor, string, PartReceipt) (PartReceipt, bool, error)
	ListUploadParts(context.Context, Root, Actor, string, int, int64, int64) ([]PartReceipt, int64, error)
	FreezeUpload(context.Context, Root, Actor, string, UploadBinding) (Upload, error)
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
	// Native Conditions, ParentPath, ParentRevision and Parents are captured admission
	// evidence. ClientCondition is the canonical original API precondition.
	//
	// Parents is not even the client's flag: admission rewrites it to the connection's
	// ImplicitParents capability, so with a provider that makes a folder with its first
	// child (COS) an admitted create stores true while the replay of the same request —
	// which runs before resolution — computes false. Every parents:true create replay then
	// answered idempotency_conflict, and the one path that replays a create is the repair of
	// a lost ACK, so a cancelled import reported one failed row per file whose session it
	// was about to discard.
	w.Conditions = storage.Conditions{}
	w.ParentPath = ""
	w.ParentRevision = nil
	w.Parents = false
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

// uploadStateAt projects the stored session state at now. It derives the two states that are
// facts about time or about the operation rather than columns: a committing session follows
// its operation (uploadState), and an open session past expires_at is expired.
//
// Deriving expiry is what lets a listing report a lapsed session without writing the row. A
// read-only transaction cannot terminate it (SQLSTATE 25006), and a read must not release the
// logical reservation or cancel the operation even when it could; ExpireUploads owns that
// transition. The derived state therefore matches the answer a status read gives, without
// claiming the row was settled.
func uploadStateAt(u Upload, now time.Time) UploadState {
	state := uploadState(u)
	if state == UploadOpen && !u.ExpiresAt.After(now) {
		return UploadExpired
	}
	return state
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

// PlannedUploadWriter adapts a class A planned upload to the staged writer
// contract. Stage seals the provider's own part listing; no bytes are received.
type PlannedUploadWriter struct {
	NativeActions
	planned  storage.PlannedUploadWriter
	metadata storage.UploadMetadata
}

var (
	_ storage.StagedWriter        = (*NativeUploadWriter)(nil)
	_ storage.StagedDiscardProver = (*NativeUploadWriter)(nil)
	_ storage.StagedWriter        = (*PlannedUploadWriter)(nil)
	_ storage.StagedDiscardProver = (*PlannedUploadWriter)(nil)
)

func NewPlannedUploadWriter(native NativeActions, planned storage.PlannedUploadWriter, metadata storage.UploadMetadata) *PlannedUploadWriter {
	return &PlannedUploadWriter{NativeActions: native, planned: planned, metadata: metadata}
}

func (w *PlannedUploadWriter) Stage(ctx context.Context, intent storage.WriteIntent, _ io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	return w.planned.SealPlannedUpload(ctx, intent, w.metadata, cp)
}

func (w *PlannedUploadWriter) Discard(ctx context.Context, intent storage.WriteIntent, evidence []byte) error {
	if err := w.NativeActions.Discard(ctx, intent, evidence); err != nil {
		return err
	}
	return w.planned.DiscardPlannedUpload(ctx, intent, w.metadata)
}

func (w *PlannedUploadWriter) ObserveDiscard(ctx context.Context, in storage.WriteIntent) (storage.DiscardEvidence, error) {
	prover, ok := w.planned.(storage.DiscardProver)
	if !ok {
		return storage.DiscardEvidence{}, E("unsupported", nil)
	}
	return prover.ObserveDiscard(ctx, in, w.metadata)
}

func (w *PlannedUploadWriter) ProveDiscard(ctx context.Context, in storage.WriteIntent) (storage.DiscardEvidence, error) {
	prover, ok := w.planned.(storage.DiscardProver)
	if !ok {
		return storage.DiscardEvidence{}, E("unsupported", nil)
	}
	return prover.ProveDiscard(ctx, in, w.metadata)
}

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
