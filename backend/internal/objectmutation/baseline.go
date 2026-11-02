package objectmutation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"everythingabout.me/backend/internal/storage"
)

// CheckSpoolEmpty inspects a single root's exact private namespace without
// creating it. Other owners' namespaces are not deletion/admission evidence.
func CheckSpoolEmpty(binding storage.Config, options Options) error {
	if !filepath.IsAbs(options.SpoolRoot) || storage.ValidateBinding(binding) != nil {
		return storage.NewError("invalid_config", "", nil)
	}
	base, err := os.OpenRoot(options.SpoolRoot)
	if err != nil {
		return err
	}
	defer base.Close()
	if err = admitSpool(base); err != nil {
		return err
	}
	binding.Credentials = nil
	data, _ := json.Marshal(binding)
	hash := sha256.Sum256(data)
	name := "object-mutations-v1-" + hex.EncodeToString(hash[:])
	info, err := base.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return storage.NewError("invalid_config", "", nil)
	}
	root, err := base.OpenRoot(name)
	if err != nil {
		return err
	}
	defer root.Close()
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		return storage.NewError("invalid_config", "", err)
	}
	listing, err := root.Open(".")
	if err != nil {
		return err
	}
	defer listing.Close()
	entries, err := listing.ReadDir(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 0 {
		return storage.NewError("baseline_required", "", nil)
	}
	return nil
}
