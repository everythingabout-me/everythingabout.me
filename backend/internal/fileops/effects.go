package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"everythingabout.me/backend/internal/storage"
)

// ApplyEntry executes one already-admitted immutable manifest entry. effect must
// be an admitted native driver action from the same root executor as recovery,
// not an arbitrary external task. Trusted callback injection is not a sandbox for
// malicious implementations. This method
// records native outcomes only: it neither settles quota/indexes nor releases path
// guards, rescans a tree, deletes unrecorded files, or retries a final effect.
func (s *Service) ApplyEntry(ctx context.Context, root Root, actor Actor, id string, number int, recovery storage.EffectRecovery, effect func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error)) (Operation, error) {
	if recovery == nil || effect == nil {
		return Operation{}, E("unsupported", nil)
	}
	return s.coordinateEntry(ctx, root, actor, id, number, recovery, effect)
}

// RecoverEntry inspects only the saved native effect identity. A new lease/epoch
// grants authority to save evidence; it never proves an old native request fenced.
func (s *Service) RecoverEntry(ctx context.Context, root Root, actor Actor, id string, number int, recovery storage.EffectRecovery) (Operation, error) {
	if recovery == nil {
		return Operation{}, E("unsupported", nil)
	}
	return s.coordinateEntry(ctx, root, actor, id, number, recovery, nil)
}

func (s *Service) coordinateEntry(ctx context.Context, root Root, actor Actor, id string, number int, recovery storage.EffectRecovery, effect func(context.Context, storage.EffectIntent, storage.Checkpoint) (storage.MutationResult, error)) (Operation, error) {
	bound, ok := recovery.(storage.BoundEffectRecovery)
	if !ok {
		return Operation{}, E("unsupported", nil)
	}
	if err := validateNativeBinding(root, bound.StorageBinding(), bound.StorageNameRules()); err != nil {
		return Operation{}, err
	}
	op, err := s.journal.Get(ctx, root, actor, id)
	if err != nil {
		return op, err
	}
	entry, intent, err := manifestEffect(op, number)
	if err != nil {
		return op, err
	}
	if done, err := recordedEntry(entry); done {
		return op, err
	}
	if op.Terminal() {
		return op, E("invalid_transition", nil)
	}
	if entry.State == "pending" && (effect == nil || op.CancelRequested) {
		return op, E("recovery_required", nil)
	}
	op, err = s.journal.Claim(ctx, root, actor, id, s.options.LeaseTTL)
	if err != nil {
		return op, err
	}
	work, stop := s.keepLease(ctx, op.Lease)
	defer stop()
	entry, intent, err = manifestEffect(op, number)
	if err != nil {
		return op, err
	}
	if done, err := recordedEntry(entry); done {
		return op, err
	}
	if entry.State == "started" || entry.State == "outcome_unknown" {
		return s.inspectEntry(work, root, actor, op, entry, intent, recovery, nil)
	}
	if entry.State != "pending" || effect == nil || op.CancelRequested || op.State == Unknown {
		return op, E("recovery_required", nil)
	}
	// Fresh authorization is checked immediately before journaling the final effect.
	if err = s.options.Authorize(work, op); err != nil {
		return op, err
	}
	if err = s.journal.BeginEntry(work, op.Lease, number); err != nil {
		return op, err
	}
	entry.State = "started"
	setOperationEntry(&op, entry)
	op.Phase = EffectStarted
	// If revocation wins after the durable start marker, native inspection must still
	// resolve that marker; do not reset it to pending or declare a safe rollback.
	if err = s.options.Authorize(work, op); err != nil {
		return s.inspectEntry(work, root, actor, op, entry, intent, recovery, err)
	}
	var mu sync.Mutex
	checkpoint := bytes.Clone(entry.Checkpoint)
	active := true
	save := func(nativeCtx context.Context, value []byte) error {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return E("invalid_transition", nil)
		}
		if err := s.journal.SaveEntry(nativeCtx, op.Lease, number, "started", value, entry.Evidence, entry.Result); err != nil {
			return err
		}
		checkpoint = bytes.Clone(value)
		return nil
	}
	result, nativeErr := effect(work, cloneEffectIntent(intent), save)
	mu.Lock()
	active = false
	entry.Checkpoint = bytes.Clone(checkpoint)
	mu.Unlock()
	setOperationEntry(&op, entry)
	if nativeErr != nil {
		// NoFinalEffect describes this attempt only, never a proof about earlier native
		// requests. It follows the same InspectEffect path as any other native error.
		return s.inspectEntry(ctx, root, actor, op, entry, intent, recovery, nativeErr)
	}
	if err = validateEntryResult(entry, result); err != nil {
		return s.saveEntryUnknown(ctx, root, actor, op, entry, nil, err)
	}
	persist, cancel := durableContext(ctx)
	defer cancel()
	encoded, err := json.Marshal(result)
	if err != nil {
		return s.saveEntryUnknown(persist, root, actor, op, entry, nil, err)
	}
	if err = s.journal.SaveEntry(persist, op.Lease, number, "applied", entry.Checkpoint, result.Evidence, encoded); err != nil {
		// The native effect already happened. Keep the durable started/checkpoint state
		// and require inspection on recovery; never issue another effect after this error.
		return op, err
	}
	return s.journal.Get(persist, root, actor, op.ID)
}

