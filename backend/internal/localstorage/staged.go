package localstorage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"

	"everythingabout.me/backend/internal/storage"
)

// WriteOptions requires all application and external writers to participate in
// coordinated exclusion. The standalone staged helper remains create-only.
type WriteOptions struct{ CoordinatedExclusiveWriter bool }

// StagedWriter preserves ordinary files and exact native names. The private
// system-root ledger survives restart independently of service checkpoints.
// Temporary discovery is bounded to known per-intent ledger and data names;
// there is no content-root garbage collection. The caller owns driver lifetime.
type StagedWriter struct {
	driver      *driver
	namespace   string
	link        func(*os.Root, string, *os.Root, string) error
	replacement bool
	replace     func(*os.Root, string, *os.Root, string, bool) error
}

var _ storage.StagedWriter = (*StagedWriter)(nil)
var _ storage.EffectRecovery = (*StagedWriter)(nil)

// StorageBinding identifies the exact configured owner/root pair for journal
// admission. It excludes credentials; the native ledger additionally binds held
// root inode identity through driver.binding. The returned value is a copy.
func (w *StagedWriter) StorageBinding() storage.Config { return w.driver.config }

// StorageNameRules binds coordinator path guards to this driver's equivalence
// policy. Return an independent slice if future local rules reserve names.
func (w *StagedWriter) StorageNameRules() storage.NameRules {
	rules := w.driver.Describe().Names
	rules.ReservedNames = append([]string(nil), rules.ReservedNames...)
	return rules
}

