//go:build darwin || linux

package objectmutation

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// spoolPath names one ledger entry the way an operator can find it. Every syscall
// below used to return the bare errno — "operation not permitted", with no file and
// no step — so a settlement that could not open its own record logged only that
// string. A stalled import then looked like a stuck worker instead of a refused
// file, and the one fact needed to fix it (which entry, which call) was the one
// thing the error did not carry.
func spoolPath(root *os.Root, name string) string { return filepath.Join(root.Name(), name) }

func spoolOpen(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: spoolPath(root, name), Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// lockFile reports (false, nil) for a lock another holder owns, because that is a
// retry, not a failure. Anything else is a filesystem answer and keeps its file.
func lockFile(file *os.File, path string) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, &os.PathError{Op: "flock", Path: path, Err: err}
	}
	return true, nil
}

func unlockFile(file *os.File, path string) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return &os.PathError{Op: "flock", Path: path, Err: err}
	}
	return nil
}
