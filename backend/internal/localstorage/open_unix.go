//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package localstorage

import (
	"fmt"
	"os"
	"syscall"
)

const safeReadSupported = true

func rootIdentity(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}

func openReadOnly(root *os.Root, name string) (*os.File, error) {
	// Nonblocking prevents a raced replacement with a FIFO from hanging before
	// f.Stat can reject it. No-follow rejects a raced final-component symlink.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