// NewStagedWriter rejects cross-volume staging and implicit writer admission.
// This constructor does not change the read driver's advertised capabilities.
func NewStagedWriter(bound storage.Driver, options WriteOptions) (*StagedWriter, error) {
	d, ok := bound.(*driver)
	if !ok || !options.CoordinatedExclusiveWriter || !stagedSupported {
		return nil, storage.NewError("unsupported", "", nil)
	}
	release, err := d.acquire(context.Background())
	if err != nil {
		return nil, err
	}
	defer release()
	ci, err := d.content.Stat(".")
	if err != nil {
		return nil, err
	}
	si, err := d.system.Stat(".")
	if err != nil {
		return nil, err
	}
	if !stagedSameFilesystem(ci, si) {
		return nil, storage.NewError("unsupported", "", nil)
	}
	for _, root := range []*os.Root{d.content, d.system} {
		local, err := stagedLocalFilesystem(root)
		if err != nil {
			return nil, err
		}
		if !local {
			return nil, storage.NewError("unsupported", "", nil)
		}
	}
	w := &StagedWriter{driver: d, namespace: "staged-v1-" + d.binding, link: stagedLink}
	if err := d.system.Mkdir(w.namespace, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	root, err := w.root(context.Background())
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := syncRoot(d.system); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *StagedWriter) root(ctx context.Context) (*os.Root, error) {
	root, info, err := w.driver.ownerChild(ctx, w.driver.system, w.namespace)
	if err != nil {
		return nil, err
	}
	if !stagedPrivate(info) {
		root.Close()
		return nil, storage.NewError("permission_denied", "", nil)
	}
	return root, nil
}

// Persistent lock names are never unlinked, which would split lock identity.
func intentKey(id string) string { h := sha256.Sum256([]byte(id)); return hex.EncodeToString(h[:]) }

type stageReceipt struct {
	Version                     int
	Binding, ID, Attempt, Phase string
}
type stageLedger struct {
	Version        int
	Binding        string
	Intent         storage.WriteIntent
	Attempt        string
	Phase          string
	Identity       string
	ParentIdentity string
	Digest         *storage.Digest
	Result         *storage.MutationResult
}

func (w *StagedWriter) receipt(r *stageLedger) []byte {
	b, _ := json.Marshal(stageReceipt{1, w.driver.binding, r.Intent.ID, r.Attempt, r.Phase})
	return b
}
func (w *StagedWriter) staged(r *stageLedger) storage.Staged {
	return storage.Staged{Intent: cloneIntent(r.Intent), Handle: w.receipt(r), Size: r.Intent.Size, Digest: cloneDigest(r.Digest)}
}
func cloneDigest(d *storage.Digest) *storage.Digest {
	if d == nil {
		return nil
	}
	return &storage.Digest{Algorithm: d.Algorithm, Value: bytes.Clone(d.Value)}
}
func cloneIntent(in storage.WriteIntent) storage.WriteIntent {
	in.Conditions.ExpectedRevision = bytes.Clone(in.Conditions.ExpectedRevision)
	in.Digest = cloneDigest(in.Digest)
	if in.SourceModifiedAt != nil {
		captured := *in.SourceModifiedAt
		in.SourceModifiedAt = &captured
	}
	return in
}
func sameIntent(a, b storage.WriteIntent) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}
func (w *StagedWriter) validateIntent(in storage.WriteIntent) error {
	if !validIdentity(in.ID) || len(in.ID) > 1024 || len(in.MediaType) > 1024 || in.Target == "" || in.Size < 0 {
		return storage.NewError("invalid_intent", in.Target, nil)
	}
	if err := validatePath(in.Target); err != nil {
		return err
	}
	if !(in.Conditions.Absent && len(in.Conditions.ExpectedRevision) == 0) && !(w.replacement && !in.Conditions.Absent && len(in.Conditions.ExpectedRevision) == sha256.Size) {
		return storage.NewError("unsupported", in.Target, nil)
	}
	if in.Digest != nil && (in.Digest.Algorithm != "sha-256" || len(in.Digest.Value) != sha256.Size) {
		return storage.NewError("unsupported", in.Target, nil)
	}
	return nil
}
func (w *StagedWriter) validateReceipt(in storage.WriteIntent, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	var r stageReceipt
	if len(b) > 8192 || json.Unmarshal(b, &r) != nil || r.Version != 1 || r.Binding != w.driver.binding || r.ID != in.ID || !validAttempt(r.Attempt) {
		return storage.NewError("invalid_evidence", in.Target, nil)
	}
	return nil
}
func (w *StagedWriter) load(root *os.Root, in storage.WriteIntent) (*stageLedger, error) {
	f, err := openReadOnly(root, intentKey(in.ID)+".json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, storage.NewError("invalid_evidence", in.Target, nil)
	}
	var r stageLedger
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || dec.Decode(new(any)) != io.EOF || r.Version != 1 || r.Binding != w.driver.binding || !sameIntent(r.Intent, in) || !validAttempt(r.Attempt) {
		return nil, storage.NewError("intent_conflict", in.Target, nil)
	}
	return &r, nil
}
func validAttempt(a string) bool {
	if a == "" {
		return true
	}
	b, err := hex.DecodeString(a)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == a
}
func dataName(r *stageLedger) string { return intentKey(r.Intent.ID) + "-" + r.Attempt + ".data" }
func syncRoot(root *os.Root) error {
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (w *StagedWriter) save(root *os.Root, r *stageLedger) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	key := intentKey(r.Intent.ID)
	// The sole scratch name is known before allocation and protected by the lock.
	if err := root.Remove(key + ".next"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := stagedCreate(root, key+".next")
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	syncErr := f.Sync()
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := root.Rename(key+".next", key+".json"); err != nil {
		return err
	}
	return syncRoot(root)
}
func (w *StagedWriter) begin(ctx context.Context, in storage.WriteIntent) (*os.Root, func(), error) {
	if err := w.validateIntent(in); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if w.driver.lifetime.Err() != nil {
		return nil, nil, storage.NewError("closed", in.Target, nil)
	}
	root, err := w.root(ctx)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := stagedLock(root, intentKey(in.ID)+".lock")
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	release, err := w.driver.acquire(ctx)
	if err != nil {
		unlock()
		root.Close()
		return nil, nil, err
	}
	return root, func() { unlock(); root.Close(); release() }, nil
}
func checkpoint(ctx context.Context, cp storage.Checkpoint, b []byte) error {
	if cp == nil {
		return storage.NewError("invalid_checkpoint", "", nil)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return cp(ctx, b)
}
func noEffect(err error) error { return &storage.NoFinalEffect{Cause: err} }

// Never create implicit parents; reuse exact-spelling and symlink protections.
func (w *StagedWriter) destination(ctx context.Context, in storage.WriteIntent) (*os.Root, string, error) {
	parent, name := splitPath(in.Target)
	root, err := w.driver.directory(ctx, parent)
	if err != nil {
		if w.replacement && storage.IsCode(err, "not_found") {
			err = storage.NewError("parent_missing", in.Target, err)
		}
		return nil, "", err
	}
	pi, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, "", err
	}
	si, err := w.driver.system.Stat(".")
	if err != nil {
		root.Close()
		return nil, "", err
	}
	if !stagedSameFilesystem(pi, si) {
		root.Close()
		return nil, "", storage.NewError("unsupported", in.Target, nil)
	}
	local, err := stagedLocalFilesystem(root)
	if err != nil {
		root.Close()
		return nil, "", err
	}
	if !local {
		root.Close()
		return nil, "", storage.NewError("unsupported", in.Target, nil)
	}
	_, err = w.driver.find(ctx, root, parent, name)
	if !in.Conditions.Absent {
		if err != nil {
			root.Close()
			if storage.IsCode(err, "not_found") {
				err = storage.NewError("changed", in.Target, err)
			}
			return nil, "", err
		}
		mw := &MutationDriver{StagedWriter: w}
		e, err := mw.nativeAt(ctx, root, parent, name)
		if err != nil {
			root.Close()
			return nil, "", err
		}
		if e.Kind != storage.File || !bytes.Equal(e.Revision, in.Conditions.ExpectedRevision) {
			root.Close()
			return nil, "", storage.NewError("changed", in.Target, nil)
		}
		return root, name, nil
	}
	if err == nil {
		root.Close()
		return nil, "", storage.NewError("precondition_failed", in.Target, nil)
	}
	if !storage.IsCode(err, "not_found") {
		root.Close()
		return nil, "", err
	}
	return root, name, nil
}

type stageReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r stageReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(b)
	if ce := r.ctx.Err(); ce != nil {
		return n, ce
	}
	return n, err
}

