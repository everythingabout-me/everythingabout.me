package fileops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type ServiceOptions struct {
	Authorize Authorize
	Commit    IndexCommit
	LeaseTTL  time.Duration
	// PhysicalDelta must account for retained provider versions/copy costs on any
	// replacement strategy. Nil admits only create-only publication, charging size.
	PhysicalDelta func(Operation, storage.MutationResult) (int64, error)
}
type Service struct {
	journal Journal
	options ServiceOptions
}

func NewService(journal Journal, options ServiceOptions) (*Service, error) {
	if journal == nil || options.Authorize == nil || options.Commit == nil {
		return nil, E("invalid_config", nil)
	}
	if options.LeaseTTL == 0 {
		options.LeaseTTL = 30 * time.Second
	}
	if options.LeaseTTL < time.Second || options.LeaseTTL > 10*time.Minute {
		return nil, E("invalid_config", nil)
	}
	return &Service{journal: journal, options: options}, nil
}

func operationIntent(op Operation) (storage.WriteIntent, error) {
	var intent storage.WriteIntent
	if op.Kind != "upload_commit" || json.Unmarshal(op.Intent, &intent) != nil || intent.Size < 0 || intent.Size > 9007199254740991 || intent.Target == "" || storage.ValidatePath(intent.Target) != nil {
		return intent, E("invalid_intent", nil)
	}
	intent.ID = op.ID
	if intent.Conditions.Absent == (len(intent.Conditions.ExpectedVersion) > 0) {
		return intent, E("invalid_intent", nil)
	}
	return intent, nil
}

func validateWriterRoot(root Root, writer storage.StagedWriter) error {
	bound, ok := writer.(storage.BoundStagedWriter)
	if !ok {
		return E("unbound_writer", nil)
	}
	return validateNativeBinding(root, bound.StorageBinding(), bound.StorageNameRules())
}
func validateNativeBinding(root Root, cfg storage.Config, names storage.NameRules) error {
	if cfg.OwnerID != root.OwnerID || cfg.ConnectionID != root.ConnectionID || cfg.Generation != root.Generation || cfg.ContentRoot != root.ContentRoot || cfg.SystemRoot != root.SystemRoot || names.CaseSensitive != root.CaseSensitive {
		return E("root_conflict", nil)
	}
	return nil
}

// Write operates on an already admitted immutable intent. It never acquires
// admission implicitly from an index row or enables a backend's write capability.
func (s *Service) Write(ctx context.Context, root Root, actor Actor, id string, writer storage.StagedWriter, body io.Reader) (Operation, error) {
	if writer == nil {
		return Operation{}, E("unsupported", nil)
	}
	if err := validateWriterRoot(root, writer); err != nil {
		return Operation{}, err
	}
	op, err := s.journal.Get(ctx, root, actor, id)
	if err != nil {
		return op, err
	}
	if err = s.options.Authorize(ctx, op); err != nil {
		return op, err
	}
	intent, err := operationIntent(op)
	if err != nil {
		return op, err
	}
	if op.Terminal() {
		if body != nil {
			if err = validateReplay(ctx, body, intent, op.Stage); err != nil {
				return op, err
			}
		}
		return op, nil
	}
	if op.Phase == EffectStarted || op.Phase == NativeApplied || op.State == Unknown {
		return s.Recover(ctx, root, actor, id, writer)
	}
	if s.options.PhysicalDelta == nil && (!intent.Conditions.Absent || op.Reservation.OldBytes != 0) {
		return op, E("unsupported", nil)
	}
	op, err = s.journal.Claim(ctx, root, actor, id, s.options.LeaseTTL)
	if err != nil {
		return op, err
	}
	work, stop := s.keepLease(ctx, op.Lease)
	defer stop()
	checkpoint := bytes.Clone(op.Checkpoint)
	save := func(nativeCtx context.Context, value []byte) error {
		if err := s.journal.SaveCheckpoint(nativeCtx, op.Lease, value); err != nil {
			return err
		}
		checkpoint = bytes.Clone(value)
		return nil
	}
	if op.Phase == Receiving && op.Stage == nil {
		// The earlier receiver may still own temporary I/O. Resolve/seal its saved
		// allocation before another attempt can consume a new body for this intent.
		return s.recoverClaimed(work, root, actor, op, writer)
	}
	if op.CancelRequested {
		return s.finishBeforeEffect(work, root, actor, op, writer, checkpoint, Cancelled, nil)
	}
	stage := op.Stage
	if stage == nil {
		if body == nil {
			return op, E("body_required", nil)
		}
		received, e := writer.Stage(work, intent, body, save)
		if e != nil {
			// Stage cannot publish authoritative bytes. Discard must also fence late
			// temporary attempts; failed cleanup retains the operation and reservation.
			return s.finishBeforeEffect(work, root, actor, op, writer, checkpoint, Failed, problem("body_reception_failed", e))
		}
		if err = validateStage(intent, received); err != nil {
			return s.finishBeforeEffect(work, root, actor, op, writer, checkpoint, Failed, problem("invalid_stage", err))
		}
		if err = s.journal.SaveStage(work, op.Lease, received); err != nil {
			return op, err
		}
		stage = &received
		op.Stage = stage
		op.Phase = StagedReady
	} else if body != nil {
		if err = validateReplay(work, body, intent, stage); err != nil {
			return op, err
		}
	}
	current, err := s.journal.Get(work, root, actor, id)
	if err != nil {
		return op, err
	}
	if current.CancelRequested {
		return s.finishBeforeEffect(work, root, actor, current, writer, checkpoint, Cancelled, nil)
	}
	if err = s.options.Authorize(work, current); err != nil {
		return s.finishBeforeEffect(work, root, actor, current, writer, checkpoint, Failed, problem("authorization_changed", err))
	}
	if err = s.journal.StartEffect(work, op.Lease); err != nil {
		return op, err
	}
	op.Phase = EffectStarted
	native, err := writer.Publish(work, *stage, save)
	if err != nil {
		// Even an error claiming no new effect must be reconciled against saved
		// native evidence. Unknown earlier effects are never an automatic rollback.
		return s.recoverAfterError(ctx, root, actor, op, writer, checkpoint, err)
	}
	if err = validateNative(intent, native); err != nil {
		return s.markUnknown(ctx, root, actor, op, checkpoint, err)
	}
	persist, cancel := durableContext(ctx)
	defer cancel()
	if err = s.journal.SaveApplied(persist, op.Lease, native); err != nil {
		return op, err
	}
	op.NativeResult = &native
	op.Phase = NativeApplied
	return s.settleApplied(persist, root, actor, op, native)
}

