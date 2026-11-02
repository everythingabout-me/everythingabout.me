//go:build darwin || linux

package objectmutation

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func spoolOpen(root *os.Root, name string, flags int, mode os.FileMode) (*os.File, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func lockFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
func unlockFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
