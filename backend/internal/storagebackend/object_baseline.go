package storagebackend

import (
	"context"
	"strings"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// ValidateObjectBaseline performs read-only checks on the exact enumerated owner
// tree and its gateway spool. Provider history usage is separately evidenced by
// the operator; current HEAD/listing cannot discover retained bytes by inference.
func (b *Backend) ValidateObjectBaseline(ctx context.Context, ownerID string, entries []storage.Entry) error {
	if b.objectBackend == nil || !b.cfg.Storage.CoordinatedWrites {
		return storage.NewError("unsupported", "", nil)
	}
	binding, err := b.Binding(ownerID)
	if err != nil {
		return err
	}
	if err = objectmutation.CheckSpoolEmpty(binding, b.objectOptions); err != nil {
		return err
	}
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if e.Path == "" || storage.ValidatePath(e.Path) != nil {
			return storage.NewError("invalid_name", e.Path, nil)
		}
		key := binding.ContentRoot + string(e.Path)
		if e.Kind == storage.Folder {
			key += "/"
		} else if e.Kind != storage.File {
			return storage.NewError("invalid_name", e.Path, nil)
		}
		obj, err := b.objectBackend.Head(ctx, key)
		if err != nil {
			return err
		}
		if obj.Key != key || obj.VersionID == "" || obj.VersionID == "null" || strings.ContainsAny(obj.VersionID, "\r\n\x00") || obj.Size != e.Size || e.Kind == storage.Folder && obj.Size != 0 {
			return storage.NewError("unsupported", e.Path, nil)
		}
	}
	return objectmutation.CheckSpoolEmpty(binding, b.objectOptions)
}
