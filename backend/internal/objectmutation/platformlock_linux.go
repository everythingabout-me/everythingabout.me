//go:build linux

package objectmutation

import (
	"golang.org/x/sys/unix"
	"os"
)

func admitSpool(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}
	defer file.Close()
	var info unix.Statfs_t
	if err = unix.Fstatfs(int(file.Fd()), &info); err != nil {
		return err
	}
	switch uint64(info.Type) {
	case 0xef53, 0x58465342, 0x9123683e, 0xf2f52010, 0x01021994, 0x858458f6:
		return nil
	}
	return fail("unsupported", "")
}
