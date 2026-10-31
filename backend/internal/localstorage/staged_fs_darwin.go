//go:build darwin

package localstorage

import (
	"bytes"
	"os"

	"golang.org/x/sys/unix"
)

// Recovery relies on a returned native syscall being terminal. Network mounts
// are not admitted, even when content and staging report the same device.
func stagedLocalFilesystem(root *os.Root) (bool, error) {
	f, err := root.Open(".")
	if err != nil {
		return false, err
	}
	defer f.Close()
	var info unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &info); err != nil {
		return false, err
	}
	name := string(bytes.TrimRight(info.Fstypename[:], "\x00"))
	return info.Flags&unix.MNT_LOCAL != 0 && (name == "apfs" || name == "hfs"), nil
}
