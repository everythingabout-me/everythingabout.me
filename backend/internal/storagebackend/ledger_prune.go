package storagebackend

import (
	"context"
	"time"

	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// PruneSettled removes expired, settled intent-ledger records for one owner. It never
// consults or mutates index rows, never removes a non-terminal record, and leaves any
// entry it cannot classify untouched. The caller gets the number of records it removed,
// or would remove when apply is false.
func (b *Backend) PruneSettled(ctx context.Context, ownerID string, now time.Time, apply bool) (int, error) {
	if b == nil {
		return 0, storage.NewError("unsupported", "", nil)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	binding, err := b.Binding(ownerID)
	if err != nil {
		return 0, err
	}
	switch b.cfg.BackendName() {
	case "local":
		return localstorage.PruneSettledSystemRoot(binding.SystemRoot, now, apply)
	case "cos", "s3":
		return objectmutation.PruneSettled(binding, b.objectOptions, now, apply)
	default:
		return 0, storage.NewError("unsupported", "", nil)
	}
}
