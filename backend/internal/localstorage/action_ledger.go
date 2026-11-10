package localstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"everythingabout.me/backend/internal/storage"
)

type actionLedger struct {
	Version                    int
	Binding                    string
	Intent                     storage.EffectIntent
	Phase                      string
	Source                     *storage.Entry
	SourceParent, TargetParent string
	Prepared                   string
	Write                      *storage.WriteIntent
	StageEvidence              []byte
	Result                     *storage.MutationResult
}
type actionReceipt struct {
	Version                    int
	Binding, ID, Action, Phase string
}

func actionKey(id string) string { return "action-" + intentKey(id) }
func (w *MutationDriver) actionReceipt(r *actionLedger) []byte {
	b, _ := json.Marshal(actionReceipt{1, w.driver.binding, r.Intent.ID, r.Intent.Action, r.Phase})
	return b
}
func sameEffect(a, b storage.EffectIntent) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}
func (w *MutationDriver) validateAction(in storage.EffectIntent) error {
	if !validIdentity(in.ID) || len(in.ID) > 1024 || in.Target == "" {
		return storage.NewError("invalid_intent", in.Target, nil)
	}
	if err := validatePath(in.Target); err != nil {
		return err
	}
	switch in.Action {
	case "mkdir":
		if in.Source != "" || len(in.SourceRevision) != 0 {
			return storage.NewError("invalid_intent", in.Target, nil)
		}
	case "remove":
		if in.Source != "" || len(in.SourceRevision) != 0 || in.Conditions.Absent || len(in.Conditions.ExpectedRevision) != sha256.Size {
			return storage.NewError("unsupported", in.Target, nil)
		}
		return nil
	case "move", "copy":
		if in.Source == "" {
			return storage.NewError("invalid_intent", in.Source, nil)
		}
		if err := validatePath(in.Source); err != nil {
			return err
		}
		if len(in.SourceRevision) != sha256.Size {
			return storage.NewError("unsupported", in.Source, nil)
		}
		if w.driver.key(string(in.Source)) == w.driver.key(string(in.Target)) || strings.HasPrefix(w.driver.key(string(in.Target)), w.driver.key(string(in.Source))+"/") {
			return storage.NewError("invalid_name", in.Target, nil)
		}
	default:
		return storage.NewError("unsupported", in.Target, nil)
	}
	if !in.Conditions.Absent || len(in.Conditions.ExpectedRevision) != 0 {
		return storage.NewError("unsupported", in.Target, nil)
	}
	return nil
}
func (w *MutationDriver) beginAction(ctx context.Context, in storage.EffectIntent) (*os.Root, func(), error) {
	if err := w.validateAction(in); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	root, err := w.root(ctx)
	if err != nil {
		return nil, nil, err
	}
	unlock, err := stagedLock(root, actionKey(in.ID)+".lock")
	if err != nil {
		root.Close()
		return nil, nil, err
	}
	// Copy uses the staged writer's own admission while holding this native lock.
	// Other actions acquire bounded read admission for their entire effect below.
	return root, func() { unlock(); root.Close() }, nil
}
func (w *MutationDriver) saveAction(root *os.Root, r *actionLedger) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	key := actionKey(r.Intent.ID)
	if err := root.Remove(key + ".next"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := stagedCreate(root, key+".next")
	if err != nil {
		return err
	}
	_, we := f.Write(b)
	se := f.Sync()
	ce := f.Close()
	if err := errors.Join(we, se, ce); err != nil {
		return err
	}
	if err := root.Rename(key+".next", key+".json"); err != nil {
		return err
	}
	return syncRoot(root)
}
func (w *MutationDriver) loadAction(root *os.Root, in storage.EffectIntent) (*actionLedger, error) {
	f, err := openReadOnly(root, actionKey(in.ID)+".json")
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
	var r actionLedger
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if dec.Decode(&r) != nil || dec.Decode(new(any)) != io.EOF || r.Version != 1 || r.Binding != w.driver.binding || !sameEffect(in, r.Intent) {
		return nil, storage.NewError("intent_conflict", in.Target, nil)
	}
	return &r, nil
}
func (w *MutationDriver) checkActionReceipt(in storage.EffectIntent, evidence []byte) error {
	if len(evidence) == 0 {
		return nil
	}
	var r actionReceipt
	if len(evidence) > 8192 || json.Unmarshal(evidence, &r) != nil || r.Version != 1 || r.Binding != w.driver.binding || r.ID != in.ID || r.Action != in.Action {
		return storage.NewError("invalid_evidence", in.Target, nil)
	}
	return nil
}
func (w *MutationDriver) actionParent(ctx context.Context, path storage.Path) (*os.Root, string, string, error) {
	parent, name := splitPath(path)
	root, err := w.driver.directory(ctx, parent)
	if err != nil {
		if storage.IsCode(err, "not_found") {
			err = storage.NewError("parent_missing", path, err)
		}
		return nil, "", "", err
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, "", "", err
	}
	system, err := w.driver.system.Stat(".")
	if err != nil {
		root.Close()
		return nil, "", "", err
	}
	local, err := stagedLocalFilesystem(root)
	if err != nil || !local || !stagedSameFilesystem(info, system) {
		root.Close()
		return nil, "", "", storage.NewError("unsupported", path, err)
	}
	return root, name, rootIdentity(info), nil
}
func (w *MutationDriver) absent(ctx context.Context, root *os.Root, path storage.Path) error {
	parent, name := splitPath(path)
	_, err := w.driver.find(ctx, root, parent, name)
	if storage.IsCode(err, "not_found") {
		return nil
	}
	if err == nil {
		return storage.NewError("name_conflict", path, nil)
	}
	return err
}
func (w *MutationDriver) Mkdir(ctx context.Context, in storage.Mutation, cp storage.Checkpoint) (storage.MutationResult, error) {
	return w.runAction(ctx, storage.EffectIntent{ID: in.ID, Action: "mkdir", Target: in.Path, Conditions: in.Conditions}, cp)
}
func (w *MutationDriver) Remove(ctx context.Context, in storage.Mutation, cp storage.Checkpoint) (storage.MutationResult, error) {
	return w.runAction(ctx, storage.EffectIntent{ID: in.ID, Action: "remove", Target: in.Path, Conditions: in.Conditions}, cp)
}

