package localstorage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"

	"everythingabout.me/backend/internal/storage"
)

func (w *MutationDriver) copyAction(ctx context.Context, root *os.Root, r *actionLedger, cp storage.Checkpoint) (storage.MutationResult, error) {
	in := r.Intent
	release, err := w.driver.acquire(ctx)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	source, name, identity, err := w.actionParent(ctx, in.Source)
	if err != nil {
		release()
		return storage.MutationResult{}, noEffect(err)
	}
	parent, _ := splitPath(in.Source)
	found, err := w.driver.find(ctx, source, parent, name)
	if err != nil {
		source.Close()
		release()
		return storage.MutationResult{}, noEffect(err)
	}
	if found.entry.Kind != storage.File {
		source.Close()
		release()
		return storage.MutationResult{}, noEffect(storage.NewError("unsupported", in.Source, nil))
	}
	// Observe the source through its open handle without hashing it: the staged copy
	// computes the digest of the bytes it actually copies, and the source's native
	// state is checked again after the copy, so the extra full read bought nothing.
	f, err := openReadOnly(source, name)
	if err != nil {
		source.Close()
		release()
		return storage.MutationResult{}, noEffect(mapError(in.Source, err))
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !sameNativeStat(found.info, info) {
		source.Close()
		release()
		return storage.MutationResult{}, noEffect(storage.NewError("changed", in.Source, err))
	}
	e, err := entry(in.Source, info)
	if err != nil {
		source.Close()
		release()
		return storage.MutationResult{}, noEffect(err)
	}
	e = nativeVersion(e, info)
	source.Close()
	if !bytes.Equal(e.Revision, in.SourceRevision) {
		release()
		return storage.MutationResult{}, noEffect(storage.NewError("changed", in.Source, nil))
	}
	target, _, targetIdentity, err := w.actionParent(ctx, in.Target)
	if err != nil {
		release()
		return storage.MutationResult{}, noEffect(err)
	}
	err = w.absent(ctx, target, in.Target)
	target.Close()
	release()
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	r.Source = &e
	r.SourceParent = identity
	r.TargetParent = targetIdentity
	// No digest is carried into the intent: Stage computes the digest of the bytes it
	// copies and verifies the staged file against it, and the source's native state is
	// re-checked after the copy. A digest observed before the copy would only have
	// compared those bytes to themselves.
	r.Write = &storage.WriteIntent{ID: "local-copy-v1/" + intentKey(in.ID), Target: in.Target, Conditions: in.Conditions, SourceModifiedAt: &e.ModifiedAt, Size: e.Size}
	r.Phase = "staging"
	if err := w.saveAction(root, r); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	staged, err := w.Stage(ctx, *r.Write, f, func(ctx context.Context, evidence []byte) error {
		r.StageEvidence = bytes.Clone(evidence)
		if err := w.saveAction(root, r); err != nil {
			return err
		}
		return checkpoint(ctx, cp, w.actionReceipt(r))
	})
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	// The copy read the source for as long as it took, so confirm the bytes it read
	// still belong to the state the operation was admitted for. This replaces the
	// observation-time digest as the torn-copy check at the cost of one stat.
	if after, statErr := f.Stat(); statErr != nil || !sameNativeStat(found.info, after) {
		return storage.MutationResult{}, noEffect(storage.NewError("changed", in.Source, statErr))
	}
	r.Phase = "executing"
	if err := w.saveAction(root, r); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	result, err := w.Publish(ctx, staged, func(ctx context.Context, evidence []byte) error {
		r.StageEvidence = bytes.Clone(evidence)
		if err := w.saveAction(root, r); err != nil {
			return err
		}
		if err := checkpoint(ctx, cp, w.actionReceipt(r)); err != nil {
			return err
		}
		var receipt stageReceipt
		if json.Unmarshal(evidence, &receipt) != nil {
			return storage.NewError("invalid_evidence", in.Target, nil)
		}
		if receipt.Phase == "publishing" {
			return w.recheckAction(ctx, r)
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	r.Phase = "applied"
	result.Evidence = w.actionReceipt(r)
	r.Result = &result
	if err := w.saveAction(root, r); err != nil {
		return result, err
	}
	// The staged inode is private and independent of the source. Cleanup removes
	// only the extra system hardlink, preserving the final ordinary file.
	if err := w.Discard(ctx, *r.Write, r.StageEvidence); err != nil {
		return result, err
	}
	return result, checkpoint(ctx, cp, w.actionReceipt(r))
}

func (w *MutationDriver) InspectEffect(ctx context.Context, in storage.EffectIntent, evidence []byte) (storage.Inspection, error) {
	if in.Action == "delete" {
		if in.Target != "" || in.Conditions.Absent || len(in.Conditions.ExpectedRevision) != 0 {
			return storage.Inspection{}, storage.NewError("intent_conflict", in.Target, nil)
		}
		in.Action, in.Target, in.Source, in.Conditions.ExpectedRevision, in.SourceRevision = "remove", in.Source, "", bytes.Clone(in.SourceRevision), nil
	}
	if in.Action == "publish" {
		return w.StagedWriter.InspectEffect(ctx, in, evidence)
	}
	if err := w.checkActionReceipt(in, evidence); err != nil {
		return storage.Inspection{}, err
	}
	root, done, err := w.beginAction(ctx, in)
	if errors.Is(err, errStagedLocked) {
		return storage.Inspection{State: storage.OutcomeUnknown}, nil
	}
	if err != nil {
		return storage.Inspection{}, err
	}
	defer done()
	r, err := w.loadAction(root, in)
	if errors.Is(err, os.ErrNotExist) {
		r = &actionLedger{Version: 1, Binding: w.driver.binding, Intent: in, Phase: "not_applied"}
		if len(evidence) != 0 {
			r.Phase = "unresolved"
		}
		if err := w.saveAction(root, r); err != nil {
			return storage.Inspection{}, err
		}
	} else if err != nil {
		return storage.Inspection{}, err
	}
	unknown := func() (storage.Inspection, error) {
		return storage.Inspection{State: storage.OutcomeUnknown, Evidence: w.actionReceipt(r)}, nil
	}
	seal := func() (storage.Inspection, error) {
		r.Phase = "not_applied"
		if err := w.saveAction(root, r); err != nil {
			return storage.Inspection{}, err
		}
		return storage.Inspection{State: storage.NotApplied, Evidence: w.actionReceipt(r)}, nil
	}
	switch r.Phase {
	case "not_applied":
		return storage.Inspection{State: storage.NotApplied, Evidence: w.actionReceipt(r)}, nil
	case "applied":
		if r.Result != nil {
			return storage.Inspection{State: storage.Applied, Result: r.Result, Evidence: w.actionReceipt(r)}, nil
		}
		return unknown()
	case "reserved":
		return seal()
	case "staging", "executing":
	default:
		return unknown()
	}
	if in.Action == "copy" {
		if r.Write == nil {
			return unknown()
		}
		inspection, err := w.Inspect(ctx, *r.Write, r.StageEvidence)
		if err != nil {
			return storage.Inspection{}, err
		}
		switch inspection.State {
		case storage.StagedReady, storage.NotApplied:
			if err := w.Discard(ctx, *r.Write, r.StageEvidence); err != nil {
				return storage.Inspection{}, err
			}
			return seal()
		case storage.Applied:
			if inspection.Result == nil {
				return unknown()
			}
			result := *inspection.Result
			r.Phase = "applied"
			result.Evidence = w.actionReceipt(r)
			r.Result = &result
			if err := w.saveAction(root, r); err != nil {
				return storage.Inspection{}, err
			}
			if err := w.Discard(ctx, *r.Write, r.StageEvidence); err != nil {
				return storage.Inspection{}, err
			}
			return storage.Inspection{State: storage.Applied, Result: &result, Evidence: w.actionReceipt(r)}, nil
		default:
			return unknown()
		}
	}
	release, err := w.driver.acquire(ctx)
	if err != nil {
		return storage.Inspection{}, err
	}
	defer release()
	target, _, identity, err := w.actionParent(ctx, in.Target)
	if err != nil {
		return unknown()
	}
	defer target.Close()
	if identity != r.TargetParent || r.Source == nil {
		return unknown()
	}
	targetEntry, targetErr := w.stat(ctx, in.Target)
	if in.Action == "remove" {
		if storage.IsCode(targetErr, "not_found") {
			if err := syncRoot(target); err != nil {
				return storage.Inspection{}, err
			}
			result, err := w.completeAction(ctx, root, r, func(context.Context, []byte) error { return nil })
			if err != nil {
				return storage.Inspection{}, err
			}
			return storage.Inspection{State: storage.Applied, Result: &result, Evidence: w.actionReceipt(r)}, nil
		}
		if targetErr == nil && bytes.Equal(targetEntry.Revision, r.Source.Revision) {
			return seal()
		}
		return unknown()
	}
	if in.Action == "mkdir" {
		if targetErr == nil && bytes.Equal(targetEntry.Revision, r.Source.Revision) {
			if err := syncRoot(target); err != nil {
				return storage.Inspection{}, err
			}
			if err := syncRoot(root); err != nil {
				return storage.Inspection{}, err
			}
			result, err := w.completeAction(ctx, root, r, func(context.Context, []byte) error { return nil })
			if err != nil {
				return storage.Inspection{}, err
			}
			return storage.Inspection{State: storage.Applied, Result: &result, Evidence: w.actionReceipt(r)}, nil
		}
		prepared, err := root.Lstat(r.Prepared)
		if err == nil && prepared.IsDir() && bytes.Equal([]byte(rootIdentity(prepared)), r.Source.NativeRef) {
			return seal()
		}
		return unknown()
	}
	source, _, identity, err := w.actionParent(ctx, in.Source)
	if err != nil {
		return unknown()
	}
	defer source.Close()
	if identity != r.SourceParent {
		return unknown()
	}
	sourceEntry, sourceErr := w.stat(ctx, in.Source)
	if sourceErr == nil && bytes.Equal(sourceEntry.Revision, r.Source.Revision) {
		return seal()
	}
	if storage.IsCode(sourceErr, "not_found") && targetErr == nil && bytes.Equal(targetEntry.Revision, r.Source.Revision) {
		if err := syncRoot(source); err != nil {
			return storage.Inspection{}, err
		}
		if err := syncRoot(target); err != nil {
			return storage.Inspection{}, err
		}
		result, err := w.completeAction(ctx, root, r, func(context.Context, []byte) error { return nil })
		if err != nil {
			return storage.Inspection{}, err
		}
		return storage.Inspection{State: storage.Applied, Result: &result, Evidence: w.actionReceipt(r)}, nil
	}
	return unknown()
}
