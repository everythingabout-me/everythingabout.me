//go:build darwin

package localstorage

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
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
	// A rename that is refused names both ends and the call: the bare errno this
	// used to return ("operation not permitted") told an operator nothing about
	// which entry in which tree was refused, which is the whole of what a local
	// settlement failure has to answer.
	if replace {
		err = unix.Renameat(int(a.Fd()), old, int(b.Fd()), name)
	} else {
		err = unix.RenameatxNp(int(a.Fd()), old, int(b.Fd()), name, unix.RENAME_EXCL)
	}
	if err != nil {
		return &os.LinkError{Op: "renameatx_np", Old: filepath.Join(from.Name(), old), New: filepath.Join(to.Name(), name), Err: err}
	}
	return nil
}
