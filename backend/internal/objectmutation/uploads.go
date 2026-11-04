package objectmutation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"everythingabout.me/backend/internal/storage"
)

var _ storage.DiscardProver = (*Driver)(nil)

const nativePartSize int64 = 8 << 20
const nativeMaxParts = 10000

// Bounded independently of users/sessions. Payloads are released after a part RPC;
// independent receivers need no shared filesystem and never retain a whole file.
// Two full per-upload admissions (fileops.UploadPartSlots each) may be in flight
// at once; every payload is one 8 MiB part, never a whole file.
var nativeReceiveSlots = make(chan struct{}, 32)

type nativeSession struct {
	Intent   storage.WriteIntent
	UploadID string
	Phase    string
}
type nativePartReceipt struct {
	Number    int
	Size      int64
	SHA256    string
	UploadID  string
	ETag      string
	Uncertain bool
	MD5       string `json:",omitempty"`
	Signed    bool   `json:",omitempty"`
}
type nativeStage struct {
	UploadID string
	Parts    []NativePart
}

func (d *Driver) UploadMaxFileBytes() int64 {
	if !d.options.Admission.MultipartUploads {
		return 0
	}
	if _, ok := d.backend.(MultipartBackend); !ok {
		return 0
	}
	return nativePartSize * nativeMaxParts
}
func (d *Driver) validateUploadIntent(in storage.WriteIntent) error {
	if err := validateRecoveryIntent(in); err != nil {
		return err
	}
	if d.UploadMaxFileBytes() == 0 || in.Size > d.UploadMaxFileBytes() || in.SourceModifiedAt != nil || in.Digest != nil || len(d.binding.ContentRoot)+len(in.Target) > 1024 {
		return fail("unsupported", in.Target)
	}
	return nil
}
func encodeNative(v any) []byte { b, _ := json.Marshal(v); return b }
func readNative[T any](ctx context.Context, record storage.UploadMetadataRecord, key string) (*T, error) {
	b, e := record.Get(ctx, key)
	if e != nil || len(b) == 0 {
		return nil, e
	}
	var v T
	if json.Unmarshal(b, &v) != nil {
		return nil, fail("invalid_evidence", "")
	}
	return &v, nil
}
func partMetadataKey(n int) string { return fmt.Sprintf("part:%d", n) }
func (d *Driver) nativeAllocation(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata) (*nativeSession, error) {
	if metadata == nil {
		return nil, fail("invalid_config", in.Target)
	}
	var session *nativeSession
	// Established allocations require a shared read, not an exclusive session
	// lock: another part may already hold its shared fence across native I/O.
	err := metadata.WithUploadMetadata(ctx, d.namespace, in.ID, -1, func(record storage.UploadMetadataRecord) error {
		var err error
		session, err = readNative[nativeSession](ctx, record, "session")
		if err != nil {
			return err
		}
		if session != nil && !sameJSON(session.Intent, in) {
			return fail("intent_conflict", in.Target)
		}
		if session != nil && session.Phase != "receiving" && session.Phase != "allocating" {
			return fail("upload_aborted", in.Target)
		}
		return nil
	})
	if err != nil || session != nil && session.Phase == "receiving" && session.UploadID != "" {
		return session, err
	}
	err = metadata.WithUploadMetadata(ctx, d.namespace, in.ID, 0, func(record storage.UploadMetadataRecord) error {
		var err error
		session, err = readNative[nativeSession](ctx, record, "session")
		if err != nil {
			return err
		}
		if session == nil {
			session = &nativeSession{Intent: cloneIntent(in), Phase: "allocating"}
			if err = record.Put(ctx, "session", encodeNative(session)); err != nil {
				return err
			}
		}
		if !sameJSON(session.Intent, in) {
			return fail("intent_conflict", in.Target)
		}
		if session.Phase != "allocating" && session.Phase != "receiving" {
			return fail("upload_aborted", in.Target)
		}
		if session.UploadID == "" {
			// A lost initiation reply can leave only unpublished fragments. Persisted
			// identity never authorizes final publication; provider lifecycle is backstop.
			id, e := d.backend.(MultipartBackend).BeginMultipart(ctx, Put{Key: d.key(in.Target, storage.File), Size: in.Size, MediaType: in.MediaType, Metadata: map[string]string{"eam-intent": in.ID}})
			if e != nil {
				return e
			}
			if id == "" || len(id) > 4096 {
				return fail("invalid_backend_response", in.Target)
			}
			session.UploadID = id
			session.Phase = "receiving"
			if err = record.Put(ctx, "session", encodeNative(session)); err != nil {
				return err
			}
		}
		return nil
	})
	return session, err
}

