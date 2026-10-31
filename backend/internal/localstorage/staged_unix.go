//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package localstorage

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const stagedSupported = true

var errStagedLocked = errors.New("local stage intent is in flight")

func stagedSameFilesystem(a, b os.FileInfo) bool {
	aa, ok := a.Sys().(*syscall.Stat_t)
	bb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok && ok2 && aa.Dev == bb.Dev
}
func stagedPrivate(info os.FileInfo) bool {
	s, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.IsDir() && s.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0
}
func stagedCreate(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
}
func stagedLock(root *os.Root, name string) (func(), error) {
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("invalid local stage lock")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errStagedLocked
		}
		return nil, err
	}
	if err := syncRoot(root); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

// Both names are single components under independently pinned directory FDs.
// linkat is an atomic no-replace operation. Its temporary second hard link is
// removed only by Discard; no operation ever writes this staged inode again.
func stagedLink(source *os.Root, old string, target *os.Root, name string) error {
	from, err := source.Open(".")
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := target.Open(".")
	if err != nil {
		return err
	}
	defer to.Close()
	return unix.Linkat(int(from.Fd()), old, int(to.Fd()), name, 0)
}
