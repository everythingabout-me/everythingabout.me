//go:build darwin

package uploadspool

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
)

func localFilesystem(root *os.Root) (bool, error) {
	file, err := root.Open(".")
	if err != nil {
		return false, err
	}
	defer file.Close()
	var info unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &info); err != nil {
		return false, err
	}
	name := string(bytes.TrimRight(info.Fstypename[:], "\x00"))
	return info.Flags&unix.MNT_LOCAL != 0 && (name == "apfs" || name == "hfs"), nil
}