func (d *Driver) ReceivePart(ctx context.Context, in storage.WriteIntent, part storage.UploadPart, body io.Reader, metadata storage.UploadMetadata, accept func(storage.UploadPart) (storage.UploadPart, error)) (storage.UploadPart, error) {
	if err := d.validateUploadIntent(in); err != nil {
		return part, err
	}
	count := (in.Size + nativePartSize - 1) / nativePartSize
	expected := nativePartSize
	if int64(part.Number) == count {
		expected = in.Size - int64(part.Number-1)*nativePartSize
	}
	if body == nil || accept == nil || part.Number < 1 || int64(part.Number) > count || part.Size != expected {
		return part, fail("invalid_request", in.Target)
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return part, err
	}
	defer done()
	select {
	case nativeReceiveSlots <- struct{}{}:
		defer func() { <-nativeReceiveSlots }()
	case <-ctx.Done():
		return part, ctx.Err()
	}
	if closer, ok := body.(io.ReadCloser); ok {
		stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stop()
	}
	session, err := d.nativeAllocation(ctx, in, metadata)
	if err != nil {
		return part, err
	}
	var result storage.UploadPart
	err = metadata.WithUploadMetadata(ctx, d.namespace, in.ID, part.Number, func(record storage.UploadMetadataRecord) error {
		data := make([]byte, int(part.Size))
		_, e := io.ReadFull(&ctxReader{ctx, body}, data)
		if e != nil {
			if errors.Is(e, io.EOF) || errors.Is(e, io.ErrUnexpectedEOF) {
				return fail("size_mismatch", in.Target)
			}
			return e
		}
		var extra [1]byte
		n, readErr := io.ReadFull(&ctxReader{ctx, body}, extra[:])
		if n != 0 {
			return fail("size_mismatch", in.Target)
		}
		if readErr != io.EOF {
			return readErr
		}
		h := sha256.Sum256(data)
		hash := hex.EncodeToString(h[:])
		if part.SHA256 != "" && part.SHA256 != hash {
			return fail("digest_mismatch", in.Target)
		}
		current, e := readNative[nativeSession](ctx, record, "session")
		if e != nil {
			return e
		}
		if current == nil || !sameJSON(current.Intent, in) || current.UploadID != session.UploadID {
			return fail("invalid_evidence", in.Target)
		}
		if current.Phase != "receiving" {
			return fail("upload_aborted", in.Target)
		}
		candidate, e := readNative[nativePartReceipt](ctx, record, partMetadataKey(part.Number))
		if e != nil {
			return e
		}
		if candidate == nil {
			candidate = &nativePartReceipt{Number: part.Number, Size: part.Size, SHA256: hash, UploadID: session.UploadID}
			// Bind bytes durably BEFORE the native slot can receive them. A timed-out
			// prior request can only ever race identical content at this native slot.
			if e = record.Put(ctx, partMetadataKey(part.Number), encodeNative(candidate)); e != nil {
				return e
			}
		}
		if candidate.Signed || candidate.Number != part.Number || candidate.Size != part.Size || candidate.SHA256 != hash || candidate.UploadID != session.UploadID {
			return fail("part_conflict", in.Target)
		}
		if candidate.ETag == "" {
			priorUncertain := candidate.Uncertain
			candidate.Uncertain = true
			if e = record.Put(ctx, partMetadataKey(part.Number), encodeNative(candidate)); e != nil {
				return e
			}
			tag, e := d.backend.(MultipartBackend).PutPart(ctx, d.key(in.Target, storage.File), session.UploadID, part.Number, bytes.NewReader(data), part.Size)
			if e != nil {
				return e
			}
			if tag == "" || len(tag) > 256 {
				return fail("invalid_backend_response", in.Target)
			}
			candidate.ETag = tag
			candidate.Uncertain = priorUncertain
			if e = record.Put(ctx, partMetadataKey(part.Number), encodeNative(candidate)); e != nil {
				return e
			}
		}
		result = storage.UploadPart{Number: candidate.Number, Size: candidate.Size, SHA256: candidate.SHA256, Handle: string(encodeNative(candidate))}
		accepted, e := accept(result)
		if e != nil {
			return e
		}
		if !sameJSON(result, accepted) {
			return fail("invalid_evidence", in.Target)
		}
		return nil
	})
	return result, err
}

