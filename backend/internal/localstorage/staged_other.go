//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package localstorage

import (
	"errors"
	"os"
)

const stagedSupported = false

var errStagedLocked = errors.New("local stage intent is in flight")

func stagedSameFilesystem(a, b os.FileInfo) bool { return false }
func stagedPrivate(info os.FileInfo) bool        { return false }
func stagedCreate(root *os.Root, name string) (*os.File, error) {
	return nil, errors.New("safe local staging unsupported")
}
func stagedLock(root *os.Root, name string) (func(), error) {
	return nil, errors.New("safe local locking unsupported")
}
func stagedLink(source *os.Root, old string, target *os.Root, name string) error {
	return errors.New("safe local publication unsupported")
}