func validateStage(intent storage.WriteIntent, stage storage.Staged) error {
	a, _ := json.Marshal(intent)
	b, _ := json.Marshal(stage.Intent)
	if !bytes.Equal(a, b) || stage.Size != intent.Size || len(stage.Handle) == 0 {
		return E("invalid_stage", nil)
	}
	if intent.Digest != nil && (stage.Digest == nil || intent.Digest.Algorithm != stage.Digest.Algorithm || !bytes.Equal(intent.Digest.Value, stage.Digest.Value)) {
		return E("checksum_mismatch", nil)
	}
	return nil
}
func validateNative(intent storage.WriteIntent, result storage.MutationResult) error {
	if result.Entry == nil || result.Entry.Path != intent.Target || result.Entry.Kind != storage.File || result.Entry.Size != intent.Size || len(result.Evidence) == 0 {
		return E("invalid_native_result", nil)
	}
	return nil
}
func validateReplay(ctx context.Context, body io.Reader, intent storage.WriteIntent, stage *storage.Staged) error {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(&contextReader{ctx: ctx, reader: body}, intent.Size+1))
	if err != nil {
		return err
	}
	if n != intent.Size {
		return E("length_mismatch", nil)
	}
	digest := h.Sum(nil)
	if intent.Digest != nil && (intent.Digest.Algorithm != "sha-256" || !bytes.Equal(intent.Digest.Value, digest)) {
		return E("checksum_mismatch", nil)
	}
	if stage == nil || stage.Digest == nil || stage.Digest.Algorithm != "sha-256" || !bytes.Equal(stage.Digest.Value, digest) {
		return E("idempotency_conflict", nil)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func problem(code string, _ error) json.RawMessage {
	value, _ := json.Marshal(map[string]any{"code": code, "detail": "The file operation did not complete."})
	return value
}
func durableContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func (s *Service) keepLease(ctx context.Context, lease Lease) (context.Context, context.CancelFunc) {
	work, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.options.LeaseTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				if _, err := s.journal.Renew(work, lease, s.options.LeaseTTL); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	var once sync.Once
	return work, func() {
		once.Do(func() {
			cancel()
			<-done
			persist, finish := durableContext(ctx)
			defer finish()
			_ = s.journal.Release(persist, lease)
		})
	}
}

func (s *Service) finishBeforeEffect(ctx context.Context, root Root, actor Actor, op Operation, writer storage.StagedWriter, checkpoint []byte, state State, p json.RawMessage) (Operation, error) {
	intent, err := operationIntent(op)
	if err != nil {
		return op, err
	}
	persist, cancel := durableContext(ctx)
	defer cancel()
	if err = writer.Discard(persist, intent, checkpoint); err != nil {
		return s.markUnknown(persist, root, actor, op, checkpoint, err)
	}
	if err = s.journal.FinishWithoutEffect(persist, op.Lease, state, p); err != nil {
		return op, err
	}
	return s.journal.Get(persist, root, actor, op.ID)
}
func (s *Service) markUnknown(ctx context.Context, root Root, actor Actor, op Operation, checkpoint []byte, cause error) (Operation, error) {
	// A failed temporary-stage inspection retains its pre-effect phase and guard;
	// it must not manufacture an uncertain authoritative publication.
	if op.Phase != EffectStarted && op.Phase != NativeApplied {
		return op, E("recovery_required", cause)
	}
	persist, cancel := durableContext(ctx)
	defer cancel()
	if err := s.journal.MarkUnknown(persist, op.Lease, checkpoint); err != nil {
		return op, errors.Join(cause, err)
	}
	saved, err := s.journal.Get(persist, root, actor, op.ID)
	if err != nil {
		return saved, err
	}
	return saved, E("outcome_unknown", cause)
}
func (s *Service) recoverAfterError(ctx context.Context, root Root, actor Actor, op Operation, writer storage.StagedWriter, checkpoint []byte, cause error) (Operation, error) {
	op.Checkpoint = checkpoint
	persist, cancel := durableContext(ctx)
	defer cancel()
	recovered, err := s.recoverClaimed(persist, root, actor, op, writer)
	if err != nil {
		return recovered, errors.Join(cause, err)
	}
	return recovered, nil
}

// Recover inspects the original native intent. Expired worker leases only allow
// inspection; they never authorize another final-path Publish for that intent.
func (s *Service) Recover(ctx context.Context, root Root, actor Actor, id string, writer storage.StagedWriter) (Operation, error) {
	if writer == nil {
		return Operation{}, E("unsupported", nil)
	}
	if err := validateWriterRoot(root, writer); err != nil {
		return Operation{}, err
	}
	op, err := s.journal.Get(ctx, root, actor, id)
	if err != nil {
		return op, err
	}
	// Journal access checks the current recovery actor and retained provenance.
	// Inspect/settlement cannot issue a new final effect, so creator revocation
	// must not prevent charging or indexing an already applied native result.
	if op.Terminal() {
		return op, nil
	}
	op, err = s.journal.Claim(ctx, root, actor, id, s.options.LeaseTTL)
	if err != nil {
		return op, err
	}
	work, stop := s.keepLease(ctx, op.Lease)
	defer stop()
	return s.recoverClaimed(work, root, actor, op, writer)
}
func (s *Service) recoverClaimed(ctx context.Context, root Root, actor Actor, op Operation, writer storage.StagedWriter) (Operation, error) {
	intent, err := operationIntent(op)
	if err != nil {
		return op, err
	}
	if op.Phase == NativeApplied && op.NativeResult != nil {
		return s.settleApplied(ctx, root, actor, op, *op.NativeResult)
	}
	inspection, err := writer.Inspect(ctx, intent, op.Checkpoint)
	if err != nil {
		return s.markUnknown(ctx, root, actor, op, op.Checkpoint, err)
	}
	switch inspection.State {
	case storage.Applied:
		if inspection.Result == nil || validateNative(intent, *inspection.Result) != nil {
			return s.markUnknown(ctx, root, actor, op, inspection.Evidence, E("invalid_native_result", nil))
		}
		if err = s.journal.SaveApplied(ctx, op.Lease, *inspection.Result); err != nil {
			return op, err
		}
		op.NativeResult = inspection.Result
		op.Phase = NativeApplied
		return s.settleApplied(ctx, root, actor, op, *inspection.Result)
	case storage.NotApplied:
		if len(inspection.Evidence) == 0 {
			return s.markUnknown(ctx, root, actor, op, op.Checkpoint, E("missing_recovery_evidence", nil))
		}
		if err = writer.Discard(ctx, intent, op.Checkpoint); err != nil {
			return s.markUnknown(ctx, root, actor, op, op.Checkpoint, err)
		}
		state := Failed
		if op.CancelRequested {
			state = Cancelled
		}
		if op.Phase == EffectStarted {
			err = s.journal.ResolveNotApplied(ctx, op.Lease, state, inspection.Evidence, problem("native_not_applied", nil))
		} else {
			err = s.journal.FinishWithoutEffect(ctx, op.Lease, state, problem("native_not_applied", nil))
		}
		if err != nil {
			return op, err
		}
		return s.journal.Get(ctx, root, actor, op.ID)
	case storage.StagedReady:
		if op.Phase == EffectStarted || op.State == Unknown {
			return s.markUnknown(ctx, root, actor, op, op.Checkpoint, E("native_effect_not_fenced", nil))
		}
		if inspection.Stage == nil || validateStage(intent, *inspection.Stage) != nil {
			return s.markUnknown(ctx, root, actor, op, op.Checkpoint, E("invalid_stage", nil))
		}
		if err = s.journal.SaveStage(ctx, op.Lease, *inspection.Stage); err != nil {
			return op, err
		}
		return s.journal.Get(ctx, root, actor, op.ID)
	default:
		return s.markUnknown(ctx, root, actor, op, inspection.Evidence, E("native_effect_unresolved", nil))
	}
}
func (s *Service) settleApplied(ctx context.Context, root Root, actor Actor, op Operation, native storage.MutationResult) (Operation, error) {
	intent, err := operationIntent(op)
	if err != nil {
		return op, err
	}
	if err = validateNative(intent, native); err != nil {
		return op, err
	}
	// This is index/budget repair of proven native bytes, not permission to
	// publish. Authorization was checked immediately before the final effect.
	physical := intent.Size - op.Reservation.OldBytes
	if s.options.PhysicalDelta != nil {
		physical, err = s.options.PhysicalDelta(op, native)
		if err != nil {
			return op, err
		}
	} else if !intent.Conditions.Absent || op.Reservation.OldBytes != 0 {
		return op, E("unsupported", nil)
	}
	keep := op.Reservation.PhysicalBytes
	if physical > 0 {
		keep -= physical
	}
	if keep < 0 {
		return op, E("physical_budget_mismatch", nil)
	}
	return s.journal.Settle(ctx, op.Lease, Settlement{LogicalDelta: intent.Size - op.Reservation.OldBytes, PhysicalDelta: physical, KeepPhysical: keep, State: Succeeded}, s.options.Commit)
}

// Cleanup touches only a terminal operation's recorded temporary identity. Unknown
// outcomes cannot enter this path. The driver must never discard Target itself.
func (s *Service) Cleanup(ctx context.Context, root Root, actor Actor, id string, writer storage.StagedWriter) error {
	cleanup, ok := s.journal.(CleanupJournal)
	if !ok || writer == nil {
		return E("unsupported", nil)
	}
	if err := validateWriterRoot(root, writer); err != nil {
		return err
	}
	op, err := s.journal.Get(ctx, root, actor, id)
	if err != nil {
		return err
	}
	if !op.Terminal() {
		return E("operation_in_progress", nil)
	}
	if op.CleanedAt != nil {
		return nil
	}
	// Discard only consumes a terminal operation's recorded temporary identity;
	// it cannot mutate authoritative content and is safe system housekeeping.
	intent, err := operationIntent(op)
	if err != nil {
		return err
	}
	if err = writer.Discard(ctx, intent, op.Checkpoint); err != nil {
		return err
	}
	return cleanup.MarkCleaned(ctx, root, actor, id)
}

type WriterResolver func(context.Context, Root) (storage.StagedWriter, func() error, error)

func (s *Service) Sweep(ctx context.Context, limit int, resolve WriterResolver) error {
	cleanup, ok := s.journal.(CleanupJournal)
	if !ok || resolve == nil || limit < 1 || limit > 1000 {
		return E("invalid_config", nil)
	}
	candidates, err := cleanup.CleanupCandidates(ctx, limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, op := range candidates {
		if err = ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		writer, close, err := resolve(ctx, op.Root)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		actor := Actor{ID: "owner:" + op.Root.OwnerID, CanWrite: true, CanRead: true, ManageOwner: true}
		if err = s.Cleanup(ctx, op.Root, actor, op.ID, writer); err != nil {
			failures = append(failures, err)
		}
		if close != nil {
			if err = close(); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
func (s *Service) StartCleanup(ctx context.Context, interval time.Duration, limit int, resolve WriterResolver) (<-chan error, error) {
	if interval < time.Second || resolve == nil || limit < 1 || limit > 1000 {
		return nil, E("invalid_config", nil)
	}
	results := make(chan error, 1)
	go func() {
		defer close(results)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.Sweep(ctx, limit, resolve); err != nil {
					select {
					case results <- err:
					default:
					}
				}
			}
		}
	}()
	return results, nil
}
