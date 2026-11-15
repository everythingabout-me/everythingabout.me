//go:build !darwin && !linux

package objectmutation

import "os"

func admitSpool(*os.Root) error { return fail("unsupported", "") }
func spoolOpen(*os.Root, string, int, os.FileMode) (*os.File, error) {
	return nil, fail("unsupported", "")
}
func spoolPath(*os.Root, string) string       { return "" }
func lockFile(*os.File, string) (bool, error) { return false, fail("unsupported", "") }
func unlockFile(*os.File, string) error       { return fail("unsupported", "") }
