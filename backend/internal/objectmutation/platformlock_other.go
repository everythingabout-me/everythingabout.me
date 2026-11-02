//go:build !darwin && !linux

package objectmutation

import "os"

func admitSpool(*os.Root) error { return fail("unsupported", "") }
func spoolOpen(*os.Root, string, int, os.FileMode) (*os.File, error) {
	return nil, fail("unsupported", "")
}
func lockFile(*os.File) (bool, error) { return false, fail("unsupported", "") }
func unlockFile(*os.File) error       { return fail("unsupported", "") }
