//go:build !darwin && !linux

package localstorage

import (
	"everythingabout.me/backend/internal/storage"
	"os"
)

const actionSupported = false

func actionRename(*os.Root, string, *os.Root, string, bool) error {
	return storage.NewError("unsupported", "", nil)
}
