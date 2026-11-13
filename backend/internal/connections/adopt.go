package connections

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"everythingabout.me/backend/internal/config"
)

// ErrPathConflict reports a legacy and an id-keyed directory that hold different
// contents under one name. Choosing either side could discard a non-terminal ledger
// record, so the start stops and names both paths instead.
var ErrPathConflict = errors.New("storage connection path conflict")

// AdoptObjectPaths rewrites every per-connection directory stored under the legacy
// name-keyed layout into the id-keyed one. The server calls it once at startup, after
// the first row is seeded and before any owner resolves a connection, so no staged byte
// moves after a driver has opened it.
//
// It exists because the name is not unique: 036 lets two owners each call a connection
// "nas", while an earlier release used the name as the leaf of both the spool directory
// and the evidence file name. Two accounts therefore shared one directory on the host.
// Repointing rows without moving the directories would orphan the staged bytes of an
// upload that was in flight during the upgrade — the one thing the spool exists to
// keep across a restart.
//
// A row is adopted only when its stored path is exactly the legacy derivation; an
// operator's explicit --spool-root/--admission-file and the deployment's own paths are
// never touched. The rule for each of the two paths, independently:
//
//   - neither exists: the row is repointed and the id-keyed spool directory is created
//     under the deployment root, which must already exist;
//   - only the legacy path exists: rename it, so every staged byte and every ledger
//     record moves with the directory;
//   - only the id-keyed path exists: nothing to move;
//   - both exist: the spool is merged entry by entry. Entries absent from the new
//     directory are moved in, byte-identical duplicates are dropped from the old one,
//     and one name present in both with different contents stops the start with
//     ErrPathConflict — a non-terminal record proves its effect may still land, so
//     silently choosing either copy is exactly the loss the ledger exists to prevent
//     (docs/design/storage.md §5). The single evidence file is not merged: the
//     id-keyed file wins because it is the one the current layout reads, and the old
//     file is left in place and logged. Evidence is a measurement the probe rewrites,
//     not user state.
//
// The whole function is idempotent: a second start finds no legacy path. Two rows that
// once shared one name-keyed directory are ordered by creation, so the older row adopts
// it whole and the newer row starts empty; the log names the shared path so an operator
// can see where the other row's staged bytes went. Nothing is deleted to "clean up"
// after them, because the ledger in either directory is the proof a publication may
// still land.
func AdoptObjectPaths(ctx context.Context, store Store, deployment config.Storage) error {
	if store == nil {
		return nil
	}
	rows, err := store.Connections(ctx)
	if err != nil {
		return fmt.Errorf("adopt storage connection paths: %w", err)
	}
	// Two rows can derive the same legacy path; that collision is the defect this pass
	// repairs. Name it once, so the operator can see which row adopted the directory and
	// where the other row's staged bytes went, instead of finding two empty-looking spools.
	shared := map[string][]string{}
	for _, c := range rows {
		if c.Backend == "local" || c.ID == "" {
			continue
		}
		if legacy, ok := derivedSpool(deployment, c.Name); ok && c.Config.ObjectSpoolRoot == legacy {
			shared[legacy] = append(shared[legacy], c.ID)
		}
	}
	for path, ids := range shared {
		if len(ids) > 1 {
			slog.Warn("storage connections shared one name-keyed spool directory; the oldest row adopts it and the others start with an empty one",
				"path", path, "connection_ids", ids)
		}
	}
	for _, c := range rows {
		if err := adoptObjectPaths(ctx, store, deployment, c); err != nil {
			return fmt.Errorf("adopt storage connection %q (id %s) paths: %w", c.Name, c.ID, err)
		}
	}
	return nil
}

// adoptObjectPaths is the per-row half, separately testable.
func adoptObjectPaths(ctx context.Context, store Store, deployment config.Storage, c Connection) error {
	if c.Backend == "local" || c.ID == "" {
		// A local connection stages inside its own system root and owns no object
		// evidence or spool; a row without an id predates the uuid column or was
		// written by hand, and there is nothing to key on.
		return nil
	}
	admission, spool := c.Config.ObjectAdmissionFile, c.Config.ObjectSpoolRoot
	changed := false

	if legacy, ok := derivedAdmission(deployment, c.Name); ok && admission == legacy {
		if want, ok := derivedAdmission(deployment, c.ID); ok && want != legacy {
			if err := adoptEvidence(c.Name, c.ID, legacy, want); err != nil {
				return err
			}
			admission, changed = want, true
		}
	}
	if legacy, ok := derivedSpool(deployment, c.Name); ok && spool == legacy {
		if want, ok := derivedSpool(deployment, c.ID); ok && want != legacy {
			if err := adoptSpool(c.Name, c.ID, legacy, want); err != nil {
				return err
			}
			spool, changed = want, true
		}
	}
	if !changed {
		return nil
	}
	slog.Info("repointed a storage connection to its id-keyed on-disk paths",
		"connection", c.Name, "connection_id", c.ID, "admission", admission, "spool", spool)
	return store.SetObjectPaths(ctx, c.ID, admission, spool)
}

