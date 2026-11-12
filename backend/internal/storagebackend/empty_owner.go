package storagebackend

import (
	"context"

	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

// VerifyEmptyOwner proves that the exact owner root holds no current native entry
// and that no staging/spool state could still land a prior effect. It is the native
// half of the only automatic baseline: a zero count is safe exactly when nothing
// exists, and settled intent records are evidence that nothing can still arrive.
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
		return localstorage.CheckSettledSystemRoot(binding.SystemRoot)
	case "cos", "s3":
		// A backend that admits concurrent native writers cannot promise that the
		// empty prefix stays empty until the admission row commits.
		if b.objectBackend == nil || !b.objectOptions.Admission.SingleGateway {
			return storage.NewError("baseline_required", "", nil)
		}
		return objectmutation.CheckSpoolSettled(binding, b.objectOptions)
	default:
		return storage.NewError("unsupported", "", nil)
	}
}
