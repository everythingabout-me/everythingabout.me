package objectmutation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// The object spool is recovery evidence, not user data. A record whose operation
// settled can no longer hide a possible effect, so keeping it forever only made a
// disposable cache block the automatic baseline after an index loss. It is kept for
// storage.IntentRetention, the same window a terminal operation remains answerable,
// and then removed. A non-terminal record is never aged out: its remote effect may
// still land. Admission therefore refuses any non-terminal record, while a settled
// record (and the permanent per-intent lock beside it) no longer blocks a rebuild.

// settledPhase names every ledger phase whose native outcome is final. "discarded"
// and "not_applied" prove no final effect; "applied" proves the only one this record
// attributes. Everything else (new, receiving, ready, publishing, unknown) may still
// produce or already may have produced a native effect.
func settledPhase(phase string) bool {
	switch phase {
	case "applied", "not_applied", "discarded":
		return true
	}
	return false
}

// settleRetention stamps a terminal record once. Re-saving an already settled record
// must not move its deadline forward, and a record that regresses to a non-terminal
// phase loses its deadline entirely because cleanup may never remove it.
func settleRetention(phase string, retainUntil *time.Time, now time.Time) {
	if !settledPhase(phase) {
		*retainUntil = time.Time{}
		return
	}
	if retainUntil.IsZero() {
		*retainUntil = now.UTC().Add(storage.IntentRetention)
	}
}

// retentionPassed accepts the explicit deadline when this build wrote it. Records
// written before the field existed have no deadline; their file modification time is
// the only durable evidence of when they settled, so they age from there instead of
// staying forever. The fallback only affects pruning: admission already ignores every
// settled record without consulting time.
func retentionPassed(r *ledger, info os.FileInfo, now time.Time) bool {
	until := r.RetainUntil
	if until.IsZero() {
		if info == nil {
			return false
		}
		until = info.ModTime().Add(storage.IntentRetention)
	}
	return !now.Before(until)
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func ledgerRecordName(name string) bool {
	return strings.HasSuffix(name, ".json") && isHex(strings.TrimSuffix(name, ".json"), 64)
}
func ledgerLockName(name string) bool {
	return strings.HasSuffix(name, ".lock") && isHex(strings.TrimSuffix(name, ".lock"), 64)
}
func ledgerDataName(name string) bool {
	if !strings.HasSuffix(name, ".data") {
		return false
	}
	base, attempt, ok := strings.Cut(strings.TrimSuffix(name, ".data"), "-")
	return ok && isHex(base, 64) && isHex(attempt, 32)
}
func ledgerTempName(name string) bool {
	if !strings.HasSuffix(name, ".tmp") {
		return false
	}
	base, attempt, ok := strings.Cut(strings.TrimSuffix(name, ".tmp"), "-record-")
	return ok && isHex(base, 64) && isHex(attempt, 32)
}

// ignorableSpoolName covers private artifacts that cannot themselves describe a
// possible effect: the permanent per-intent lock, a staged payload owned by a
// terminal record, and a save scratch file. The authoritative .json record is not
// ignorable; it decides admission and cleanup.
func ignorableSpoolName(name string) bool {
	return ledgerLockName(name) || ledgerDataName(name) || ledgerTempName(name)
}

func spoolNamespace(binding storage.Config, options Options) (string, error) {
	if !filepath.IsAbs(options.SpoolRoot) || storage.ValidateBinding(binding) != nil {
		return "", storage.NewError("invalid_config", "", nil)
	}
	binding.Credentials = nil
	data, _ := json.Marshal(binding)
	hash := sha256.Sum256(data)
	return "object-mutations-v1-" + hex.EncodeToString(hash[:]), nil
}

// openSpoolNamespace opens the exact private namespace without creating it. The
// caller gets exists=false when this binding has never written.
func openSpoolNamespace(binding storage.Config, options Options) (*os.Root, string, bool, error) {
	name, err := spoolNamespace(binding, options)
	if err != nil {
		return nil, "", false, err
	}
	base, err := os.OpenRoot(options.SpoolRoot)
	if err != nil {
		return nil, "", false, err
	}
	defer base.Close()
	if err = admitSpool(base); err != nil {
		return nil, "", false, err
	}
	info, err := base.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, name, false, nil
	}
	if err != nil {
		return nil, name, false, err
	}
	if !info.IsDir() {
		return nil, name, false, storage.NewError("invalid_config", "", nil)
	}
	root, err := base.OpenRoot(name)
	if err != nil {
		return nil, name, false, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, name, false, storage.NewError("invalid_config", "", err)
	}
	return root, name, true, nil
}