func setOperationEntry(op *Operation, entry EntryEffect) {
	for i := range op.Entries {
		if op.Entries[i].Number == entry.Number {
			op.Entries[i] = entry
			return
		}
	}
}

func manifestEffect(op Operation, number int) (EntryEffect, storage.EffectIntent, error) {
	if op.ID == "" || number < 0 {
		return EntryEffect{}, storage.EffectIntent{}, E("invalid_intent", nil)
	}
	switch op.Kind {
	case "mkdir", "move", "copy", "delete":
	default:
		return EntryEffect{}, storage.EffectIntent{}, E("invalid_intent", nil)
	}
	var selected EntryEffect
	found := false
	for _, entry := range op.Entries {
		if entry.Number == number {
			if found {
				return EntryEffect{}, storage.EffectIntent{}, E("invalid_intent", nil)
			}
			selected = entry
			found = true
		}
	}
	if !found {
		return selected, storage.EffectIntent{}, E("not_found", nil)
	}
	e := selected.ManifestEntry
	if storage.ValidatePath(e.Source) != nil || storage.ValidatePath(e.Target) != nil || e.Size < 0 || e.Size > 9007199254740991 || e.TargetConditions.Absent && len(e.TargetConditions.ExpectedVersion) > 0 {
		return selected, storage.EffectIntent{}, E("invalid_intent", nil)
	}
	switch e.Action {
	case "mkdir":
		if e.Source != "" || e.Target == "" || e.Size != 0 {
			return selected, storage.EffectIntent{}, E("invalid_intent", nil)
		}
	case "delete":
		if e.Source == "" || e.Target != "" {
			return selected, storage.EffectIntent{}, E("invalid_intent", nil)
		}
	case "move", "copy":
		if e.Source == "" || e.Target == "" || e.Source == e.Target {
			return selected, storage.EffectIntent{}, E("invalid_intent", nil)
		}
	default:
		return selected, storage.EffectIntent{}, E("invalid_intent", nil)
	}
	// Admission validates guard coverage. Recheck here to prevent an invalid stored
	// manifest from reaching a driver, without constructing or expanding any guards.
	for _, path := range []storage.Path{e.Source, e.Target} {
		if path != "" && !covered(op.Root, op.Guards, path) {
			return selected, storage.EffectIntent{}, E("invalid_intent", nil)
		}
	}
	selected.SourceVersion = bytes.Clone(e.SourceVersion)
	selected.TargetConditions.ExpectedVersion = bytes.Clone(e.TargetConditions.ExpectedVersion)
	selected.Checkpoint = bytes.Clone(selected.Checkpoint)
	selected.Evidence = bytes.Clone(selected.Evidence)
	selected.Result = bytes.Clone(selected.Result)
	intent := storage.EffectIntent{ID: fmt.Sprintf("%s/entry/%d", op.ID, number), Action: e.Action, Source: e.Source, Target: e.Target, Conditions: storage.Conditions{Absent: e.TargetConditions.Absent, ExpectedVersion: bytes.Clone(e.TargetConditions.ExpectedVersion)}, SourceVersion: bytes.Clone(e.SourceVersion)}
	return selected, intent, nil
}
func cloneEffectIntent(intent storage.EffectIntent) storage.EffectIntent {
	intent.SourceVersion = bytes.Clone(intent.SourceVersion)
	intent.Conditions.ExpectedVersion = bytes.Clone(intent.Conditions.ExpectedVersion)
	return intent
}
func recordedEntry(entry EntryEffect) (bool, error) {
	switch entry.State {
	case "applied":
		var result storage.MutationResult
		if len(entry.Result) == 0 || json.Unmarshal(entry.Result, &result) != nil || validateEntryResult(entry, result) != nil || !bytes.Equal(entry.Evidence, result.Evidence) {
			return true, E("invalid_native_result", nil)
		}
		return true, nil
	case "not_applied":
		if len(entry.Evidence) == 0 {
			return true, E("missing_recovery_evidence", nil)
		}
		return true, E("native_not_applied", nil)
	case "pending", "started", "outcome_unknown":
		return false, nil
	default:
		return true, E("invalid_transition", nil)
	}
}
func validateEntryResult(entry EntryEffect, result storage.MutationResult) error {
	if len(result.Evidence) == 0 {
		return E("missing_recovery_evidence", nil)
	}
	if entry.Action == "delete" {
		if result.Entry != nil {
			return E("invalid_native_result", nil)
		}
		return nil
	}
	if result.Entry == nil || result.Entry.Path != entry.Target || storage.ValidatePath(result.Entry.Path) != nil || result.Entry.Size < 0 || result.Entry.Size > 9007199254740991 {
		return E("invalid_native_result", nil)
	}
	if entry.Action == "mkdir" {
		if result.Entry.Kind != storage.Folder || result.Entry.Size != 0 {
			return E("invalid_native_result", nil)
		}
		return nil
	}
	if result.Entry.Kind != storage.File && result.Entry.Kind != storage.Folder {
		return E("invalid_native_result", nil)
	}
	if entry.Action == "move" && entry.SourceEntry != nil && result.Entry.Kind != entry.SourceEntry.Kind {
		return E("invalid_native_result", nil)
	}
	if result.Entry.Kind == storage.Folder && result.Entry.Size != 0 || result.Entry.Kind == storage.File && result.Entry.Size != entry.Size {
		return E("invalid_native_result", nil)
	}
	return nil
}
func (s *Service) inspectEntry(ctx context.Context, root Root, actor Actor, op Operation, entry EntryEffect, intent storage.EffectIntent, recovery storage.EffectRecovery, cause error) (Operation, error) {
	persist, cancel := durableContext(ctx)
	defer cancel()
	// A checkpoint save can commit before its response is lost. Inspect the latest
	// durable evidence, while retaining this worker's original update fence.
	latest, readErr := s.journal.Get(persist, root, actor, op.ID)
	if readErr != nil {
		return op, errors.Join(cause, readErr)
	}
	latestEntry, latestIntent, readErr := manifestEffect(latest, entry.Number)
	if readErr != nil {
		return op, errors.Join(cause, readErr)
	}
	if !equalJSON(intent, latestIntent) {
		return op, E("intent_conflict", cause)
	}
	if done, recordedErr := recordedEntry(latestEntry); done {
		return latest, recordedErr
	}
	if latestEntry.State != "started" && latestEntry.State != "outcome_unknown" {
		return latest, E("recovery_required", cause)
	}
	heldLease := op.Lease
	op = latest
	op.Lease = heldLease
	entry = latestEntry
	inspection, err := recovery.InspectEffect(persist, cloneEffectIntent(intent), bytes.Clone(entry.Checkpoint))
	if err != nil {
		return s.saveEntryUnknown(persist, root, actor, op, entry, nil, errors.Join(cause, err))
	}
	switch inspection.State {
	case storage.Applied:
		if inspection.Result == nil {
			return s.saveEntryUnknown(persist, root, actor, op, entry, inspection.Evidence, E("invalid_native_result", cause))
		}
		if err = validateEntryResult(entry, *inspection.Result); err != nil {
			return s.saveEntryUnknown(persist, root, actor, op, entry, inspection.Evidence, err)
		}
		result, err := json.Marshal(inspection.Result)
		if err != nil {
			return s.saveEntryUnknown(persist, root, actor, op, entry, inspection.Evidence, err)
		}
		if err = s.journal.SaveEntry(persist, op.Lease, entry.Number, "applied", entry.Checkpoint, inspection.Result.Evidence, result); err != nil {
			return op, errors.Join(cause, err)
		}
		return s.journal.Get(persist, root, actor, op.ID)
	case storage.NotApplied:
		if len(inspection.Evidence) == 0 {
			return s.saveEntryUnknown(persist, root, actor, op, entry, nil, E("missing_recovery_evidence", cause))
		}
		if err = s.journal.SaveEntry(persist, op.Lease, entry.Number, "not_applied", entry.Checkpoint, inspection.Evidence, nil); err != nil {
			return op, errors.Join(cause, err)
		}
		saved, err := s.journal.Get(persist, root, actor, op.ID)
		if err != nil {
			return saved, err
		}
		return saved, E("native_not_applied", cause)
	default:
		// StagedReady is not terminal evidence for mkdir/move/copy/delete. Unknown or
		// malformed inspection never resets an entry or releases its conflicting guards.
		return s.saveEntryUnknown(persist, root, actor, op, entry, inspection.Evidence, E("native_effect_unresolved", cause))
	}
}
func (s *Service) saveEntryUnknown(ctx context.Context, root Root, actor Actor, op Operation, entry EntryEffect, evidence []byte, cause error) (Operation, error) {
	persist, cancel := durableContext(ctx)
	defer cancel()
	if len(evidence) == 0 {
		evidence = entry.Evidence
	}
	if err := s.journal.SaveEntry(persist, op.Lease, entry.Number, "outcome_unknown", entry.Checkpoint, evidence, nil); err != nil {
		return op, errors.Join(cause, err)
	}
	saved, err := s.journal.Get(persist, root, actor, op.ID)
	if err != nil {
		return saved, err
	}
	return saved, E("outcome_unknown", cause)
}
