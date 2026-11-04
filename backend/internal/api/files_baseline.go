package api

import (
	"context"
	"log/slog"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// filesBoundDriver is the immutable non-secret identity every coordinated writer
// exposes. A driver that cannot prove its binding is never admitted automatically.
type filesBoundDriver interface {
	storage.Driver
	StorageBinding() storage.Config
	StorageNameRules() storage.NameRules
}

func filesRootFor(content storage.Driver) (fileops.Root, bool) {
	bound, ok := content.(filesBoundDriver)
	if !ok {
		return fileops.Root{}, false
	}
	binding := bound.StorageBinding()
	return fileops.Root{
		OwnerID:         binding.OwnerID,
		ConnectionID:    binding.ConnectionID,
		Generation:      binding.Generation,
		ContentRoot:     binding.ContentRoot,
		SystemRoot:      binding.SystemRoot,
		CaseSensitive:   bound.StorageNameRules().CaseSensitive,
		RetainsVersions: content.Describe().RetainsVersions,
	}, true
}

// emptyOwnerVerifier proves an owner root has never held native content. Only the
// storage backend can answer that from the exact driver and spool.
type emptyOwnerVerifier interface {
	VerifyEmptyOwner(context.Context, string, storage.Driver) error
}

// filesUsage returns owner-wide usage and establishes the zero baseline for a
// provably empty owner on its first write. Every other missing or mismatched
// baseline still requires the explicit operator admission.
func (s *Server) filesUsage(ctx context.Context, native fileops.NativeActions, root fileops.Root) (fileops.Usage, error) {
	actor := fileops.Actor{ID: "owner:" + root.OwnerID, CanRead: true, CanWrite: true, ManageOwner: true}
	usage, err := s.filesJournal.Usage(ctx, root, actor)
	if err == nil || !fileops.IsCode(err, "baseline_required") {
		return usage, err
	}
	if provisionErr := s.filesProvisionEmptyOwner(ctx, root.OwnerID, native); provisionErr != nil {
		if !expectedProvisionOutcome(provisionErr) {
			slog.Warn("empty owner baseline provisioning failed", "owner_id", root.OwnerID, "error", provisionErr)
		}
		return usage, err
	}
	return s.filesJournal.Usage(ctx, root, actor)
}

// expectedProvisionOutcome separates "this owner is not empty" and "automatic
// admission is unavailable" from an actual storage or database failure.
func expectedProvisionOutcome(err error) bool {
	for _, code := range []string{"baseline_required", "unsupported", "invalid_config", "root_conflict"} {
		if fileops.IsCode(err, code) || storage.IsCode(err, code) {
			return true
		}
	}
	return false
}

func (s *Server) filesProvisionEmptyOwner(ctx context.Context, ownerID string, content storage.Driver) error {
	if s.cfg == nil || !s.cfg.Storage.CoordinatedWrites || s.filesJournal == nil {
		return fileops.E("unsupported", nil)
	}
	verifier, ok := s.trees.(emptyOwnerVerifier)
	if !ok {
		return fileops.E("unsupported", nil)
	}
	root, ok := filesRootFor(content)
	if !ok {
		return fileops.E("unsupported", nil)
	}
	// Cheap index/accounting precondition before the complete native scan. An owner
	// with any prior row is never auto-admitted; a restore cannot masquerade as new.
	active, err := s.filesJournal.HasFileActivity(ctx, root)
	if err != nil {
		return err
	}
	if active {
		return fileops.E("baseline_required", nil)
	}
	if err = verifier.VerifyEmptyOwner(ctx, ownerID, content); err != nil {
		return err
	}
	return s.filesJournal.AdmitEmptyOwner(ctx, root, s.cfg.QuotaBytes, 0)
}

// filesProvisionNewOwner runs the same admission right after sign-up so the new
// account is writable without a first-write scan. Failure is never fatal: the
// first write retries through filesUsage, and a storage outage must not block
// account creation.
func (s *Server) filesProvisionNewOwner(ctx context.Context, ownerID string) {
	if s.cfg == nil || !s.cfg.Storage.CoordinatedWrites || s.trees == nil || s.filesJournal == nil {
		return
	}
	provision, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	driver, err := s.trees.Open(provision, ownerID)
	if err != nil {
		slog.Warn("new owner baseline provisioning could not open the owner root", "owner_id", ownerID, "error", err)
		return
	}
	defer driver.Close()
	if err = s.filesProvisionEmptyOwner(provision, ownerID, driver); err != nil {
		if !expectedProvisionOutcome(err) {
			slog.Warn("new owner baseline provisioning failed", "owner_id", ownerID, "error", err)
		}
	}
}
