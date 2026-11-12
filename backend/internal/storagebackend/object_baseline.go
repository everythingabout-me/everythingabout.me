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
	if err = objectmutation.CheckSpoolSettled(binding, b.objectOptions); err != nil {
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
			// The complete native scan already established this prefix folder; it
			// needs no marker object.
			if e.Kind == storage.Folder && storage.IsCode(err, "not_found") {
				continue
			}
			return err
		}
		invalidETag := obj.ETag == "" || strings.ContainsAny(obj.ETag, "\r\n\x00")
		if obj.Key != key || invalidETag || obj.Size != e.Size || e.Kind == storage.Folder && obj.Size != 0 {
			return storage.NewError("unsupported", e.Path, nil)
		}
	}
	return objectmutation.CheckSpoolSettled(binding, b.objectOptions)
}