// Stage closes an io.ReadCloser only to unblock cancellation or driver shutdown.
// Callers supplying a potentially blocking non-closable Reader must arrange for
// its Read to return on cancellation; no goroutine can safely interrupt arbitrary
// user code. In-memory Readers can be supplied directly. No payload is published
// while receiving bytes, including a canceled or interrupted attempt.
func (w *StagedWriter) Stage(ctx context.Context, in storage.WriteIntent, body io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	ctx, cancel := context.WithCancel(ctx)
	stopLifetime := context.AfterFunc(w.driver.lifetime, cancel)
	defer func() { stopLifetime(); cancel() }()
	if closer, ok := body.(io.ReadCloser); ok {
		var once sync.Once
		stopBody := context.AfterFunc(ctx, func() { once.Do(func() { _ = closer.Close() }) })
		defer stopBody()
	}
	in = cloneIntent(in)
	root, done, err := w.begin(ctx, in)
	if err != nil {
		return storage.Staged{}, err
	}
	defer done()
	r, err := w.load(root, in)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return storage.Staged{}, err
	}
	if r != nil {
		if r.Phase == "ready" {
			f, err := w.verifyStage(ctx, root, r)
			if err != nil {
				return storage.Staged{}, err
			}
			f.Close()
			if body == nil {
				return storage.Staged{}, storage.NewError("invalid_body", in.Target, nil)
			}
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(stageReader{ctx: ctx, reader: body}, in.Size))
			if err != nil {
				return storage.Staged{}, err
			}
			var extra [1]byte
			got, end := (stageReader{ctx: ctx, reader: body}).Read(extra[:])
			if n != in.Size || got != 0 || end != io.EOF || !bytes.Equal(h.Sum(nil), r.Digest.Value) {
				return storage.Staged{}, storage.NewError("intent_conflict", in.Target, nil)
			}
			s := w.staged(r)
			return s, checkpoint(ctx, cp, s.Handle)
		}
		return storage.Staged{}, storage.NewError("invalid_state", in.Target, nil)
	}
	dest, _, err := w.destination(ctx, in)
	if err != nil {
		return storage.Staged{}, err
	}
	dest.Close()
	if body == nil {
		return storage.Staged{}, storage.NewError("invalid_body", in.Target, nil)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return storage.Staged{}, err
	}
	r = &stageLedger{Version: 1, Binding: w.driver.binding, Intent: in, Attempt: hex.EncodeToString(random[:]), Phase: "reserved"}
	if err := w.save(root, r); err != nil {
		return storage.Staged{}, err
	}
	// Durably checkpoint the identity BEFORE allocating payload storage.
	if err := checkpoint(ctx, cp, w.receipt(r)); err != nil {
		return storage.Staged{}, err
	}
	f, err := stagedCreate(root, dataName(r))
	if err != nil {
		return storage.Staged{}, err
	}
	defer f.Close()
	h := sha256.New()
	reader := stageReader{ctx: ctx, reader: body}
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(reader, in.Size))
	if err == nil && n != in.Size {
		err = storage.NewError("size_mismatch", in.Target, nil)
	}
	if err == nil {
		var extra [1]byte
		got, e := reader.Read(extra[:])
		if got != 0 {
			err = storage.NewError("size_mismatch", in.Target, nil)
		} else if e != io.EOF {
			if e == nil {
				err = io.ErrNoProgress
			} else {
				err = e
			}
		}
	}
	if err == nil && in.Digest != nil && !bytes.Equal(in.Digest.Value, h.Sum(nil)) {
		err = storage.NewError("digest_mismatch", in.Target, nil)
	}
	if err != nil {
		r.Phase = "failed"
		return storage.Staged{}, errors.Join(err, w.save(root, r))
	}
	if in.SourceModifiedAt != nil {
		if err := root.Chtimes(dataName(r), *in.SourceModifiedAt, *in.SourceModifiedAt); err != nil {
			return storage.Staged{}, err
		}
	}
	if err := f.Sync(); err != nil {
		return storage.Staged{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return storage.Staged{}, err
	}
	if err := syncRoot(root); err != nil {
		return storage.Staged{}, err
	}
	r.Identity = rootIdentity(info)
	r.Digest = &storage.Digest{Algorithm: "sha-256", Value: h.Sum(nil)}
	r.Phase = "ready"
	if err := w.save(root, r); err != nil {
		return storage.Staged{}, err
	}
	s := w.staged(r)
	return s, checkpoint(ctx, cp, s.Handle)
}
func (w *StagedWriter) verifyStage(ctx context.Context, root *os.Root, r *stageLedger) (*os.File, error) {
	f, err := openReadOnly(root, dataName(r))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != r.Intent.Size || r.Identity == "" || rootIdentity(info) != r.Identity || r.Digest == nil || r.Digest.Algorithm != "sha-256" || len(r.Digest.Value) != sha256.Size {
		f.Close()
		return nil, storage.NewError("invalid_evidence", r.Intent.Target, err)
	}
	h := sha256.New()
	n, err := io.Copy(h, stageReader{ctx: ctx, reader: f})
	if err != nil || n != r.Intent.Size || !bytes.Equal(h.Sum(nil), r.Digest.Value) {
		f.Close()
		return nil, storage.NewError("digest_mismatch", r.Intent.Target, err)
	}
	return f, nil
}
func (w *StagedWriter) Publish(ctx context.Context, s storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
	in := cloneIntent(s.Intent)
	root, done, err := w.begin(ctx, in)
	if err != nil {
		// Admission failure does not prove an older worker for this intent is
		// quiescent. Recovery must obtain the OS lock before releasing its guard.
		return storage.MutationResult{}, err
	}
	defer done()
	if err := w.validateReceipt(in, s.Handle); err != nil {
		return storage.MutationResult{}, err
	}
	r, err := w.load(root, in)
	if err != nil {
		return storage.MutationResult{}, err
	}
	switch r.Phase {
	case "ready", "applied", "not_applied", "discarded", "reserved", "failed":
	default:
		return storage.MutationResult{}, storage.NewError("outcome_unknown", in.Target, nil)
	}
	var receipt stageReceipt
	if json.Unmarshal(s.Handle, &receipt) != nil || receipt.Attempt != r.Attempt || s.Size != in.Size || s.Digest == nil || r.Digest == nil || !bytes.Equal(s.Digest.Value, r.Digest.Value) || s.Digest.Algorithm != r.Digest.Algorithm {
		err := storage.NewError("invalid_evidence", in.Target, nil)
		if r.Phase == "applied" {
			return storage.MutationResult{}, err
		}
		return storage.MutationResult{}, noEffect(err)
	}
	if r.Phase == "applied" && r.Result != nil {
		return *r.Result, checkpoint(ctx, cp, w.receipt(r))
	}
	if r.Phase == "publishing" || r.Phase == "applied" {
		return storage.MutationResult{}, storage.NewError("outcome_unknown", in.Target, nil)
	}
	if r.Phase != "ready" {
		return storage.MutationResult{}, noEffect(storage.NewError("invalid_state", in.Target, nil))
	}
	staged, err := w.verifyStage(ctx, root, r)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	defer staged.Close()
	dest, name, err := w.destination(ctx, in)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	defer dest.Close()
	pi, err := dest.Stat(".")
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	r.ParentIdentity = rootIdentity(pi)
	r.Phase = "publishing"
	if err := w.save(root, r); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	// Validate the service fence while the OS lock excludes Inspect/Discard.
	if err := checkpoint(ctx, cp, w.receipt(r)); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if err := ctx.Err(); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if w.driver.lifetime.Err() != nil {
		return storage.MutationResult{}, noEffect(storage.NewError("closed", in.Target, nil))
	}
	// A fence callback can be delayed. Recheck the captured parent and immutable
	// staged bytes after it returns, before issuing the sole native final call.
	parent, _ := splitPath(in.Target)
	current, err := w.driver.directory(ctx, parent)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	currentInfo, err := current.Stat(".")
	current.Close()
	if err != nil || rootIdentity(currentInfo) != r.ParentIdentity {
		return storage.MutationResult{}, noEffect(storage.NewError("changed", in.Target, err))
	}
	verified, err := w.verifyStage(ctx, root, r)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	verified.Close()
	// Recheck target conditions after the potentially delayed fence callback.
	checked, _, err := w.destination(ctx, in)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	checked.Close()
	var finalErr error
	if in.Conditions.Absent {
		finalErr = w.link(root, dataName(r), dest, name)
	} else {
		finalErr = w.replace(root, dataName(r), dest, name, true)
	}
	if err := finalErr; err != nil {
		if errors.Is(err, os.ErrExist) {
			// Native no-replace rejected this effect. All other errors retain the
			// publishing ledger until quiescent identity-based inspection.
			r.Phase = "not_applied"
			err = storage.NewError("precondition_failed", in.Target, err)
			return storage.MutationResult{}, errors.Join(err, w.save(root, r))
		}
		return storage.MutationResult{}, err
	}
	// After link, errors never prove rollback. Recovery verifies inode and digest.
	if err := syncRoot(dest); err != nil {
		return storage.MutationResult{}, err
	}
	info, err := staged.Stat()
	if err != nil {
		return storage.MutationResult{}, err
	}
	e, err := entry(in.Target, info)
	if err != nil {
		return storage.MutationResult{}, err
	}
	e.MediaType = in.MediaType
	e.Digest = cloneDigest(r.Digest)
	if w.replacement {
		e = nativeVersion(e, info)
	}
	r.Phase = "applied"
	result := storage.MutationResult{Entry: &e, Evidence: w.receipt(r)}
	r.Result = &result
	if err := w.save(root, r); err != nil {
		return result, err
	}
	return result, checkpoint(ctx, cp, w.receipt(r))
}
func (w *StagedWriter) Inspect(ctx context.Context, in storage.WriteIntent, evidence []byte) (storage.Inspection, error) {
	in = cloneIntent(in)
	if err := w.validateReceipt(in, evidence); err != nil {
		return storage.Inspection{}, err
	}
	root, done, err := w.begin(ctx, in)
	if errors.Is(err, errStagedLocked) {
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}
	if err != nil {
		return storage.Inspection{}, err
	}
	defer done()
	r, err := w.load(root, in)
	if errors.Is(err, os.ErrNotExist) {
		// Tombstone workers that have not acquired the OS lock yet. A receipt
		// without its native ledger may indicate restored/lost system material;
		// retain an unresolved guard rather than claiming rollback in that case.
		r = &stageLedger{Version: 1, Binding: w.driver.binding, Intent: in, Phase: "not_applied"}
		if len(evidence) != 0 {
			var receipt stageReceipt
			_ = json.Unmarshal(evidence, &receipt)
			r.Attempt = receipt.Attempt
			r.Phase = "unresolved"
		}
		if err := w.save(root, r); err != nil {
			return storage.Inspection{}, err
		}
	} else if err != nil {
		return storage.Inspection{}, err
	}
	inspection := storage.Inspection{Evidence: w.receipt(r)}
	switch r.Phase {
	case "ready":
		f, err := w.verifyStage(ctx, root, r)
		if err != nil {
			return storage.Inspection{State: storage.OutcomeUnknown, Evidence: w.receipt(r)}, nil
		}
		f.Close()
		s := w.staged(r)
		inspection.State = storage.StagedReady
		inspection.Stage = &s
	case "publishing", "applied":
		return w.inspectFinal(ctx, root, r)
	case "reserved", "failed":
		r.Phase = "not_applied"
		if err := w.save(root, r); err != nil {
			return storage.Inspection{}, err
		}
		inspection.State = storage.NotApplied
		inspection.Evidence = w.receipt(r)
	case "not_applied", "discarded":
		inspection.State = storage.NotApplied
	default:
		inspection.State = storage.OutcomeUnknown
	}
	return inspection, nil
}
func (w *StagedWriter) inspectFinal(ctx context.Context, root *os.Root, r *stageLedger) (storage.Inspection, error) {
	unknown := storage.Inspection{State: storage.OutcomeUnknown, Evidence: w.receipt(r)}
	parent, name := splitPath(r.Intent.Target)
	dest, err := w.driver.directory(ctx, parent)
	if err != nil {
		return unknown, nil
	}
	defer dest.Close()
	pi, err := dest.Stat(".")
	if err != nil || rootIdentity(pi) != r.ParentIdentity {
		return unknown, nil
	}
	found, err := w.driver.find(ctx, dest, parent, name)
	if storage.IsCode(err, "not_found") && r.Phase == "publishing" {
		r.Phase = "not_applied"
		if err := w.save(root, r); err != nil {
			return unknown, err
		}
		return storage.Inspection{State: storage.NotApplied, Evidence: w.receipt(r)}, nil
	}
	if r.Phase == "publishing" && !r.Intent.Conditions.Absent && err == nil {
		mw := &MutationDriver{StagedWriter: w}
		current, verifyErr := mw.nativeAt(ctx, dest, parent, name)
		if verifyErr == nil && bytes.Equal(current.Revision, r.Intent.Conditions.ExpectedRevision) {
			r.Phase = "not_applied"
			if err := w.save(root, r); err != nil {
				return unknown, err
			}
			return storage.Inspection{State: storage.NotApplied, Evidence: w.receipt(r)}, nil
		}
	}
	if err != nil || found.entry.Kind != storage.File || rootIdentity(found.info) != r.Identity {
		return unknown, nil
	}
	f, err := openReadOnly(dest, name)
	if err != nil {
		return unknown, nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(found.info, info) || info.Size() != r.Intent.Size || r.Digest == nil {
		return unknown, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, stageReader{ctx: ctx, reader: f})
	if err != nil {
		return unknown, err
	}
	if n != r.Intent.Size || !bytes.Equal(h.Sum(nil), r.Digest.Value) {
		return unknown, nil
	}
	if err := syncRoot(dest); err != nil {
		return unknown, err
	}
	if r.Phase == "applied" && r.Result != nil {
		return storage.Inspection{State: storage.Applied, Result: r.Result, Evidence: w.receipt(r)}, nil
	}
	e, err := entry(r.Intent.Target, info)
	if err != nil {
		return unknown, err
	}
	e.MediaType = r.Intent.MediaType
	e.Digest = cloneDigest(r.Digest)
	if w.replacement {
		e = nativeVersion(e, info)
	}
	r.Phase = "applied"
	result := storage.MutationResult{Entry: &e, Evidence: w.receipt(r)}
	r.Result = &result
	if err := w.save(root, r); err != nil {
		return unknown, err
	}
	return storage.Inspection{State: storage.Applied, Result: &result, Evidence: w.receipt(r)}, nil
}
func (w *StagedWriter) Discard(ctx context.Context, in storage.WriteIntent, evidence []byte) error {
	if err := w.validateReceipt(in, evidence); err != nil {
		return err
	}
	root, done, err := w.begin(ctx, in)
	if err != nil {
		return err
	}
	defer done()
	r, err := w.load(root, in)
	if errors.Is(err, os.ErrNotExist) {
		if len(evidence) != 0 {
			return storage.NewError("outcome_unknown", in.Target, nil)
		}
		r = &stageLedger{Version: 1, Binding: w.driver.binding, Intent: cloneIntent(in), Phase: "discarded"}
	} else if err != nil {
		return err
	}
	switch r.Phase {
	case "ready", "applied", "not_applied", "discarded", "reserved", "failed":
	default:
		return storage.NewError("outcome_unknown", in.Target, nil)
	}
	// Preserve applied evidence. Fence other workers before removing known data.
	if r.Phase != "applied" {
		r.Phase = "discarded"
	}
	if err := w.save(root, r); err != nil {
		return err
	}
	if r.Attempt != "" {
		if err := root.Remove(dataName(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncRoot(root)
}

// Unsupported mkdir/move/copy/remove recovery never speculates about outcomes.
func (w *StagedWriter) InspectEffect(ctx context.Context, in storage.EffectIntent, evidence []byte) (storage.Inspection, error) {
	if in.Action != "publish" {
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}
	if in.Source != "" || len(in.SourceRevision) != 0 {
		return storage.Inspection{}, storage.NewError("intent_conflict", in.Target, nil)
	}
	if !validIdentity(in.ID) {
		return storage.Inspection{}, storage.NewError("invalid_intent", in.Target, nil)
	}
	root, err := w.root(ctx)
	if err != nil {
		return storage.Inspection{}, err
	}
	defer root.Close()
	// Inspect below obtains the actual lock and rereads before deciding outcomes.
	f, err := openReadOnly(root, intentKey(in.ID)+".json")
	if err != nil {
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}
	defer f.Close()
	var r stageLedger
	if json.NewDecoder(io.LimitReader(f, 65537)).Decode(&r) != nil || r.Intent.ID != in.ID || r.Intent.Target != in.Target || r.Intent.Conditions.Absent != in.Conditions.Absent || !bytes.Equal(r.Intent.Conditions.ExpectedRevision, in.Conditions.ExpectedRevision) {
		return storage.Inspection{}, storage.NewError("intent_conflict", in.Target, nil)
	}
	return w.Inspect(ctx, r.Intent, evidence)
}
