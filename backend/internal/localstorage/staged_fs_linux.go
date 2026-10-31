//go:build linux

package localstorage

import (
	"os"

	"golang.org/x/sys/unix"
)

// Only known local kernel filesystems are admitted. NFS/CIFS/FUSE and unknown
// filesystems, including overlays with unverified backing mounts, are rejected:
// returning from their syscall need not prove remote request quiescence.
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
	switch info.Type {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.F2FS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.RAMFS_MAGIC:
		return true, nil
	default:
		return false, nil
	}
}
