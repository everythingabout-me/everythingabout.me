//go:build darwin || linux

package uploadspool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func private(info os.FileInfo, directory bool) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0077 == 0 && ((directory && info.IsDir()) || (!directory && info.Mode().IsRegular()))
}
func openBase(name string) (*os.Root, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok || !before.IsDir() || stat.Uid != uint32(os.Geteuid()) || before.Mode().Perm()&0022 != 0 {
		return nil, ErrInvalid
	}
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, errors.Join(ErrIntegrity, err)
	}
	return root, nil
}
func childDirectory(parent *os.Root, name string, create bool) (*os.Root, error) {
	if create {
		if err := parent.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !private(before, true) {
		return nil, ErrIntegrity
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := root.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		root.Close()
		return nil, errors.Join(ErrIntegrity, err)
	}
	if create {
		if err := syncDirectory(parent); err != nil {
			root.Close()
			return nil, err
		}
	}
	return root, nil
}

// os.Root may resolve an in-root symlink even when OpenFile receives O_NOFOLLOW.
// All spool filenames are single components; direct openat on the held directory
// FD rejects such links as well as links escaping the root.
func noFollowOpen(root *os.Root, name string, flags int, mode uint32) (*os.File, error) {
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	// Concurrent O_CREAT|O_NOFOLLOW on admitted macOS filesystems can transiently
	// report ENOENT while another process creates the same directory entry.
	// Retry only creation, with the same pinned directory and no-follow flags.
	var fd int
	for attempt := 0; attempt < 8; attempt++ {
		fd, err = unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
		if err == nil || flags&os.O_CREATE == 0 || !errors.Is(err, syscall.ENOENT) {
			break
		}
	}
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}
func createFile(root *os.Root, name string) (*os.File, error) {
	return noFollowOpen(root, name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}
func readFile(root *os.Root, name string) (*os.File, error) {
	file, err := noFollowOpen(root, name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !private(info, false) {
		file.Close()
		return nil, errors.Join(ErrIntegrity, err)
	}
	return file, nil
}
func readSmall(root *os.Root, name string, max int64) ([]byte, error) {
	file, err := readFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrIntegrity
	}
	return data, nil
}
func immutable(root *os.Root, name string, data []byte) error {
	file, err := noFollowOpen(root, name, os.O_RDWR|os.O_CREATE|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !private(info, false) {
		return errors.Join(ErrIntegrity, err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	old, err := io.ReadAll(io.LimitReader(file, int64(len(data))+1))
	if err != nil {
		return err
	}
	if len(old) == 0 {
		if _, err := file.Write(data); err != nil {
			return err
		}
	} else if !bytes.Equal(old, data) {
		return ErrIntegrity
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDirectory(root)
}
func syncDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func lockFile(ctx context.Context, root *os.Root, name string, exclusive, wait bool) (func(), error) {
	file, err := noFollowOpen(root, name, os.O_RDWR|os.O_CREATE|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !private(info, false) {
		file.Close()
		return nil, errors.Join(ErrIntegrity, err)
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		if err := ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, err
		}
		if !wait {
			file.Close()
			return nil, ErrBusy
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	if err := syncDirectory(root); err != nil {
		file.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
func linkFile(source *os.Root, old string, target *os.Root, name string) error {
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
func removeRegular(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !private(info, false) {
		return ErrIntegrity
	}
	return root.Remove(name)
}