func readSpoolLedger(root *os.Root, name, namespace string) (*ledger, os.FileInfo, error) {
	file, err := spoolOpen(root, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, nil, fail("invalid_evidence", "")
	}
	var r ledger
	decoder := json.NewDecoder(io.LimitReader(file, (4<<20)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, nil, fail("invalid_evidence", "")
	}
	if r.Format != 1 || r.Binding != namespace || r.Kind == "" || r.ID == "" || !validAttempt(r.Attempt) {
		return nil, nil, fail("invalid_evidence", "")
	}
	return &r, info, nil
}

// CheckSpoolSettled is the admission check for one exact object binding. It refuses a
// non-terminal record and any spool entry it cannot classify, because either one can
// still describe an effect that has not settled. Settled records, their payloads and
// the permanent locks are accepted: they can no longer land an effect, so they must
// not make a disposable spool require hand deletion before a rebuild.
func CheckSpoolSettled(binding storage.Config, options Options) error {
	root, namespace, exists, err := openSpoolNamespace(binding, options)
	if err != nil || !exists {
		return err
	}
	defer root.Close()
	listing, err := root.Open(".")
	if err != nil {
		return err
	}
	defer listing.Close()
	for {
		entries, readErr := listing.ReadDir(256)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, dirEntry := range entries {
			name := dirEntry.Name()
			if ignorableSpoolName(name) {
				continue
			}
			if !ledgerRecordName(name) {
				return storage.NewError("baseline_required", "", nil)
			}
			r, _, err := readSpoolLedger(root, name, namespace)
			if err != nil || name != keyFor(r.Kind, r.ID)+".json" {
				return storage.NewError("baseline_required", "", nil)
			}
			if !settledPhase(r.Phase) {
				return storage.NewError("baseline_required", "", nil)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

// PruneSettled removes settled intent ledger records whose retention has passed. It is
// a no-op for every other entry: a non-terminal record is never removed, an unreadable
// or unknown file is left untouched, and apply=false only counts candidates.
//
// Every removal happens under the same per-intent lock a writer takes, then rereads the
// record under that lock, so a live operation is skipped rather than raced. Payloads are
// removed before the record that names them, making an interrupted run leave a settled
// record that the next run can simply finish; the next run is idempotent by construction.
func PruneSettled(binding storage.Config, options Options, now time.Time, apply bool) (int, error) {
	if now.IsZero() {
		return 0, storage.NewError("invalid_config", "", nil)
	}
	now = now.UTC()
	root, namespace, exists, err := openSpoolNamespace(binding, options)
	if err != nil || !exists {
		return 0, err
	}
	defer root.Close()
	listing, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	entries, err := listing.ReadDir(-1)
	listing.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	pruned := 0
	for _, dirEntry := range entries {
		name := dirEntry.Name()
		if !ledgerRecordName(name) {
			continue
		}
		r, info, err := readSpoolLedger(root, name, namespace)
		if err != nil || name != keyFor(r.Kind, r.ID)+".json" {
			continue
		}
		if !settledPhase(r.Phase) || !retentionPassed(r, info, now) {
			continue
		}
		if !apply {
			pruned++
			continue
		}
		unlock, ok, err := tryLedgerLock(root, keyFor(r.Kind, r.ID)+".lock")
		if err != nil {
			return pruned, err
		}
		if !ok {
			continue
		}
		// Reread under the lock: a writer may have changed the phase or deadline
		// between the first read and the lock.
		r, info, err = readSpoolLedger(root, name, namespace)
		if err != nil || name != keyFor(r.Kind, r.ID)+".json" || !settledPhase(r.Phase) || !retentionPassed(r, info, now) {
			unlock()
			continue
		}
		if err = removeSettledRecord(root, r); err != nil {
			unlock()
			return pruned, err
		}
		unlock()
		pruned++
	}
	return pruned, nil
}

func tryLedgerLock(root *os.Root, name string) (func(), bool, error) {
	file, err := spoolOpen(root, name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, false, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		file.Close()
		return nil, false, fail("invalid_evidence", "")
	}
	locked, err := lockFile(file)
	if err != nil {
		file.Close()
		return nil, false, err
	}
	if !locked {
		file.Close()
		return nil, false, nil
	}
	return func() { _ = unlockFile(file); _ = file.Close() }, true, nil
}

// removeSettledRecord removes the private payload first, then the record. The record
// is the authority, so an interruption between the two leaves a settled record whose
// payload is already gone and whose next prune is still safe.
func removeSettledRecord(root *os.Root, r *ledger) error {
	if r.Attempt != "" {
		if err := root.Remove(keyFor(r.Kind, r.ID) + "-" + r.Attempt + ".data"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := root.Remove(keyFor(r.Kind, r.ID) + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(root)
}
