package fileops

import (
	"bytes"
	"context"
	"io"

	"everythingabout.me/backend/internal/storage"
)

// DirectUploadWriter settles an upload whose bytes the client already PUT at the
// destination itself. Nothing is staged and nothing is published here: the client's
// conditional PUT was the publication, so Stage observes the object it created and
// Publish re-observes it. Discard removes that object when the session is aborted before
// it settled, conditional on the exact revision this writer observed.
//
// The writer still goes through the ordinary journal pipeline (claim, stage, publish,
// settle), because the index settlement, quota and audit of a direct publication are the
// same facts a multipart publication produces; only the native effect is missing.
type DirectUploadWriter struct {
	NativeActions
}

func NewDirectUploadWriter(native NativeActions) *DirectUploadWriter {
	return &DirectUploadWriter{NativeActions: native}
}

func (w *DirectUploadWriter) Stage(ctx context.Context, in storage.WriteIntent, body io.Reader, cp storage.Checkpoint) (storage.Staged, error) {
	entry, err := w.Stat(ctx, in.Target)
	if err != nil {
		return storage.Staged{}, err
	}
	if entry.Kind != storage.File {
		return storage.Staged{}, E("not_a_file", nil)
	}
	// The presigned URL bound the exact length, so a different one is not the object this
	// session admitted (and a replace that kept the old length would still be the new
	// revision, which the condition below proves).
	if entry.Size != in.Size {
		return storage.Staged{}, E("length_mismatch", nil)
	}
	// A replace names the revision it may overwrite, and the destination still holding that
	// exact revision is *not* proof that the client skipped its PUT: bytes identical to the
	// ones already there produce the identical ETag. Re-importing an unchanged folder — the
	// ordinary case for a person re-uploading last month's export — therefore refused every
	// file at settlement with body_reception_failed, after its client had already applied the
	// PUT, and no row could ever finish. The check that refused equality read "the client
	// never wrote the replacement" into a fact about content, which cannot say that.
	//
	// What remains after the length check is a destination of the admitted size: settle it.
	// The bytes a caller asked to publish are the bytes already there, whichever way the
	// equality came about, and a destination that no longer holds the revision the session
	// named is refused by the PUT's own If-Match before this writer is reached.
	return storage.Staged{Intent: in, Size: entry.Size, Handle: entry.Revision}, nil
}

func (w *DirectUploadWriter) Publish(ctx context.Context, staged storage.Staged, cp storage.Checkpoint) (storage.MutationResult, error) {
	entry, err := w.Stat(ctx, staged.Intent.Target)
	if err != nil {
		return storage.MutationResult{}, err
	}
	if entry.Kind != storage.File || entry.Size != staged.Size {
		return storage.MutationResult{}, E("invalid_native_result", nil)
	}
	return storage.MutationResult{Entry: &entry, Evidence: observation(entry)}, nil
}

// observation is the evidence a direct publication can offer: the target's own revision,
// the value a later HEAD has to reproduce to prove those are this session's bytes. A staged
// publication carries the mutation ledger's receipt instead. The distinction matters
// because Service.Write refuses any applied result that carries no evidence — a direct
// commit without one retries forever as outcome_unknown while the file sits at the
// destination, unsettled and unindexed.
func observation(entry storage.Entry) []byte {
	return bytes.Clone(entry.Revision)
}

// Inspect answers what a lost attempt did, from the object itself: the same rules Stage
// applies, without claiming a new effect.
func (w *DirectUploadWriter) Inspect(ctx context.Context, in storage.WriteIntent, evidence []byte) (storage.Inspection, error) {
	entry, err := w.Stat(ctx, in.Target)
	if storage.IsCode(err, "not_found") {
		return storage.Inspection{State: storage.NotApplied}, nil
	}
	if err != nil {
		return storage.Inspection{}, err
	}
	if entry.Kind != storage.File || entry.Size != in.Size {
		return storage.Inspection{State: storage.NotApplied}, nil
	}
	if len(in.Conditions.ExpectedRevision) > 0 && bytes.Equal(entry.Revision, in.Conditions.ExpectedRevision) {
		return storage.Inspection{State: storage.NotApplied}, nil
	}
	staged := storage.Staged{Intent: in, Size: entry.Size, Handle: entry.Revision}
	return storage.Inspection{State: storage.Applied, Stage: &staged, Evidence: observation(entry),
		Result: &storage.MutationResult{Entry: &entry, Evidence: observation(entry)}}, nil
}

// Discard does nothing. A direct publication has no temporary identity to discard: the
// object at the destination is either the file this session published or somebody else's
// file that happened to be there, and the writer cannot tell them apart from the bytes it
// observes. Removing an aborted session's landed object therefore belongs to the caller
// that knows the session never settled (abort/expiry), not to the post-settle cleanup that
// also reaches this method for a successful commit.
func (w *DirectUploadWriter) Discard(ctx context.Context, in storage.WriteIntent, evidence []byte) error {
	return nil
}
