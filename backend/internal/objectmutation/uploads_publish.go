package objectmutation

import (
	"bytes"
	"context"
	"errors"

	"everythingabout.me/backend/internal/storage"
)

// publishNativeLocked sends only control metadata for nonempty files. The sole
// final-path RPC remains behind the same durable publication fence as simple PUT.
func (d *Driver) publishNativeLocked(ctx context.Context, r *ledger, cp storage.Checkpoint) (storage.MutationResult, error) {
	if err := d.validateUploadIntent(r.Intent); err != nil {
		return storage.MutationResult{}, err
	}
	verify := func() error {
		parent, err := d.Stat(ctx, parentPath(r.Intent.Target))
		if err != nil {
			return err
		}
		if parent.Kind != storage.Folder || !bytes.Equal(parent.Revision, r.ParentRevision) {
			return fail("changed", r.Intent.Target)
		}
		return d.checkTarget(ctx, r)
	}
	if err := verify(); err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	r.Phase = "publishing"
	if err := d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err := checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.MutationResult{}, err
	}
	if err := ctx.Err(); err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	if err := verify(); err != nil {
		r.Phase = "not_applied"
		return storage.MutationResult{}, errors.Join(&storage.NoFinalEffect{Cause: err}, d.save(r))
	}
	request := Put{Key: d.key(r.Intent.Target, storage.File), Size: r.Intent.Size, MediaType: r.Intent.MediaType, Metadata: map[string]string{"eam-intent": r.ID}, Absent: r.Intent.Conditions.Absent, ExpectedETag: r.ExpectedETag}
	var object Object
	var err error
	if r.Intent.Size == 0 {
		request.Body = bytes.NewReader(nil)
		object, err = d.backend.Put(ctx, request)
	} else {
		object, err = d.backend.(MultipartBackend).CompleteMultipart(ctx, request, r.Native.UploadID, r.Native.Parts)
	}
	if err != nil {
		if storage.IsCode(err, "precondition_failed") || storage.IsCode(err, "name_conflict") {
			r.Phase = "not_applied"
			return storage.MutationResult{}, errors.Join(err, d.save(r))
		}
		// Remote errors, missing revision evidence and lost replies remain unknown.
		return storage.MutationResult{}, err
	}
	entry, err := d.objectEntry(r.Intent.Target, storage.File, object)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if entry.Size != r.Intent.Size {
		return storage.MutationResult{}, fail("invalid_backend_response", r.Intent.Target)
	}
	r.Phase = "applied"
	result := storage.MutationResult{Entry: &entry, Evidence: d.receipt(r)}
	r.Result = &result
	if err = d.save(r); err != nil {
		return storage.MutationResult{}, err
	}
	if err = d.refreshApplied(ctx, r); err != nil {
		return storage.MutationResult{}, err
	}
	return cloneResult(*r.Result), checkpoint(ctx, cp, d.receipt(r))
}
