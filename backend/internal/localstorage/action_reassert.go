package localstorage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"syscall"

	"everythingabout.me/backend/internal/storage"
)

// lockPath serializes the revision check and the native effect for one path. Purge
// removals and marker reasserts share it; unrelated paths never wait on each other.
func (w *MutationDriver) lockPath(p storage.Path) func() {
	key := string(p)
	w.pathMu.Lock()
	lock := w.paths[key]
	if lock == nil {
		lock = &pathLock{}
		w.paths[key] = lock
	}
	lock.refs++
	w.pathMu.Unlock()
	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		w.pathMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(w.paths, key)
		}
		w.pathMu.Unlock()
	}
}

// Reassert replaces an empty folder's directory inode so it no longer matches the
// revision a purge observed, without touching anything under it. See storage.FolderWriter.
//
// A local folder's revision is its directory identity (device:inode), so the only way to
// move it is to replace the directory. The new directory is prepared in the private system
// root and renamed over the target with replace semantics: rename refuses a non-empty
// target, so user content is never removed to reassert a marker. The shared path lock
// closes the check-then-rmdir gap with the purge, which has no conditional unlink.
//
// This is a fence, not a content mutation: a lost reply is safe to repeat, because the
// only effect is replacing one empty marker and the caller retries the create. When the
// purge already removed the marker, a fresh folder is created so the create still ends
// with a native folder.
func (w *MutationDriver) Reassert(ctx context.Context, in storage.Mutation, _ storage.Checkpoint) (storage.MutationResult, error) {
	if in.Path == "" || storage.ValidatePath(in.Path) != nil || in.Conditions.Absent {
		return storage.MutationResult{}, storage.NewError("invalid_request", in.Path, nil)
	}
	release := w.lockPath(in.Path)
	defer release()
	current, err := w.stat(ctx, in.Path)
	if storage.IsCode(err, "not_found") {
		// The purge reached the marker first; leave a fresh one behind. A fresh mutation ID
		// keeps a retry from replaying an earlier create's ledger receipt without creating.
		id, idErr := reassertCreateID(in.ID)
		if idErr != nil {
			return storage.MutationResult{}, idErr
		}
		return w.Mkdir(ctx, storage.Mutation{ID: id, Path: in.Path, Conditions: storage.Conditions{Absent: true}}, func(context.Context, []byte) error { return nil })
	}
	if err != nil {
		return storage.MutationResult{}, err
	}
	if current.Kind != storage.Folder {
		return storage.MutationResult{}, storage.NewError("not_a_folder", in.Path, nil)
	}
	if len(in.Conditions.ExpectedRevision) > 0 && !bytes.Equal(in.Conditions.ExpectedRevision, current.Revision) {
		return storage.MutationResult{}, storage.NewError("changed", in.Path, nil)
	}
	// Removing content to reassert a marker is never acceptable. A non-empty folder already
	// protects itself: the purge keeps a folder whose children survived its pass.
	if err = w.emptyFolder(ctx, in.Path); err != nil {
		if storage.IsCode(err, "not_empty") {
			return storage.MutationResult{}, err
		}
		return storage.MutationResult{}, err
	}
	private, err := w.root(ctx)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer private.Close()
	prepared := intentKey(in.ID) + ".reassert"
	// A previous attempt may have left its private allocation behind; it holds no user bytes.
	_ = private.Remove(prepared)
	if err = private.Mkdir(prepared, 0700); err != nil {
		return storage.MutationResult{}, storage.NewError("invalid_state", in.Path, err)
	}
	defer private.Remove(prepared)
	if err = syncRoot(private); err != nil {
		return storage.MutationResult{}, err
	}
	target, name, _, err := w.actionParent(ctx, in.Path)
	if err != nil {
		return storage.MutationResult{}, err
	}
	defer target.Close()
	if err = w.rename(private, prepared, target, name, true); err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
			// The purge removed the marker after the emptiness check. Create a fresh one;
			// the create path still ends with a native folder.
			id, idErr := reassertCreateID(in.ID)
			if idErr != nil {
				return storage.MutationResult{}, idErr
			}
			return w.Mkdir(ctx, storage.Mutation{ID: id, Path: in.Path, Conditions: storage.Conditions{Absent: true}}, func(context.Context, []byte) error { return nil })
		case errors.Is(err, os.ErrExist), errors.Is(err, syscall.ENOTEMPTY):
			return storage.MutationResult{}, storage.NewError("not_empty", in.Path, err)
		}
		return storage.MutationResult{}, err
	}
	if err = syncRoot(target); err != nil {
		return storage.MutationResult{}, err
	}
	info, err := target.Stat(name)
	if err != nil {
		return storage.MutationResult{}, err
	}
	e, err := entry(in.Path, info)
	if err != nil {
		return storage.MutationResult{}, err
	}
	e = nativeVersion(e, info)
	return storage.MutationResult{Entry: &e}, nil
}

// reassertCreateID names the create that replaces a marker the purge already removed. The
// ID must be unique per attempt: the local action ledger treats an applied ID as a replay
// and would otherwise report a create that no longer exists as done.
func reassertCreateID(base string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base + ":create:" + hex.EncodeToString(b[:]), nil
}
