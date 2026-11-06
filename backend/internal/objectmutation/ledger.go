package objectmutation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type ledger struct {
	Format        int
	Binding       string
	Kind          string
	ID            string
	Intent        storage.WriteIntent
	Effect        *storage.EffectIntent
	Attempt       string
	Phase         string
	Digest        *storage.Digest
	ParentVersion []byte
	ExpectedETag  string
	Result        *storage.MutationResult
	MetadataReady bool
	Native        *nativeStage
	// Provider marks a copy performed inside the storage provider, which has no
	// spool payload and resolves a lost reply from the target's intent marker.
	Provider bool
}
type receipt struct {
	Format                            int
	Binding, Kind, ID, Attempt, Phase string
}

func keyFor(kind, id string) string {
	h := sha256.Sum256([]byte(kind + "\x00" + id))
	return hex.EncodeToString(h[:])
}
func (d *Driver) receipt(r *ledger) []byte {
	b, _ := json.Marshal(receipt{1, d.namespace, r.Kind, r.ID, r.Attempt, r.Phase})
	return b
}
func validAttempt(attempt string) bool {
	b, err := hex.DecodeString(attempt)
	return attempt == "" || err == nil && len(b) == 16 && hex.EncodeToString(b) == attempt
}
func (d *Driver) validateReceipt(kind, id string, evidence []byte) error {
	if len(evidence) == 0 {
		return nil
	}
	var r receipt
	if len(evidence) > 8192 || json.Unmarshal(evidence, &r) != nil || r.Format != 1 || r.Binding != d.namespace || r.Kind != kind || r.ID != id || !validAttempt(r.Attempt) {
		return fail("invalid_evidence", "")
	}
	return nil
}
func (d *Driver) reconcileReceipt(r *ledger, evidence []byte) error {
	if len(evidence) == 0 {
		return nil
	}
	var prior receipt
	if json.Unmarshal(evidence, &prior) != nil {
		return fail("invalid_evidence", r.Intent.Target)
	}
	// Applied is justified only by this attempt's durably saved sole RPC response.
	// Every other mismatch with evidence of final I/O is a permanent unknown fence.
	final := prior.Phase == "publishing" || prior.Phase == "unknown" || prior.Phase == "applied"
	mismatch := prior.Attempt != r.Attempt
	regressed := final && r.Phase != "applied" && r.Phase != "publishing" && r.Phase != "unknown"
	if mismatch || regressed {
		r.Phase = "unknown"
		if err := d.save(r); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) dataName(r *ledger) string { return keyFor(r.Kind, r.ID) + "-" + r.Attempt + ".data" }
func (d *Driver) load(kind, id string) (*ledger, error) {
	file, err := spoolOpen(d.root, keyFor(kind, id)+".json", os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, fail("invalid_evidence", "")
	}
	var r ledger
	decoder := json.NewDecoder(io.LimitReader(file, (4<<20)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF || r.Format != 1 || r.Binding != d.namespace || r.Kind != kind || r.ID != id || !validAttempt(r.Attempt) {
		return nil, fail("invalid_evidence", "")
	}
	return &r, nil
}
func (d *Driver) save(r *ledger) error {
	r.Format = 1
	r.Binding = d.namespace
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return fail("invalid_evidence", r.Intent.Target)
	}
	attempt, err := randomAttempt()
	if err != nil {
		return err
	}
	name := keyFor(r.Kind, r.ID) + "-record-" + attempt + ".tmp"
	file, err := spoolOpen(d.root, name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// Temp names are created locally and removed only after verifying this exact
	// constant namespace; they are never derived from a native content path.
	defer d.root.Remove(name)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = d.root.Rename(name, keyFor(r.Kind, r.ID)+".json"); err != nil {
		return err
	}
	return syncDirectory(d.root)
}
func syncDirectory(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
func privateRoot(base *os.Root, name string) (*os.Root, error) {
	before, err := base.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode().Perm()&0077 != 0 {
		return nil, fail("permission_denied", "")
	}
	pinned, err := spoolOpen(base, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer pinned.Close()
	info, err := pinned.Stat()
	if err != nil || !os.SameFile(before, info) {
		return nil, fail("invalid_config", "")
	}
	root, err := base.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, fail("invalid_config", "")
	}
	return root, nil
}
func (d *Driver) lock(ctx context.Context, kind, id string, try bool) (func(), error) {
	if id == "" || len(id) > 1024 || strings.ContainsAny(id, "\r\n\x00") {
		return nil, fail("invalid_intent", "")
	}
	file, err := spoolOpen(d.root, keyFor(kind, id)+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, fail("invalid_evidence", "")
	}
	for {
		if err = ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		locked, err := lockFile(file)
		if err != nil {
			file.Close()
			return nil, err
		}
		if locked {
			return func() { _ = unlockFile(file); _ = file.Close() }, nil
		}
		if try {
			file.Close()
			return nil, fail("outcome_unknown", "")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *Driver) Inspect(ctx context.Context, in storage.WriteIntent, evidence []byte) (storage.Inspection, error) {
	if err := validateRecoveryIntent(in); err != nil {
		return storage.Inspection{}, err
	}
	return d.inspect(ctx, "write", in.ID, &in, nil, evidence)
}
func (d *Driver) inspect(ctx context.Context, kind, id string, in *storage.WriteIntent, effect *storage.EffectIntent, evidence []byte) (storage.Inspection, error) {
	if err := d.validateReceipt(kind, id, evidence); err != nil {
		return storage.Inspection{}, err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Inspection{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, kind, id, true)
	if storage.IsCode(err, "outcome_unknown") {
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}
	if err != nil {
		return storage.Inspection{}, err
	}
	defer unlock()
	r, err := d.load(kind, id)
	if errors.Is(err, os.ErrNotExist) {
		r = &ledger{Binding: d.namespace, Kind: kind, ID: id, Phase: "not_applied"}
		if in != nil {
			r.Intent = cloneIntent(*in)
		}
		if effect != nil {
			r.Effect = cloneEffect(*effect)
		}
		// A receipt surviving a lost/restored spool disproves the assumption that no
		// previous request existed. Keep a permanent unresolved fence instead.
		if len(evidence) > 0 {
			r.Phase = "unknown"
		}
		if err = d.save(r); err != nil {
			return storage.Inspection{}, err
		}
	} else if err != nil {
		return storage.Inspection{}, err
	}
	if in != nil && !sameJSON(r.Intent, *in) || effect != nil && !sameJSON(r.Effect, effect) {
		return storage.Inspection{}, fail("intent_conflict", r.Intent.Target)
	}
	if err = d.reconcileReceipt(r, evidence); err != nil {
		return storage.Inspection{}, err
	}
	switch r.Phase {
	case "applied":
		if err := d.refreshApplied(ctx, r); err != nil {
			return storage.Inspection{}, err
		}
		if r.Result == nil || len(r.Result.Evidence) == 0 {
			return storage.Inspection{}, fail("invalid_evidence", r.Intent.Target)
		}
		result := cloneResult(*r.Result)
		return storage.Inspection{State: storage.Applied, Result: &result, Evidence: d.receipt(r)}, nil
	case "publishing", "unknown":
		if r.Provider {
			resolved, ok, err := d.resolveCopied(ctx, r)
			if err != nil {
				return storage.Inspection{}, err
			}
			if ok {
				return storage.Inspection{State: storage.Applied, Result: resolved, Evidence: d.receipt(r)}, nil
			}
			if r.Phase == "not_applied" {
				return storage.Inspection{State: storage.NotApplied, Evidence: d.receipt(r)}, nil
			}
		}
		return storage.Inspection{State: storage.OutcomeUnknown, Evidence: d.receipt(r)}, nil
	case "ready":
		if r.Native == nil && !r.Provider {
			file, err := d.verifiedSpool(ctx, r)
			if err != nil {
				return storage.Inspection{}, err
			}
			file.Close()
		}
		stage := d.staged(r)
		return storage.Inspection{State: storage.StagedReady, Stage: &stage, Evidence: d.receipt(r)}, nil
	case "receiving", "new":
		// Holding the per-intent lock proves the former local receiver has stopped.
		// No remote final request may be sent before publishing was durably recorded.
		r.Phase = "not_applied"
		if err = d.save(r); err != nil {
			return storage.Inspection{}, err
		}
		fallthrough
	case "not_applied", "discarded":
		return storage.Inspection{State: storage.NotApplied, Evidence: d.receipt(r)}, nil
	default:
		return storage.Inspection{}, fail("invalid_evidence", r.Intent.Target)
	}
}
func (d *Driver) Discard(ctx context.Context, in storage.WriteIntent, evidence []byte) error {
	if err := validateRecoveryIntent(in); err != nil {
		return err
	}
	return d.discard(ctx, "write", in.ID, &in, nil, evidence)
}
func (d *Driver) discard(ctx context.Context, kind, id string, in *storage.WriteIntent, effect *storage.EffectIntent, evidence []byte) error {
	if err := d.validateReceipt(kind, id, evidence); err != nil {
		return err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return err
	}
	defer done()
	unlock, err := d.lock(ctx, kind, id, true)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := d.load(kind, id)
	if errors.Is(err, os.ErrNotExist) {
		r = &ledger{Kind: kind, ID: id, Phase: "discarded"}
		if in != nil {
			r.Intent = cloneIntent(*in)
		}
		if effect != nil {
			r.Effect = cloneEffect(*effect)
		}
		if len(evidence) > 0 {
			r.Phase = "unknown"
			if e := d.save(r); e != nil {
				return e
			}
			return fail("outcome_unknown", r.Intent.Target)
		}
	} else if err != nil {
		return err
	}
	if in != nil && !sameJSON(r.Intent, *in) || effect != nil && !sameJSON(r.Effect, effect) {
		return fail("intent_conflict", r.Intent.Target)
	}
	if err = d.reconcileReceipt(r, evidence); err != nil {
		return err
	}
	if r.Phase == "publishing" || r.Phase == "unknown" {
		return fail("outcome_unknown", r.Intent.Target)
	}
	if r.Phase != "applied" {
		r.Phase = "discarded"
	}
	// Seal before unlinking so queued Stage/Publish cannot create new allocations.
	if err = d.save(r); err != nil {
		return err
	}
	if r.Attempt != "" {
		if err = d.root.Remove(d.dataName(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDirectory(d.root)
}
func cloneEffect(in storage.EffectIntent) *storage.EffectIntent {
	in.SourceVersion = bytes.Clone(in.SourceVersion)
	in.Conditions.ExpectedVersion = bytes.Clone(in.Conditions.ExpectedVersion)
	return &in
}

func (d *Driver) Mkdir(ctx context.Context, in storage.Mutation, cp storage.Checkpoint) (storage.MutationResult, error) {
	if in.Path == "" || !in.Conditions.Absent || len(in.Conditions.ExpectedVersion) > 0 {
		return storage.MutationResult{}, fail("unsupported", in.Path)
	}
	intent := storage.WriteIntent{ID: in.ID, Target: in.Path, Conditions: in.Conditions, Size: 0, MediaType: "application/octet-stream"}
	if err := d.validateIntent(intent); err != nil {
		return storage.MutationResult{}, err
	}
	effect := storage.EffectIntent{ID: in.ID, Action: "mkdir", Target: in.Path, Conditions: in.Conditions}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "mkdir", in.ID, false)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer unlock()
	r, err := d.load("mkdir", in.ID)
	if errors.Is(err, os.ErrNotExist) {
		r = &ledger{Kind: "mkdir", ID: in.ID, Intent: intent, Effect: &effect, Phase: "new"}
	} else if err != nil {
		return storage.MutationResult{}, err
	}
	if !sameJSON(r.Intent, intent) || !sameJSON(r.Effect, &effect) {
		return storage.MutationResult{}, fail("intent_conflict", in.Path)
	}
	if r.Phase == "new" {
		if _, err = d.stageLocked(ctx, r, bytes.NewReader(nil), cp); err != nil {
			return storage.MutationResult{}, err
		}
	}
	result, err := d.publishLocked(ctx, r, cp)
	if err == nil {
		err = d.removeKnownData(r)
	}
	return result, err
}
func (d *Driver) removeKnownData(r *ledger) error {
	if r.Attempt != "" {
		if err := d.root.Remove(d.dataName(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return syncDirectory(d.root)
}
func (d *Driver) Move(context.Context, storage.MoveRequest, storage.Checkpoint) (storage.MutationResult, error) {
	return storage.MutationResult{}, &storage.NoFinalEffect{Cause: fail("unsupported", "")}
}
func (d *Driver) Copy(ctx context.Context, in storage.MoveRequest, cp storage.Checkpoint) (storage.MutationResult, error) {
	if in.Source == "" || in.Target == "" || in.Source == in.Target || storage.ValidatePath(in.Source) != nil || !in.TargetConditions.Absent || len(in.TargetConditions.ExpectedVersion) > 0 {
		return storage.MutationResult{}, fail("unsupported", in.Target)
	}
	etag, err := parseVersion(in.SourceVersion)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if copier, ok := d.backend.(CopyBackend); ok {
		return d.copyProvider(ctx, in, etag, cp, copier)
	}
	return d.copyRelay(ctx, in, etag, cp)
}

// copyRelay streams the source through the private spool and publishes a single
// PUT. It stays the fallback for backends without a provider-side copy.
func (d *Driver) copyRelay(ctx context.Context, in storage.MoveRequest, etag string, cp storage.Checkpoint) (storage.MutationResult, error) {
	intent := storage.WriteIntent{ID: in.ID, Target: in.Target, Conditions: in.TargetConditions, MediaType: "application/octet-stream"}
	effect := storage.EffectIntent{ID: in.ID, Action: "copy", Source: in.Source, Target: in.Target, Conditions: in.TargetConditions, SourceVersion: bytes.Clone(in.SourceVersion)}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "copy", in.ID, false)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer unlock()
	r, err := d.load("copy", in.ID)
	if err == nil {
		if !sameJSON(r.Effect, &effect) {
			return storage.MutationResult{}, fail("intent_conflict", in.Target)
		}
		if r.Phase == "applied" {
			if r.Result == nil {
				return storage.MutationResult{}, fail("invalid_evidence", in.Target)
			}
			if err := d.refreshApplied(ctx, r); err != nil {
				return storage.MutationResult{}, err
			}
			return cloneResult(*r.Result), nil
		}
		if r.Phase != "ready" {
			return storage.MutationResult{}, fail("outcome_unknown", in.Target)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		source, err := d.Stat(ctx, in.Source)
		if err != nil {
			return storage.MutationResult{}, err
		}
		if source.Kind != storage.File || !bytes.Equal(source.Version, in.SourceVersion) {
			return storage.MutationResult{}, fail("changed", in.Source)
		}
		if source.Size > d.options.MaxPublishBytes {
			return storage.MutationResult{}, fail("unsupported", in.Source)
		}
		intent.Size = source.Size
		intent.MediaType = source.MediaType
		if err = d.validateIntent(intent); err != nil {
			return storage.MutationResult{}, err
		}
		r = &ledger{Kind: "copy", ID: in.ID, Intent: intent, Effect: &effect, Phase: "new"}
		body, err := d.backend.Open(ctx, d.key(in.Source, storage.File), etag)
		if err != nil {
			return storage.MutationResult{}, err
		}
		_, err = d.stageLocked(ctx, r, body, cp)
		closeErr := body.Close()
		if err != nil {
			return storage.MutationResult{}, err
		}
		if closeErr != nil {
			return storage.MutationResult{}, closeErr
		}
	} else {
		return storage.MutationResult{}, err
	}
	result, err := d.publishLocked(ctx, r, cp)
	if err == nil {
		err = d.removeKnownData(r)
	}
	return result, err
}

// singleCopyLimit is the largest object one provider copy request moves: COS and
// S3 both document 5 GiB for PutObject - Copy. Larger objects use numbered part
// copies, which completion still publishes with one conditional request.
const singleCopyLimit int64 = 5 << 30

// copyPartBytes keeps every part below the provider's 5 GiB part limit and a
// 5 TiB object below the 10,000-part maximum.
const copyPartBytes int64 = 512 << 20

// copyProvider copies through the provider itself, so no source or target bytes
// cross the gateway. The ledger records the intent before the request; a lost
// reply is resolved from the intent marker this exact request wrote, never by
// replaying the copy.
func (d *Driver) copyProvider(ctx context.Context, in storage.MoveRequest, sourceETag string, cp storage.Checkpoint, copier CopyBackend) (storage.MutationResult, error) {
	effect := storage.EffectIntent{ID: in.ID, Action: "copy", Source: in.Source, Target: in.Target, Conditions: in.TargetConditions, SourceVersion: bytes.Clone(in.SourceVersion)}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "copy", in.ID, false)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer unlock()
	r, err := d.load("copy", in.ID)
	if err == nil {
		if !sameJSON(r.Effect, &effect) {
			return storage.MutationResult{}, fail("intent_conflict", in.Target)
		}
		if r.Phase == "applied" {
			if r.Result == nil {
				return storage.MutationResult{}, fail("invalid_evidence", in.Target)
			}
			if err := d.refreshApplied(ctx, r); err != nil {
				return storage.MutationResult{}, err
			}
			return cloneResult(*r.Result), nil
		}
		if r.Phase == "publishing" || r.Phase == "unknown" {
			if resolved, ok, err := d.resolveCopied(ctx, r); err != nil {
				return storage.MutationResult{}, err
			} else if ok {
				return cloneResult(*resolved), nil
			}
			return storage.MutationResult{}, fail("outcome_unknown", in.Target)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		r = &ledger{Kind: "copy", ID: in.ID, Effect: &effect, Phase: "new"}
	} else {
		return storage.MutationResult{}, err
	}
	source, err := d.Stat(ctx, in.Source)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if source.Kind != storage.File || !bytes.Equal(source.Version, in.SourceVersion) {
		return storage.MutationResult{}, fail("changed", in.Source)
	}
	if _, ok := d.backend.(PartCopyBackend); source.Size > singleCopyLimit && (!ok || !d.options.Admission.MultipartUploads) {
		// Deterministic refusal: no native request is issued, so the intent is never
		// published and the coordinator may report it without a recovery cycle.
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: fail("too_large", in.Source)}
	}
	intent := storage.WriteIntent{ID: in.ID, Target: in.Target, Conditions: in.TargetConditions, Size: source.Size, MediaType: source.MediaType}
	if err = validateRecoveryIntent(intent); err != nil {
		return storage.MutationResult{}, err
	}
	parent, err := d.Stat(ctx, parentPath(in.Target))
	if err != nil {
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: err}
	}
	if parent.Kind != storage.Folder {
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: fail("parent_missing", in.Target)}
	}
	r.Intent = cloneIntent(intent)
	r.ParentVersion = bytes.Clone(parent.Version)
	r.Provider, r.Attempt, r.Native = true, "", nil
	r.Phase = "ready"
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.MutationResult{}, err
	}
	if err = ctx.Err(); err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	// A delayed checkpoint can let the parent or the target change. Revalidate the
	// indexed view, then let the native condition refuse an occupied target.
	parent, err = d.Stat(ctx, parentPath(in.Target))
	if err == nil && !bytes.Equal(parent.Version, r.ParentVersion) {
		err = fail("changed", in.Target)
	}
	if err == nil {
		err = d.checkTarget(ctx, r)
	}
	if err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	r.Phase = "publishing"
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.MutationResult{}, err
	}
	obj, err := d.nativeCopy(ctx, r, source, sourceETag, copier)
	if err != nil {
		if storage.IsCode(err, "precondition_failed") || storage.IsCode(err, "name_conflict") || storage.IsCode(err, "not_found") || storage.IsCode(err, "changed") {
			// The provider proved no final effect, but a failed completion can leave
			// part copies unpublished; discard them before the intent is sealed.
			if r.Native != nil && r.Native.UploadID != "" {
				if multipart, ok := d.backend.(MultipartBackend); ok {
					_ = multipart.AbortMultipart(context.WithoutCancel(ctx), d.key(r.Intent.Target, storage.File), r.Native.UploadID)
				}
			}
			r.Phase = "not_applied"
			r.Native = nil
			return storage.MutationResult{}, errors.Join(err, d.save(r))
		}
		return storage.MutationResult{}, err
	}
	e, err := d.objectEntry(in.Target, storage.File, obj)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if e.Size != intent.Size {
		return storage.MutationResult{}, fail("invalid_backend_response", e.Path)
	}
	r.Phase = "applied"
	r.Native = nil
	result := storage.MutationResult{Entry: &e, Evidence: d.receipt(r)}
	r.Result = &result
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = d.refreshApplied(ctx, r); err != nil {
		return storage.MutationResult{}, err
	}
	return cloneResult(*r.Result), checkpoint(ctx, cp, d.receipt(r))
}

func (d *Driver) nativeCopy(ctx context.Context, r *ledger, source storage.Entry, sourceETag string, copier CopyBackend) (Object, error) {
	request := Copy{
		Source:       d.key(r.Effect.Source, storage.File),
		Target:       d.key(r.Intent.Target, storage.File),
		SourceETag:   sourceETag,
		Size:         r.Intent.Size,
		MediaType:    r.Intent.MediaType,
		Metadata:     map[string]string{"eam-intent": r.ID},
		Absent:       r.Intent.Conditions.Absent,
		ExpectedETag: r.ExpectedETag,
	}
	if source.Size <= singleCopyLimit {
		return copier.CopyObject(ctx, request)
	}
	// Part copies finish with one conditional completion, which only an admitted
	// multipart connection has proven.
	parts, ok := d.backend.(PartCopyBackend)
	if !ok || !d.options.Admission.MultipartUploads {
		return Object{}, fail("too_large", r.Intent.Target)
	}
	return d.copyParts(ctx, r, request, parts)
}

// copyParts copies numbered provider-side ranges. Nothing is visible until one
// conditional completion, so a failure discards the unpublished session and
// reports no final effect; a failed completion stays ambiguous for recovery.
func (d *Driver) copyParts(ctx context.Context, r *ledger, request Copy, parts PartCopyBackend) (Object, error) {
	uploadID, err := parts.BeginMultipart(ctx, Put{Key: request.Target, Size: request.Size, MediaType: request.MediaType, Metadata: request.Metadata})
	if err != nil {
		return Object{}, err
	}
	r.Native = &nativeStage{UploadID: uploadID}
	if err = d.save(r); err != nil {
		_ = parts.AbortMultipart(context.WithoutCancel(ctx), request.Target, uploadID)
		return Object{}, err
	}
	var done []NativePart
	for offset, number := int64(0), 1; offset < request.Size; number++ {
		length := min(copyPartBytes, request.Size-offset)
		tag, err := parts.CopyPart(ctx, request.Target, uploadID, number, request.Source, request.SourceETag, offset, length)
		if err != nil {
			if abortErr := parts.AbortMultipart(context.WithoutCancel(ctx), request.Target, uploadID); abortErr == nil {
				r.Native = nil
				_ = d.save(r)
				return Object{}, &storage.NoFinalEffect{Cause: err}
			}
			return Object{}, err
		}
		done = append(done, NativePart{Number: number, ETag: tag})
		offset += length
	}
	obj, err := parts.CompleteMultipart(ctx, Put{Key: request.Target, Size: request.Size, MediaType: request.MediaType, Metadata: request.Metadata, Absent: request.Absent}, uploadID, done)
	if err != nil {
		return Object{}, err
	}
	r.Native = nil
	if err = d.save(r); err != nil {
		return Object{}, err
	}
	return obj, nil
}

// resolveCopied attributes a provider-side copy whose reply was lost. The
// target's intent metadata is written by this exact conditional request, so it
// proves the publish; a missing target proves nothing was published, and an
// uncompleted part-copy session is discarded before the copy may be retried.
func (d *Driver) resolveCopied(ctx context.Context, r *ledger) (*storage.MutationResult, bool, error) {
	obj, err := d.backend.Head(ctx, d.key(r.Intent.Target, storage.File))
	if storage.IsCode(err, "not_found") {
		if r.Native != nil && r.Native.UploadID != "" {
			multipart, ok := d.backend.(MultipartBackend)
			if !ok {
				return nil, false, nil
			}
			if abortErr := multipart.AbortMultipart(context.WithoutCancel(ctx), d.key(r.Intent.Target, storage.File), r.Native.UploadID); abortErr != nil {
				return nil, false, nil
			}
		}
		r.Native = nil
		r.Phase = "not_applied"
		return nil, false, d.save(r)
	}
	if err != nil {
		return nil, false, err
	}
	if obj.Metadata["eam-intent"] != r.ID || obj.Size != r.Intent.Size {
		return nil, false, nil
	}
	e, err := d.objectEntry(r.Intent.Target, storage.File, obj)
	if err != nil {
		return nil, false, err
	}
	r.Native = nil
	r.Phase = "applied"
	result := storage.MutationResult{Entry: &e, Evidence: d.receipt(r)}
	r.Result = &result
	if err = d.save(r); err != nil {
		return nil, false, err
	}
	return &result, true, nil
}
func (d *Driver) Remove(ctx context.Context, in storage.Mutation, cp storage.Checkpoint) (storage.MutationResult, error) {
	if in.Path == "" || storage.ValidatePath(in.Path) != nil || in.Conditions.Absent {
		return storage.MutationResult{}, fail("unsupported", in.Path)
	}
	if len(in.Conditions.ExpectedVersion) == 0 {
		return d.removeImplicitFolder(ctx, in)
	}
	if _, err := parseVersion(in.Conditions.ExpectedVersion); err != nil {
		return storage.MutationResult{}, err
	}
	effect := storage.EffectIntent{ID: in.ID, Action: "delete", Source: in.Path, SourceVersion: bytes.Clone(in.Conditions.ExpectedVersion)}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "delete", in.ID, false)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer unlock()
	r, err := d.load("delete", in.ID)
	if errors.Is(err, os.ErrNotExist) {
		r = &ledger{Kind: "delete", ID: in.ID, Intent: storage.WriteIntent{ID: in.ID, Target: in.Path, Conditions: in.Conditions}, Effect: &effect, Phase: "new"}
	} else if err != nil {
		return storage.MutationResult{}, err
	}
	if !sameJSON(r.Effect, &effect) {
		return storage.MutationResult{}, fail("intent_conflict", in.Path)
	}
	if r.Phase == "applied" && r.Result != nil {
		return cloneResult(*r.Result), nil
	}
	if r.Phase != "new" {
		return storage.MutationResult{}, fail("outcome_unknown", in.Path)
	}
	current, err := d.Stat(ctx, in.Path)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if !bytes.Equal(current.Version, in.Conditions.ExpectedVersion) {
		return storage.MutationResult{}, fail("changed", in.Path)
	}
	if current.Kind == storage.Folder {
		page, err := d.tree.List(ctx, in.Path, storage.PageRequest{Limit: 1})
		if err != nil {
			return storage.MutationResult{}, err
		}
		if len(page.Entries) > 0 || page.NextCursor != "" {
			return storage.MutationResult{}, fail("not_empty", in.Path)
		}
	}
	obj, err := d.backend.Head(ctx, d.key(in.Path, current.Kind))
	if err != nil {
		return storage.MutationResult{}, err
	}
	if !bytes.Equal(nativeVersion(obj.ETag), in.Conditions.ExpectedVersion) {
		return storage.MutationResult{}, fail("changed", in.Path)
	}
	r.ExpectedETag = obj.ETag
	r.Phase = "ready"
	r.ParentVersion = bytes.Clone(current.Version)
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.MutationResult{}, err
	}
	// Revalidate after a delayed checkpoint, before journaling the one remote call.
	current, err = d.Stat(ctx, in.Path)
	if err == nil && !bytes.Equal(current.Version, in.Conditions.ExpectedVersion) {
		err = fail("changed", in.Path)
	}
	if err == nil && current.Kind == storage.Folder {
		page, e := d.tree.List(ctx, in.Path, storage.PageRequest{Limit: 1})
		err = e
		if err == nil && (len(page.Entries) > 0 || page.NextCursor != "") {
			err = fail("not_empty", in.Path)
		}
	}
	if err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	r.Phase = "publishing"
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.MutationResult{}, err
	}
	if err = ctx.Err(); err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	latest, checkErr := d.Stat(ctx, in.Path)
	if checkErr == nil && !bytes.Equal(latest.Version, in.Conditions.ExpectedVersion) {
		checkErr = fail("changed", in.Path)
	}
	if checkErr == nil && latest.Kind == storage.Folder {
		page, e := d.tree.List(ctx, in.Path, storage.PageRequest{Limit: 1})
		checkErr = e
		if checkErr == nil && (len(page.Entries) > 0 || page.NextCursor != "") {
			checkErr = fail("not_empty", in.Path)
		}
	}
	if checkErr != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: checkErr}, d.save(r))
	}
	_, err = d.backend.Delete(ctx, d.key(in.Path, current.Kind), r.ExpectedETag)
	if err != nil {
		if storage.IsCode(err, "precondition_failed") {
			r.Phase = "not_applied"
			return storage.MutationResult{}, errors.Join(err, d.save(r))
		}
		return storage.MutationResult{}, err
	}
	// A complete sole current-key Delete response is terminal evidence. A HEAD is
	// neither used to attribute it nor to resolve an ambiguous earlier Delete.
	r.Phase = "applied"
	result := storage.MutationResult{Evidence: d.receipt(r)}
	r.Result = &result
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	return result, checkpoint(ctx, cp, d.receipt(r))
}
func (d *Driver) InspectEffect(ctx context.Context, in storage.EffectIntent, evidence []byte) (storage.Inspection, error) {
	switch in.Action {
	case "mkdir", "copy", "delete":
		if in.ID == "" {
			return storage.Inspection{}, fail("invalid_intent", "")
		}
		observed, err := d.inspect(ctx, in.Action, in.ID, nil, &in, evidence)
		if err == nil && (observed.State == storage.Applied || observed.State == storage.NotApplied) {
			unlock, lockErr := d.lock(ctx, in.Action, in.ID, true)
			if lockErr != nil {
				return storage.Inspection{State: storage.OutcomeUnknown}, nil
			}
			r, loadErr := d.load(in.Action, in.ID)
			if loadErr == nil && (r.Phase == "applied" || r.Phase == "not_applied" || r.Phase == "discarded") {
				loadErr = d.removeKnownData(r)
			} else if loadErr == nil {
				observed = storage.Inspection{State: storage.OutcomeUnknown, Evidence: d.receipt(r)}
			}
			unlock()
			if loadErr != nil {
				return storage.Inspection{}, loadErr
			}
		}
		if err == nil && observed.State == storage.StagedReady {
			// An entry started marker may precede actual remote I/O. Fence this unused
			// local stage rather than asking the coordinator to reissue the native action.
			unlock, e := d.lock(ctx, in.Action, in.ID, true)
			if e != nil {
				return storage.Inspection{State: storage.OutcomeUnknown}, nil
			}
			r, e := d.load(in.Action, in.ID)
			if e == nil && (r.Phase == "ready" || r.Phase == "new") {
				r.Phase = "not_applied"
				e = d.save(r)
				observed = storage.Inspection{State: storage.NotApplied, Evidence: d.receipt(r)}
			} else if e == nil {
				observed = storage.Inspection{State: storage.OutcomeUnknown, Evidence: d.receipt(r)}
			}
			unlock()
			if e != nil {
				return storage.Inspection{}, e
			}
			return observed, nil
		}
		return observed, err
	default:
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}
}

// removeImplicitFolder settles deletion of a prefix folder that has no marker
// object. It issues no native request: the prefix vanished with its last child.
// A marker or child that appeared since capture is reported as a change.
func (d *Driver) removeImplicitFolder(ctx context.Context, in storage.Mutation) (storage.MutationResult, error) {
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	if _, err = d.backend.Head(ctx, d.key(in.Path, storage.Folder)); err == nil {
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: fail("changed", in.Path)}
	} else if !storage.IsCode(err, "not_found") {
		return storage.MutationResult{}, err
	}
	evidence, _ := json.Marshal(map[string]string{"binding": d.namespace, "kind": "implicit-folder-delete", "id": in.ID, "path": string(in.Path)})
	page, err := d.tree.List(ctx, in.Path, storage.PageRequest{Limit: 1})
	if storage.IsCode(err, "not_found") {
		return storage.MutationResult{Evidence: evidence}, nil
	}
	if err != nil {
		return storage.MutationResult{}, err
	}
	if len(page.Entries) > 0 || page.NextCursor != "" {
		return storage.MutationResult{}, &storage.NoFinalEffect{Cause: fail("not_empty", in.Path)}
	}
	return storage.MutationResult{Evidence: evidence}, nil
}
