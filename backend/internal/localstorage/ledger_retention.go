package localstorage

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// The private staging namespace is recovery evidence, not user data. An intent record
// whose operation settled can no longer hide a possible effect, so keeping it forever
// only made a disposable cache block the automatic baseline after an index loss. It is
// kept for storage.IntentRetention, the same window a terminal operation remains
// answerable, and then removed. A non-terminal record is never aged out: its native
// effect may still land. Admission therefore refuses any non-terminal record, while a
// settled record (and the permanent per-intent lock beside it) no longer blocks a
// rebuild.

// actionSettled names every action-ledger phase whose native outcome is final.
// "unresolved" is deliberately absent: it records a receipt whose ledger was lost and
// may still describe an effect that must stay a fence. "reserved", "staging" and
// "executing" may still perform one.
func actionSettled(phase string) bool {
	switch phase {
	case "applied", "not_applied":
		return true
	}
	return false
}

// stageSettled names every write-ledger phase with no possible publication.
// "failed" is terminal because Stage never issues a final-path request; "ready" and
// "publishing" are not, because a staged payload may still be published.
func stageSettled(phase string) bool {
	switch phase {
	case "applied", "not_applied", "discarded", "failed":
		return true
	}
	return false
}

// settleLedgerRetention stamps a terminal record once. Re-saving an already settled
// record must not move its deadline forward, and a record that regresses to a
// non-terminal phase loses its deadline entirely because cleanup may never remove it.
func settleLedgerRetention(phase string, retainUntil *time.Time, settled func(string) bool, now time.Time) {
	if !settled(phase) {
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
func retentionPassed(retainUntil time.Time, info os.FileInfo, now time.Time) bool {
	if retainUntil.IsZero() {
		if info == nil {
			return false
		}
		retainUntil = info.ModTime().Add(storage.IntentRetention)
	}
	return !now.Before(retainUntil)
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func actionRecordName(name string) bool {
	return strings.HasPrefix(name, "action-") && strings.HasSuffix(name, ".json") &&
		isLowerHex(strings.TrimSuffix(strings.TrimPrefix(name, "action-"), ".json"), 64)
}
func stageRecordName(name string) bool {
	return strings.HasSuffix(name, ".json") && isLowerHex(strings.TrimSuffix(name, ".json"), 64)
}
func actionLockName(name string) bool {
	return strings.HasPrefix(name, "action-") && strings.HasSuffix(name, ".lock") &&
		isLowerHex(strings.TrimSuffix(strings.TrimPrefix(name, "action-"), ".lock"), 64)
}
func stageLockName(name string) bool {
	return strings.HasSuffix(name, ".lock") && isLowerHex(strings.TrimSuffix(name, ".lock"), 64)
}
func actionFolderName(name string) bool {
	return strings.HasPrefix(name, "action-") && strings.HasSuffix(name, ".folder") &&
		isLowerHex(strings.TrimSuffix(strings.TrimPrefix(name, "action-"), ".folder"), 64)
}
func actionNextName(name string) bool {
	return strings.HasPrefix(name, "action-") && strings.HasSuffix(name, ".next") &&
		isLowerHex(strings.TrimSuffix(strings.TrimPrefix(name, "action-"), ".next"), 64)
}
func stageNextName(name string) bool {
	return strings.HasSuffix(name, ".next") && isLowerHex(strings.TrimSuffix(name, ".next"), 64)
}
func stageDataName(name string) bool {
	if !strings.HasSuffix(name, ".data") {
		return false
	}
	base, attempt, ok := strings.Cut(strings.TrimSuffix(name, ".data"), "-")
	return ok && isLowerHex(base, 64) && isLowerHex(attempt, 32)
}

// ignorableStagingName covers private artifacts that cannot themselves describe a
// possible effect: the permanent per-intent lock, a staged payload or prepared mkdir
// folder owned by a record, and a save scratch file. The authoritative .json record is
// not ignorable; it decides admission and cleanup.
func ignorableStagingName(name string) bool {
	return actionLockName(name) || stageLockName(name) || actionFolderName(name) ||
		actionNextName(name) || stageNextName(name) || stageDataName(name)
}

func readActionRecord(root *os.Root, name, binding string) (*actionLedger, os.FileInfo, error) {
	file, err := openReadOnly(root, name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, nil, storage.NewError("invalid_evidence", "", nil)
	}
	var r actionLedger
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF ||
		r.Version != 1 || r.Binding != binding || r.Intent.ID == "" ||
		name != actionKey(r.Intent.ID)+".json" {
		return nil, nil, storage.NewError("invalid_evidence", "", nil)
	}
	return &r, info, nil
}

func readStageRecord(root *os.Root, name, binding string) (*stageLedger, os.FileInfo, error) {
	file, err := openReadOnly(root, name)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return nil, nil, storage.NewError("invalid_evidence", "", nil)
	}
	var r stageLedger
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || decoder.Decode(new(any)) != io.EOF ||
		r.Version != 1 || r.Binding != binding || r.Intent.ID == "" || !validAttempt(r.Attempt) ||
		name != intentKey(r.Intent.ID)+".json" {
		return nil, nil, storage.NewError("invalid_evidence", "", nil)
	}
	return &r, info, nil
}

func stagingBinding(name string) (string, bool) {
	const prefix = "staged-v1-"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	binding := strings.TrimPrefix(name, prefix)
	return binding, isLowerHex(binding, 64)
}

// CheckSettledSystemRoot is the admission check for one owner's local system root. It
// accepts only the known staging namespaces and refuses any non-terminal record or
// entry it cannot classify, because either one can still describe an unsettled effect.
// Settled records, their payloads and the permanent locks are accepted: they can no
// longer land an effect, so they must not make a disposable spool require hand deletion
// before a rebuild. It opens no mutation path and deletes nothing.
func CheckSettledSystemRoot(systemRoot string) error {
	if systemRoot == "" {
		return storage.NewError("invalid_config", "", nil)
	}
	base, err := os.OpenRoot(systemRoot)
	if err != nil {
		return err
	}
	defer base.Close()
	listing, err := base.Open(".")
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
			binding, ok := stagingBinding(name)
			if !ok {
				return storage.NewError("baseline_required", "", nil)
			}
			info, err := base.Lstat(name)
			if err != nil || !stagedPrivate(info) {
				return storage.NewError("baseline_required", "", err)
			}
			root, err := base.OpenRoot(name)
			if err != nil {
				return storage.NewError("baseline_required", "", err)
			}
			actual, statErr := root.Stat(".")
			if statErr != nil || !os.SameFile(info, actual) {
				root.Close()
				return storage.NewError("baseline_required", "", statErr)
			}
			checkErr := checkSettledStaging(root, binding)
			root.Close()
			if checkErr != nil {
				return checkErr
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

func checkSettledStaging(root *os.Root, binding string) error {
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
			switch {
			case actionRecordName(name):
				r, _, err := readActionRecord(root, name, binding)
				if err != nil || !actionSettled(r.Phase) {
					return storage.NewError("baseline_required", "", nil)
				}
			case stageRecordName(name):
				r, _, err := readStageRecord(root, name, binding)
				if err != nil || !stageSettled(r.Phase) {
					return storage.NewError("baseline_required", "", nil)
				}
			case ignorableStagingName(name):
			default:
				return storage.NewError("baseline_required", "", nil)
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}

// PruneSettledSystemRoot removes settled local intent records whose retention has
// passed. It is a no-op for every other entry: a non-terminal record is never removed,
// an unreadable or unknown file is left untouched, and apply=false only counts
// candidates.
//
// Every removal happens under the same per-intent lock a writer takes, then rereads the
// record under that lock, so a live operation is skipped rather than raced. Payloads and
// the prepared mkdir folder are removed before the record that names them, making an
// interrupted run leave a settled record that the next run can simply finish; the next
// run is idempotent by construction.
func PruneSettledSystemRoot(systemRoot string, now time.Time, apply bool) (int, error) {
	if systemRoot == "" || now.IsZero() {
		return 0, storage.NewError("invalid_config", "", nil)
	}
	now = now.UTC()
	base, err := os.OpenRoot(systemRoot)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer base.Close()
	listing, err := base.Open(".")
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
		binding, ok := stagingBinding(name)
		if !ok {
			continue
		}
		info, err := base.Lstat(name)
		if err != nil || !stagedPrivate(info) {
			continue
		}
		root, err := base.OpenRoot(name)
		if err != nil {
			continue
		}
		actual, statErr := root.Stat(".")
		if statErr != nil || !os.SameFile(info, actual) {
			root.Close()
			continue
		}
		count, pruneErr := pruneSettledStaging(root, binding, now, apply)
		root.Close()
		pruned += count
		if pruneErr != nil {
			return pruned, pruneErr
		}
	}
	return pruned, nil
}

func pruneSettledStaging(root *os.Root, binding string, now time.Time, apply bool) (int, error) {
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
		switch {
		case actionRecordName(name):
			r, info, err := readActionRecord(root, name, binding)
			if err != nil || !actionSettled(r.Phase) || !retentionPassed(r.RetainUntil, info, now) {
				continue
			}
			if !apply {
				pruned++
				continue
			}
			unlock, ok, err := tryStagingLock(root, actionKey(r.Intent.ID)+".lock")
			if err != nil {
				return pruned, err
			}
			if !ok {
				continue
			}
			r, info, err = readActionRecord(root, name, binding)
			if err != nil || !actionSettled(r.Phase) || !retentionPassed(r.RetainUntil, info, now) {
				unlock()
				continue
			}
			if err = removeSettledAction(root, r); err != nil {
				unlock()
				return pruned, err
			}
			unlock()
			pruned++
		case stageRecordName(name):
			r, info, err := readStageRecord(root, name, binding)
			if err != nil || !stageSettled(r.Phase) || !retentionPassed(r.RetainUntil, info, now) {
				continue
			}
			if !apply {
				pruned++
				continue
			}
			unlock, ok, err := tryStagingLock(root, intentKey(r.Intent.ID)+".lock")
			if err != nil {
				return pruned, err
			}
			if !ok {
				continue
			}
			r, info, err = readStageRecord(root, name, binding)
			if err != nil || !stageSettled(r.Phase) || !retentionPassed(r.RetainUntil, info, now) {
				unlock()
				continue
			}
			if err = removeSettledStage(root, r); err != nil {
				unlock()
				return pruned, err
			}
			unlock()
			pruned++
		}
	}
	return pruned, nil
}

func tryStagingLock(root *os.Root, name string) (func(), bool, error) {
	unlock, err := stagedLock(root, name)
	if errors.Is(err, errStagedLocked) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return unlock, true, nil
}

// removeSettledAction removes the prepared mkdir folder first, then the record. The
// folder name is derived from this exact intent ID, so no other operation can own it.
func removeSettledAction(root *os.Root, r *actionLedger) error {
	if prepared := actionKey(r.Intent.ID) + ".folder"; r.Prepared == prepared {
		if err := root.Remove(r.Prepared); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := root.Remove(actionKey(r.Intent.ID) + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRoot(root)
}

func removeSettledStage(root *os.Root, r *stageLedger) error {
	if r.Attempt != "" {
		if err := root.Remove(dataName(r)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := root.Remove(intentKey(r.Intent.ID) + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRoot(root)
}
