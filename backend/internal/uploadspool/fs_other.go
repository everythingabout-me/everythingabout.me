//go:build !darwin && !linux

package uploadspool

import (
	"context"
	"everythingabout.me/backend/internal/storage"
	"os"
)

func unsupported() error                                      { return storage.NewError("unsupported", "", nil) }
func openBase(string) (*os.Root, error)                       { return nil, unsupported() }
func childDirectory(*os.Root, string, bool) (*os.Root, error) { return nil, unsupported() }
func localFilesystem(*os.Root) (bool, error)                  { return false, unsupported() }
func createFile(*os.Root, string) (*os.File, error)           { return nil, unsupported() }
func readFile(*os.Root, string) (*os.File, error)             { return nil, unsupported() }
func readSmall(*os.Root, string, int64) ([]byte, error)       { return nil, unsupported() }
func immutable(*os.Root, string, []byte) error                { return unsupported() }
func syncDirectory(*os.Root) error                            { return unsupported() }
func lockFile(context.Context, *os.Root, string, bool, bool) (func(), error) {
	return nil, unsupported()
}
func linkFile(*os.Root, string, *os.Root, string) error { return unsupported() }
func removeRegular(*os.Root, string) error              { return unsupported() }
