//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package localstorage

import (
	"errors"
	"os"
)

const safeReadSupported = false

func rootIdentity(info os.FileInfo) string { return "" }

func openReadOnly(root *os.Root, name string) (*os.File, error) {
	return nil, errors.New("safe nonblocking no-follow reads are unsupported on this platform")
}
