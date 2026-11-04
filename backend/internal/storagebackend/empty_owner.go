package storagebackend

import (
	"context"
	"strings"

	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

// VerifyEmptyOwner proves that the exact owner root holds no current native entry
// and that no staging/spool state could hide a prior effect. It is the native half
// of the only automatic baseline: a zero count is safe exactly when nothing exists.
// Callers must still commit the admission atomically with an owner-scoped
// emptiness check; a positive result is not a snapshot or a writer fence.
func (b *Backend) VerifyEmptyOwner(ctx context.Context, ownerID string, content storage.Driver) error {
	if b == nil || !b.cfg.Storage.CoordinatedWrites || content == nil {
		return storage.NewError("unsupported", "", nil)
	}
	bound, ok := content.(interface {
		StorageBinding() storage.Config
		StorageNameRules() storage.NameRules
	})
	if !ok {
		return storage.NewError("unsupported", "", nil)
	}
	binding, err := b.Binding(ownerID)
	if err != nil {
		return err
	}
	got := bound.StorageBinding()
	if got.OwnerID != binding.OwnerID || got.ConnectionID != binding.ConnectionID || got.Generation != binding.Generation || got.ContentRoot != binding.ContentRoot || got.SystemRoot != binding.SystemRoot {
		return storage.NewError("invalid_config", "", nil)
	}
	// Retained generations make physical usage unknowable from a current listing.
	if content.Describe().RetainsVersions {
		return storage.NewError("baseline_required", "", nil)
	}
	scanned, err := treescan.Scan(ctx, content, treescan.Options{Names: bound.StorageNameRules()})
	if err != nil {
		return err
	}
	records, err := scanned.Records()
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return storage.NewError("baseline_required", "", nil)
	}
	switch b.cfg.BackendName() {
	case "local":
		return b.verifyEmptySystemRoot(ctx, binding)
	case "cos", "s3":
		// A backend that admits concurrent native writers cannot promise that the
		// empty prefix stays empty until the admission row commits.
		if b.objectBackend == nil || !b.objectOptions.Admission.SingleGateway {
			return storage.NewError("baseline_required", "", nil)
		}
		return objectmutation.CheckSpoolEmpty(binding, b.objectOptions)
	default:
		return storage.NewError("unsupported", "", nil)
	}
}

// verifyEmptySystemRoot lists the owner's system root through a reversed binding so
// a staged or intent record from an earlier lifetime is never mistaken for a fresh
// account. A coordinated Local writer creates its own empty staging namespace when
// the driver opens, so that one empty directory is the only entry the check admits.
// It opens no mutation path and deletes nothing.
func (b *Backend) verifyEmptySystemRoot(ctx context.Context, binding storage.Config) error {
	factory, ok := b.factory.(*localstorage.Factory)
	if !ok {
		return storage.NewError("invalid_config", "", nil)
	}
	reversed := binding
	reversed.ContentRoot, reversed.SystemRoot = binding.SystemRoot, binding.ContentRoot
	tree, err := factory.OpenOwner(ctx, reversed, b.cfg.Storage.Local.SystemRoot, b.cfg.Storage.Local.ContentRoot)
	if err != nil {
		return err
	}
	defer tree.Close()
	page, err := tree.List(ctx, "", storage.PageRequest{Limit: 2})
	if err != nil {
		return err
	}
	if len(page.Entries) == 0 && page.NextCursor == "" {
		return nil
	}
	if len(page.Entries) != 1 || page.NextCursor != "" {
		return storage.NewError("baseline_required", "", nil)
	}
	staging := page.Entries[0]
	if staging.Kind != storage.Folder || !strings.HasPrefix(string(staging.Path), "staged-v1-") {
		return storage.NewError("baseline_required", "", nil)
	}
	inner, err := tree.List(ctx, staging.Path, storage.PageRequest{Limit: 1})
	if err != nil {
		return err
	}
	if len(inner.Entries) != 0 || inner.NextCursor != "" {
		return storage.NewError("baseline_required", "", nil)
	}
	return nil
}
