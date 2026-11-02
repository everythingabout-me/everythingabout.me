//go:build darwin

package objectmutation

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
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
	raw := make([]byte, len(info.Fstypename))
	for i, b := range info.Fstypename {
		raw[i] = byte(b)
	}
	name := strings.TrimRight(string(raw), "\x00")
	if info.Flags&unix.MNT_LOCAL == 0 || (name != "apfs" && name != "hfs") {
		return fail("unsupported", "")
	}
	return nil
}