// Purge delegates to the journalled removal: a local unlink is a syscall, so the ledger
// costs microseconds here, and keeping one deletion path keeps one set of guarantees.
func (w *MutationDriver) Purge(ctx context.Context, p storage.Path, _ storage.Kind, revision []byte) error {
	_, err := w.Remove(ctx, storage.Mutation{ID: "purge:" + string(p), Path: p, Conditions: storage.Conditions{ExpectedRevision: revision}}, func(context.Context, []byte) error { return nil })
	return err
}
func (w *MutationDriver) Move(ctx context.Context, in storage.MoveRequest, cp storage.Checkpoint) (storage.MutationResult, error) {
	return w.runAction(ctx, storage.EffectIntent{ID: in.ID, Action: "move", Source: in.Source, Target: in.Target, SourceRevision: in.SourceRevision, Conditions: in.TargetConditions}, cp)
}
func (w *MutationDriver) Copy(ctx context.Context, in storage.MoveRequest, cp storage.Checkpoint) (storage.MutationResult, error) {
	return w.runAction(ctx, storage.EffectIntent{ID: in.ID, Action: "copy", Source: in.Source, Target: in.Target, SourceRevision: in.SourceRevision, Conditions: in.TargetConditions}, cp)
}
func (w *MutationDriver) runAction(ctx context.Context, in storage.EffectIntent, cp storage.Checkpoint) (storage.MutationResult, error) {
	// Clone caller-owned conditions before durable comparison/callbacks.
	in.Conditions.ExpectedRevision = bytes.Clone(in.Conditions.ExpectedRevision)
	in.SourceRevision = bytes.Clone(in.SourceRevision)
	root, done, err := w.beginAction(ctx, in)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer done()
	r, err := w.loadAction(root, in)
	if err == nil {
		if r.Phase == "applied" && r.Result != nil {
			return *r.Result, checkpoint(ctx, cp, w.actionReceipt(r))
		}
		return storage.MutationResult{}, storage.NewError("outcome_unknown", in.Target, nil)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return storage.MutationResult{}, err
	}
	r = &actionLedger{Version: 1, Binding: w.driver.binding, Intent: in, Phase: "reserved"}
	if err := w.saveAction(root, r); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if err := checkpoint(ctx, cp, w.actionReceipt(r)); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if in.Action == "copy" {
		return w.copyAction(ctx, root, r, cp)
	}
	release, err := w.driver.acquire(ctx)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	defer release()
	target, name, identity, err := w.actionParent(ctx, in.Target)
	if err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	defer target.Close()
	r.TargetParent = identity
	var source *os.Root
	var sourceName string
	if in.Action == "move" {
		source, sourceName, r.SourceParent, err = w.actionParent(ctx, in.Source)
		if err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
		defer source.Close()
	}
	if in.Action == "remove" {
		source = target
		sourceName = name
		r.SourceParent = identity
	}
	if source != nil {
		path := in.Source
		if in.Action == "remove" {
			path = in.Target
		}
		parent, _ := splitPath(path)
		e, err := w.nativeAt(ctx, source, parent, sourceName)
		if err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
		expected := in.SourceRevision
		if in.Action == "remove" {
			expected = in.Conditions.ExpectedRevision
		}
		if !bytes.Equal(expected, e.Revision) {
			return storage.MutationResult{}, noEffect(storage.NewError("changed", path, nil))
		}
		r.Source = &e
		if in.Action == "remove" && e.Kind == storage.Folder {
			if err := w.emptyFolder(ctx, path); err != nil {
				return storage.MutationResult{}, noEffect(err)
			}
		}
	}
	if in.Action != "remove" {
		if err := w.absent(ctx, target, in.Target); err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
	}
	if in.Action == "mkdir" {
		r.Prepared = actionKey(in.ID) + ".folder"
		// Record the exact private allocation before creating it.
		if err := w.saveAction(root, r); err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
		if err := root.Mkdir(r.Prepared, 0700); err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
		prepared, err := root.OpenRoot(r.Prepared)
		if err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
		syncErr := syncRoot(prepared)
		prepared.Close()
		if syncErr != nil {
			return storage.MutationResult{}, noEffect(syncErr)
		}
		info, err := root.Stat(r.Prepared)
		if err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
		e, _ := entry(in.Target, info)
		e = nativeVersion(e, info)
		r.Source = &e
		if err := syncRoot(root); err != nil {
			return storage.MutationResult{}, noEffect(err)
		}
	}
	r.Phase = "executing"
	if err := w.saveAction(root, r); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if err := checkpoint(ctx, cp, w.actionReceipt(r)); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if err := w.recheckAction(ctx, r); err != nil {
		return storage.MutationResult{}, noEffect(err)
	}
	if in.Action == "remove" {
		err = w.remove(target, name)
	} else if in.Action == "mkdir" {
		err = w.rename(root, r.Prepared, target, name, false)
	} else {
		err = w.rename(source, sourceName, target, name, false)
	}
	if err != nil {
		// EEXIST/ENOTEMPTY are terminal native rejections; other failures may have
		// applied and retain the executing record for identity-based recovery.
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.ENOTEMPTY) {
			r.Phase = "not_applied"
			code := "name_conflict"
			if in.Action == "remove" {
				code = "not_empty"
			}
			return storage.MutationResult{}, errors.Join(storage.NewError(code, in.Target, err), w.saveAction(root, r))
		}
		return storage.MutationResult{}, err
	}
	if err := syncRoot(target); err != nil {
		return storage.MutationResult{}, err
	}
	if source != nil && source != target {
		if err := syncRoot(source); err != nil {
			return storage.MutationResult{}, err
		}
	}
	if in.Action == "mkdir" {
		if err := syncRoot(root); err != nil {
			return storage.MutationResult{}, err
		}
	}
	return w.completeAction(ctx, root, r, cp)
}
func (w *MutationDriver) emptyFolder(ctx context.Context, path storage.Path) error {
	folder, err := w.driver.directory(ctx, path)
	if err != nil {
		return err
	}
	defer folder.Close()
	f, err := folder.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.ReadDir(1)
	if len(entries) != 0 {
		return storage.NewError("not_empty", path, nil)
	}
	if err != nil && err != io.EOF {
		return err
	}
	return ctx.Err()
}
func (w *MutationDriver) recheckAction(ctx context.Context, r *actionLedger) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.driver.lifetime.Err() != nil {
		return storage.NewError("closed", r.Intent.Target, nil)
	}
	in := r.Intent
	target, _, identity, err := w.actionParent(ctx, in.Target)
	if err != nil {
		return err
	}
	defer target.Close()
	if identity != r.TargetParent {
		return storage.NewError("changed", in.Target, nil)
	}
	if in.Action != "remove" {
		if err := w.absent(ctx, target, in.Target); err != nil {
			return err
		}
	}
	if in.Action == "mkdir" {
		private, err := w.root(ctx)
		if err != nil {
			return err
		}
		defer private.Close()
		info, err := private.Lstat(r.Prepared)
		if err != nil || !info.IsDir() || r.Source == nil || rootIdentity(info) != string(r.Source.NativeRef) {
			return storage.NewError("changed", in.Target, err)
		}
	}
	if in.Action == "move" || in.Action == "remove" || in.Action == "copy" {
		path := in.Source
		if in.Action == "remove" {
			path = in.Target
		}
		source, _, identity, err := w.actionParent(ctx, path)
		if err != nil {
			return err
		}
		source.Close()
		if identity != r.SourceParent {
			return storage.NewError("changed", path, nil)
		}
		e, err := w.stat(ctx, path)
		if err != nil {
			return err
		}
		if r.Source == nil || !bytes.Equal(e.Revision, r.Source.Revision) {
			return storage.NewError("changed", path, nil)
		}
		if in.Action == "remove" && e.Kind == storage.Folder {
			return w.emptyFolder(ctx, path)
		}
	}
	return nil
}
func (w *MutationDriver) completeAction(ctx context.Context, root *os.Root, r *actionLedger, cp storage.Checkpoint) (storage.MutationResult, error) {
	var e *storage.Entry
	if r.Intent.Action != "remove" {
		got, err := w.stat(ctx, r.Intent.Target)
		if err != nil {
			return storage.MutationResult{}, err
		}
		e = &got
	}
	r.Phase = "applied"
	result := storage.MutationResult{Entry: e, Evidence: w.actionReceipt(r)}
	r.Result = &result
	if err := w.saveAction(root, r); err != nil {
		return result, err
	}
	return result, checkpoint(ctx, cp, w.actionReceipt(r))
}