// adoptEvidence moves the legacy evidence file. Its name changes with the layout, so
// the two paths rarely collide; when they do the id-keyed file wins and the old one is
// only reported, because a probe rewrites the file and removing another connection's
// evidence would be worse than leaving a dead file behind.
func adoptEvidence(name, id, from, to string) error {
	info, err := os.Lstat(from)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("inspect legacy admission evidence %s: %w", from, err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("legacy admission evidence %s is not a regular file, so it is not what this service wrote; move it away from the evidence path and probe the connection again", from)
	}
	current, err := os.Lstat(to)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move admission evidence %s to %s: %w", from, to, err)
		}
		slog.Info("moved a storage connection's admission evidence to its id-keyed path",
			"connection", name, "connection_id", id, "from", from, "to", to)
		return nil
	case err != nil:
		return fmt.Errorf("inspect admission evidence %s: %w", to, err)
	case current.Mode().IsRegular() && sameContents(from, to):
		if err := os.Remove(from); err != nil {
			return fmt.Errorf("remove the superseded evidence copy %s: %w", from, err)
		}
		return nil
	}
	slog.Warn("both a legacy and an id-keyed admission evidence file exist; keeping the id-keyed one and leaving the old file in place",
		"connection", name, "connection_id", id, "legacy", from, "current", to)
	return nil
}

// adoptSpool moves or merges one connection's staged material and ledger.
func adoptSpool(name, id, from, to string) error {
	info, err := os.Lstat(from)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing was staged under the old name. The driver still needs its root to
		// exist, and its parent is the deployment's spool root, which must already be
		// there; creating only this child keeps a failed mount from being hidden.
		if err := ensureDirectory(to); err != nil {
			return err
		}
		slog.Info("created a storage connection's id-keyed spool directory",
			"connection", name, "connection_id", id, "path", to)
		return nil
	case err != nil:
		return fmt.Errorf("inspect legacy spool %s: %w", from, err)
	case !info.IsDir():
		return fmt.Errorf("legacy spool path %s is not a directory, so it is not what this service wrote; move it away and restart", from)
	}
	if _, err := os.Lstat(to); errors.Is(err, fs.ErrNotExist) {
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("move legacy spool %s to %s: %w", from, to, err)
		}
		slog.Info("adopted a storage connection's legacy spool directory",
			"connection", name, "connection_id", id, "from", from, "to", to)
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect spool %s: %w", to, err)
	}
	if err := ensureDirectory(to); err != nil {
		return err
	}
	moved, err := mergeSpool(from, to)
	if err != nil {
		return err
	}
	slog.Info("merged a storage connection's legacy spool into its id-keyed one",
		"connection", name, "connection_id", id, "from", from, "to", to, "entries_moved", moved)
	return nil
}

// mergeSpool merges a legacy spool into the id-keyed one and removes the legacy
// directory once it is empty. It never follows a symlink: a spool may only contain the
// directories and files the driver wrote, so anything else is a conflict, not a copy.
func mergeSpool(from, to string) (int, error) {
	entries, err := os.ReadDir(from)
	if err != nil {
		return 0, fmt.Errorf("read legacy spool %s: %w", from, err)
	}
	moved := 0
	for _, entry := range entries {
		source, target := filepath.Join(from, entry.Name()), filepath.Join(to, entry.Name())
		info, err := os.Lstat(target)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Rename(source, target); err != nil {
				return moved, fmt.Errorf("move spool entry %s to %s: %w", source, target, err)
			}
			moved++
		case err != nil:
			return moved, fmt.Errorf("inspect spool entry %s: %w", target, err)
		case entry.IsDir() && info.IsDir():
			n, err := mergeSpool(source, target)
			moved += n
			if err != nil {
				return moved, err
			}
		case entry.Type().IsRegular() && info.Mode().IsRegular() && sameContents(source, target):
			if err := os.Remove(source); err != nil {
				return moved, fmt.Errorf("remove the duplicate spool entry %s: %w", source, err)
			}
		default:
			return moved, fmt.Errorf("%w: the legacy spool %s and the id-keyed spool %s both hold %q with different contents; refusing to start because a record in either one may describe a publication whose effect can still land, so neither copy can be discarded automatically — inspect both files, keep the one the connection's live upload/operation rows name, rename the other out of its spool directory, then restart",
				ErrPathConflict, from, to, entry.Name())
		}
	}
	if err := os.Remove(from); err != nil {
		return moved, fmt.Errorf("remove the now-empty legacy spool %s: %w", from, err)
	}
	return moved, nil
}

// ensureDirectory requires an existing directory or creates exactly that one child.
func ensureDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("spool path %s exists and is not a directory", path)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("inspect spool path %s: %w", path, err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return fmt.Errorf("create spool directory %s: %w (its parent must already exist; a missing parent is usually a volume that did not mount, so it is not created here)", path, err)
	}
	return nil
}

// sameContents reports whether two regular files hold the same bytes. It streams
// because a spool entry can be a whole staged part.
func sameContents(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil || !ai.Mode().IsRegular() {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil || !bi.Mode().IsRegular() || ai.Size() != bi.Size() {
		return false
	}
	af, err := os.Open(a)
	if err != nil {
		return false
	}
	defer af.Close()
	bf, err := os.Open(b)
	if err != nil {
		return false
	}
	defer bf.Close()
	left := ai.Size()
	first, second := make([]byte, 64<<10), make([]byte, 64<<10)
	for left > 0 {
		chunk := int64(len(first))
		if left < chunk {
			chunk = left
		}
		if _, err := io.ReadFull(af, first[:chunk]); err != nil {
			return false
		}
		if _, err := io.ReadFull(bf, second[:chunk]); err != nil {
			return false
		}
		if !bytes.Equal(first[:chunk], second[:chunk]) {
			return false
		}
		left -= chunk
	}
	return true
}
