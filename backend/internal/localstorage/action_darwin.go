//go:build darwin

package localstorage

import (
	"golang.org/x/sys/unix"
	"os"
)

const actionSupported = true

func actionRename(from *os.Root, old string, to *os.Root, name string, replace bool) error {
	a, err := from.Open(".")
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := to.Open(".")
	if err != nil {
		return err
	}
	defer b.Close()
	if replace {
		return unix.Renameat(int(a.Fd()), old, int(b.Fd()), name)
	}
	return unix.RenameatxNp(int(a.Fd()), old, int(b.Fd()), name, unix.RENAME_EXCL)
}