func (d *Driver) SealStage(ctx context.Context, in storage.WriteIntent, parts []storage.UploadPart, metadata storage.UploadMetadata, cp storage.Checkpoint) (storage.Staged, error) {
	if err := d.validateUploadIntent(in); err != nil {
		return storage.Staged{}, err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Staged{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "write", in.ID, false)
	if err != nil {
		return storage.Staged{}, err
	}
	defer unlock()
	r, err := d.load("write", in.ID)
	if err == nil {
		if !sameJSON(r.Intent, in) || r.Native == nil {
			return storage.Staged{}, fail("intent_conflict", in.Target)
		}
		if r.Phase != "ready" && r.Phase != "applied" {
			return storage.Staged{}, fail("outcome_unknown", in.Target)
		}
		if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
			return storage.Staged{}, err
		}
		return d.staged(r), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return storage.Staged{}, err
	}
	stage := &nativeStage{}
	err = metadata.WithUploadMetadata(ctx, d.namespace, in.ID, 0, func(record storage.UploadMetadataRecord) error {
		session, e := readNative[nativeSession](ctx, record, "session")
		if e != nil {
			return e
		}
		if in.Size == 0 && session == nil {
			session = &nativeSession{Intent: cloneIntent(in), Phase: "receiving"}
		}
		if session == nil || !sameJSON(session.Intent, in) || session.Phase != "receiving" && session.Phase != "sealed" {
			return fail("invalid_evidence", in.Target)
		}
		count := int((in.Size + nativePartSize - 1) / nativePartSize)
		if len(parts) != count {
			return fail("upload_incomplete", in.Target)
		}
		stage.UploadID = session.UploadID
		for index, p := range parts {
			known, e := readNative[nativePartReceipt](ctx, record, partMetadataKey(index+1))
			if e != nil {
				return e
			}
			expected := nativePartSize
			if index == count-1 {
				expected = in.Size - int64(index)*nativePartSize
			}
			if known == nil || p.Number != index+1 || p.Size != expected || known.Number != p.Number || known.Size != p.Size || known.SHA256 != p.SHA256 || known.UploadID != session.UploadID || known.ETag == "" || p.Handle != string(encodeNative(known)) {
				return fail("invalid_evidence", in.Target)
			}
			stage.Parts = append(stage.Parts, NativePart{Number: p.Number, ETag: known.ETag})
		}
		session.Phase = "sealed"
		return record.Put(ctx, "session", encodeNative(session))
	})
	if err != nil {
		return storage.Staged{}, err
	}
	attempt, err := randomAttempt()
	if err != nil {
		return storage.Staged{}, err
	}
	r = &ledger{Kind: "write", ID: in.ID, Intent: cloneIntent(in), Attempt: attempt, Phase: "ready", ParentVersion: bytes.Clone(in.ParentVersion), Native: stage}
	if err = d.save(r); err != nil {
		return storage.Staged{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.Staged{}, err
	}
	return d.staged(r), nil
}

func (d *Driver) DiscardParts(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata) error {
	if metadata == nil {
		return fail("invalid_config", in.Target)
	}
	return metadata.WithUploadMetadata(ctx, d.namespace, in.ID, 0, func(record storage.UploadMetadataRecord) error {
		session, e := readNative[nativeSession](ctx, record, "session")
		if e != nil {
			return e
		}
		if session == nil {
			return record.Put(ctx, "session", encodeNative(&nativeSession{Intent: cloneIntent(in), Phase: "discarded"}))
		}
		if !sameJSON(session.Intent, in) {
			return fail("intent_conflict", in.Target)
		}
		session.Phase = "discarded"
		if e = record.Put(ctx, "session", encodeNative(session)); e != nil {
			return e
		}
		if session.UploadID != "" {
			proof, e := readNative[nativeDiscardProof](ctx, record, discardProofKey)
			if e != nil {
				return e
			}
			if !provesDiscarded(proof, session.UploadID) {
				if e = d.backend.(MultipartBackend).AbortMultipart(ctx, d.key(in.Target, storage.File), session.UploadID); e != nil {
					// The abort outcome is unknown: the provider may still hold the
					// session and its parts, so every receipt stays retained and the
					// caller retries. Only a definite provider answer releases liability.
					return e
				}
				// A definite abort consumes the whole allocation: the provider deletes
				// the upload session and every part stored under it, so a receipt that
				// no reply ever confirmed (its receiver died between the part request
				// and the answer) can no longer hide bytes anything could complete,
				// read, or replay. Record that answer as durable evidence bound to this
				// one UploadID. Without it, a single uncertain receipt would retain the
				// reservation and retry cleanup forever, even though cancelling or
				// losing an upload mid-flight is an ordinary outcome.
				if e = record.Put(ctx, discardProofKey, encodeNative(&nativeDiscardProof{UploadID: session.UploadID, Observed: time.Now().UTC(), Via: "abort"})); e != nil {
					return e
				}
			}
		}
		return nil
	})
}

// discardProofKey holds durable provider evidence for one allocation: either the
// definite answer to our own abort ("abort"), or the explicit operator probe recorded
// for an allocation whose abort never got a definite answer ("operator").
const discardProofKey = "discard-proof"

// nativeDiscardProof is durable proof that the provider no longer knows the exact
// upload session, so no part of it can be completed, read, or replayed. It releases
// retained liability only: it is never publication, content, or receipt evidence.
type nativeDiscardProof struct {
	UploadID string
	Observed time.Time
	Parts    int    // parts the provider still reported, when an operator probed
	Via      string // "abort" or "operator"; empty for proofs recorded before Via existed
}

func provesDiscarded(proof *nativeDiscardProof, uploadID string) bool {
	return proof != nil && uploadID != "" && proof.UploadID == uploadID
}

// ObserveDiscard reports, read-only, whether the provider still knows the allocation
// recorded for this intent. It is an operator probe and writes no receipt.
func (d *Driver) ObserveDiscard(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata) (storage.DiscardEvidence, error) {
	return d.discardEvidence(ctx, in, metadata, false)
}

// ProveDiscard records provider proof that a discarded allocation no longer exists, so
// DiscardParts may release receipts no reply ever confirmed. It refuses a live
// allocation and any provider answer other than "no such upload".
func (d *Driver) ProveDiscard(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata) (storage.DiscardEvidence, error) {
	return d.discardEvidence(ctx, in, metadata, true)
}

func (d *Driver) discardEvidence(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata, prove bool) (storage.DiscardEvidence, error) {
	var evidence storage.DiscardEvidence
	if metadata == nil {
		return evidence, fail("invalid_config", in.Target)
	}
	// Reconciliation inspects historical work: admission limits may have changed since
	// the upload was accepted, so only the intent shape is revalidated here.
	if err := validateRecoveryIntent(in); err != nil {
		return evidence, err
	}
	inspector, ok := d.backend.(MultipartInspector)
	if !ok {
		return evidence, fail("unsupported", in.Target)
	}
	work, done, err := d.operation(ctx)
	if err != nil {
		return evidence, err
	}
	defer done()
	// Number -1 is a shared metadata-only lookup; proving needs the exclusive session
	// fence so the evidence is written against the same session that was observed.
	number := -1
	if prove {
		number = 0
	}
	err = metadata.WithUploadMetadata(work, d.namespace, in.ID, number, func(record storage.UploadMetadataRecord) error {
		session, e := readNative[nativeSession](work, record, "session")
		if e != nil {
			return e
		}
		if session == nil || session.UploadID == "" {
			return fail("not_found", in.Target)
		}
		if !sameJSON(session.Intent, in) {
			return fail("intent_conflict", in.Target)
		}
		if prove && session.Phase != "discarded" {
			return fail("invalid_transition", in.Target)
		}
		state, e := inspector.InspectMultipart(work, d.key(in.Target, storage.File), session.UploadID)
		if e != nil {
			return e
		}
		evidence = storage.DiscardEvidence{UploadAbsent: !state.Present, Parts: state.Parts}
		if !prove {
			return nil
		}
		if state.Present {
			return fail("recovery_required", in.Target)
		}
		return record.Put(work, discardProofKey, encodeNative(&nativeDiscardProof{UploadID: session.UploadID, Observed: time.Now().UTC(), Parts: state.Parts, Via: "operator"}))
	})
	return evidence, err
}
