//go:build linux

package uploadspool

import (
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
	// Match localstorage's conservative known-local admission. Remote/FUSE and
	// unverified overlays cannot provide the required local durability semantics.
	switch info.Type {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.F2FS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.RAMFS_MAGIC:
		return true, nil
	default:
		return false, nil
	}
}
