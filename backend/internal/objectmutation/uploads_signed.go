package objectmutation

import (
	"context"
	"errors"
	"os"
	"time"

	"everythingabout.me/backend/internal/storage"
)

var (
	_ storage.PlannedUploadWriter = (*Driver)(nil)
	_ storage.ReadGranter         = (*Driver)(nil)
)

// class A: the driver opens unpublished provider state and signs exact-length
// part URLs. It never receives bytes; the server commit seals the provider part
// listing and performs the single publishing call.

func (d *Driver) SignedUploadSupported() bool {
	backend, ok := d.backend.(SignedPartBackend)
	return ok && backend.SignedPartsSupported() && d.options.Admission.MultipartUploads && d.UploadMaxFileBytes() > 0
}

func (d *Driver) ReadGrantSupported() bool {
	backend, ok := d.backend.(ReadSignerBackend)
	return ok && backend.ReadSigningSupported()
}

func plannedPartWindow(partCount int) int {
	const window = 32
	if partCount < window {
		return partCount
	}
	return window
}

func plannedPartCount(in storage.WriteIntent) (int, error) {
	if in.Size < 0 {
		return 0, fail("invalid_request", in.Target)
	}
	return int((in.Size + nativePartSize - 1) / nativePartSize), nil
}

func (d *Driver) BeginPlannedUpload(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata, expires time.Time) (storage.PlannedUpload, error) {
	var plan storage.PlannedUpload
	if err := d.validateUploadIntent(in); err != nil {
		return plan, err
	}
	if !d.SignedUploadSupported() {
		return plan, fail("unsupported", in.Target)
	}
	if !expires.After(time.Now()) {
		return plan, fail("invalid_request", in.Target)
	}
	if in.Size == 0 {
		return storage.PlannedUpload{PartSize: nativePartSize, ExpiresAt: expires.UTC()}, nil
	}
	count, err := plannedPartCount(in)
	if err != nil {
		return plan, err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return plan, err
	}
	defer done()
	session, err := d.nativeAllocation(ctx, in, metadata)
	if err != nil {
		return plan, err
	}
	numbers := make([]int, 0, plannedPartWindow(count))
	for number := 1; number <= plannedPartWindow(count); number++ {
		numbers = append(numbers, number)
	}
	parts, err := d.signPlannedParts(ctx, in, session.UploadID, numbers, expires)
	if err != nil {
		return plan, err
	}
	return storage.PlannedUpload{Handle: []byte(session.UploadID), PartSize: nativePartSize, Parts: parts, ExpiresAt: expires.UTC()}, nil
}

// SupportsDirectPut reports whether the backend can sign a conditional PUT at the
// destination. A backend without that capability keeps the multipart path.
func (d *Driver) SupportsDirectPut() bool {
	_, ok := d.backend.(DirectPutBackend)
	return ok && d.SignedUploadSupported()
}

// BeginDirectPut signs one conditional PUT at the destination itself. Nothing is staged
// and no provider request is made here: the client's PUT creates the object, and the
// session's commit verifies it. The size bound is the part size, because a file that needs
// more than one request cannot be published by a single conditional PUT.
func (d *Driver) BeginDirectPut(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	var grant storage.SignedPartGrant
	if err := d.validateUploadIntent(in); err != nil {
		return grant, err
	}
	backend, ok := d.backend.(DirectPutBackend)
	if !ok || !d.SupportsDirectPut() {
		return grant, fail("unsupported", in.Target)
	}
	if in.Size <= 0 || in.Size > nativePartSize {
		return grant, fail("unsupported", in.Target)
	}
	if !in.Conditions.Absent && len(in.Conditions.ExpectedRevision) == 0 {
		return grant, fail("precondition_required", in.Target)
	}
	if !expires.After(time.Now()) {
		return grant, fail("invalid_request", in.Target)
	}
	return backend.SignPut(ctx, d.key(in.Target, storage.File), in.Size, in.MediaType, in.Conditions, expires)
}

// SupportsDirectPost reports whether the backend can sign a form upload whose policy
// refuses to replace an occupied key. A backend without that capability keeps the
// conditional PUT.
func (d *Driver) SupportsDirectPost() bool {
	_, ok := d.backend.(DirectPostBackend)
	return ok && d.SignedUploadSupported()
}

// BeginDirectPost signs one create as a form upload. Nothing is staged and no provider
// request is made here: the client's POST creates the object, and the session's commit
// verifies it exactly as it verifies a direct PUT. Only a create can be expressed — the
// provider's form upload has no "replace only this revision" condition — so a replacement
// keeps the conditional PUT.
func (d *Driver) BeginDirectPost(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	var grant storage.SignedPartGrant
	if err := d.validateUploadIntent(in); err != nil {
		return grant, err
	}
	backend, ok := d.backend.(DirectPostBackend)
	if !ok || !d.SupportsDirectPost() {
		return grant, fail("unsupported", in.Target)
	}
	if in.Size <= 0 || in.Size > nativePartSize {
		return grant, fail("unsupported", in.Target)
	}
	if !in.Conditions.Absent {
		return grant, fail("precondition_required", in.Target)
	}
	if !expires.After(time.Now()) || expires.Sub(time.Now()) > time.Hour {
		return grant, fail("invalid_request", in.Target)
	}
	return backend.SignPost(ctx, d.key(in.Target, storage.File), in.Size, in.MediaType, expires)
}

func (d *Driver) SignUploadParts(ctx context.Context, in storage.WriteIntent, numbers []int, metadata storage.UploadMetadata, expires time.Time) ([]storage.SignedPartGrant, error) {
	if err := d.validateUploadIntent(in); err != nil {
		return nil, err
	}
	if !d.SignedUploadSupported() {
		return nil, fail("unsupported", in.Target)
	}
	if !expires.After(time.Now()) || len(numbers) == 0 || len(numbers) > 256 {
		return nil, fail("invalid_request", in.Target)
	}
	count, err := plannedPartCount(in)
	if err != nil {
		return nil, err
	}
	seen := make(map[int]bool, len(numbers))
	for _, number := range numbers {
		if number < 1 || number > count || seen[number] {
			return nil, fail("invalid_request", in.Target)
		}
		seen[number] = true
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	var session *nativeSession
	err = d.withPlannedSession(ctx, in, metadata, -1, func(current *nativeSession) error {
		session = current
		return nil
	})
	if err != nil {
		return nil, err
	}
	if session == nil || session.UploadID == "" {
		return nil, fail("not_found", in.Target)
	}
	return d.signPlannedParts(ctx, in, session.UploadID, numbers, expires)
}

func (d *Driver) signPlannedParts(ctx context.Context, in storage.WriteIntent, uploadID string, numbers []int, expires time.Time) ([]storage.SignedPartGrant, error) {
	backend := d.backend.(SignedPartBackend)
	count, err := plannedPartCount(in)
	if err != nil {
		return nil, err
	}
	parts := make([]storage.SignedPartGrant, 0, len(numbers))
	for _, number := range numbers {
		size := nativePartSize
		if number == count {
			size = in.Size - int64(number-1)*nativePartSize
		}
		grant, err := backend.SignPart(ctx, d.key(in.Target, storage.File), uploadID, number, size, expires)
		if err != nil {
			return nil, err
		}
		if grant.Number != number || grant.Size != size || grant.URL == "" || grant.Method == "" || grant.ExpiresAt.IsZero() {
			return nil, fail("invalid_backend_response", in.Target)
		}
		parts = append(parts, grant)
	}
	return parts, nil
}

func (d *Driver) ListUploadParts(ctx context.Context, in storage.WriteIntent, after, limit int, metadata storage.UploadMetadata) ([]storage.NativeUploadPart, bool, error) {
	if err := d.validateUploadIntent(in); err != nil {
		return nil, false, err
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return nil, false, err
	}
	defer done()
	count, err := plannedPartCount(in)
	if err != nil {
		return nil, false, err
	}
	if count == 0 {
		return nil, false, nil
	}
	var session *nativeSession
	err = d.withPlannedSession(ctx, in, metadata, -1, func(current *nativeSession) error {
		session = current
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if session == nil || session.UploadID == "" {
		return nil, false, fail("not_found", in.Target)
	}
	if after < 0 || limit < 1 {
		return nil, false, fail("invalid_request", in.Target)
	}
	backend := d.backend.(SignedPartBackend)
	return backend.ListParts(ctx, d.key(in.Target, storage.File), session.UploadID, after, limit)
}

func (d *Driver) SealPlannedUpload(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata, cp storage.Checkpoint) (storage.Staged, error) {
	if err := d.validateUploadIntent(in); err != nil {
		return storage.Staged{}, err
	}
	if !d.SignedUploadSupported() && in.Size != 0 {
		return storage.Staged{}, fail("unsupported", in.Target)
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return storage.Staged{}, err
	}
	defer done()
	unlock, err := d.lock(ctx, "write", in.ID, false)
	if err != nil {
		return storage.Staged{}, err
	}
	defer unlock()
	r, err := d.load("write", in.ID)
	if err == nil {
		if !sameJSON(r.Intent, in) || r.Native == nil {
			return storage.Staged{}, fail("intent_conflict", in.Target)
		}
		if r.Phase != "ready" && r.Phase != "applied" {
			return storage.Staged{}, fail("outcome_unknown", in.Target)
		}
		if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
			return storage.Staged{}, err
		}
		return d.staged(r), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return storage.Staged{}, err
	}
	stage := &nativeStage{}
	if in.Size > 0 {
		if metadata == nil {
			return storage.Staged{}, fail("invalid_config", in.Target)
		}
		count, e := plannedPartCount(in)
		if e != nil {
			return storage.Staged{}, e
		}
		err = metadata.WithUploadMetadata(ctx, d.namespace, in.ID, 0, func(record storage.UploadMetadataRecord) error {
			session, e := readNative[nativeSession](ctx, record, "session")
			if e != nil {
				return e
			}
			if session == nil || !sameJSON(session.Intent, in) || session.Phase != "receiving" && session.Phase != "sealed" {
				return fail("invalid_evidence", in.Target)
			}
			listed, e := d.listAllProviderParts(ctx, in, session.UploadID, count)
			if e != nil {
				return e
			}
			stage.UploadID = session.UploadID
			stage.Parts = listed
			session.Phase = "sealed"
			return record.Put(ctx, "session", encodeNative(session))
		})
		if err != nil {
			return storage.Staged{}, err
		}
	}
	attempt, err := randomAttempt()
	if err != nil {
		return storage.Staged{}, err
	}
	r = &ledger{Kind: "write", ID: in.ID, Intent: cloneIntent(in), Attempt: attempt, Phase: "ready", ParentRevision: append([]byte(nil), in.ParentRevision...), Native: stage}
	if err = d.save(r); err != nil {
		return storage.Staged{}, err
	}
	if err = checkpoint(ctx, cp, d.receipt(r)); err != nil {
		return storage.Staged{}, err
	}
	return d.staged(r), nil
}

func (d *Driver) DiscardPlannedUpload(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata) error {
	return d.DiscardParts(ctx, in, metadata)
}

func (d *Driver) listAllProviderParts(ctx context.Context, in storage.WriteIntent, uploadID string, count int) ([]NativePart, error) {
	if count == 0 {
		return nil, nil
	}
	backend := d.backend.(SignedPartBackend)
	parts := make([]NativePart, 0, count)
	after := 0
	for {
		page, more, err := backend.ListParts(ctx, d.key(in.Target, storage.File), uploadID, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 && more {
			return nil, fail("invalid_backend_response", in.Target)
		}
		for _, part := range page {
			expected := nativePartSize
			if part.Number == count {
				expected = in.Size - int64(part.Number-1)*nativePartSize
			}
			if part.Number != len(parts)+1 || part.Size != expected || part.ETag == "" {
				return nil, fail("upload_incomplete", in.Target)
			}
			parts = append(parts, NativePart{Number: part.Number, ETag: part.ETag})
			after = part.Number
		}
		if !more {
			break
		}
	}
	if len(parts) != count {
		return nil, fail("upload_incomplete", in.Target)
	}
	return parts, nil
}

func (d *Driver) withPlannedSession(ctx context.Context, in storage.WriteIntent, metadata storage.UploadMetadata, number int, fn func(*nativeSession) error) error {
	if metadata == nil {
		return fail("invalid_config", in.Target)
	}
	return metadata.WithUploadMetadata(ctx, d.namespace, in.ID, number, func(record storage.UploadMetadataRecord) error {
		session, err := readNative[nativeSession](ctx, record, "session")
		if err != nil {
			return err
		}
		if session != nil && !sameJSON(session.Intent, in) {
			return fail("intent_conflict", in.Target)
		}
		return fn(session)
	})
}

func (d *Driver) GrantRead(ctx context.Context, p storage.Path, options storage.ReadGrantOptions) (storage.ReadGrant, error) {
	if err := storage.ValidatePath(p); err != nil || p == "" {
		return storage.ReadGrant{}, fail("invalid_request", p)
	}
	backend, ok := d.backend.(ReadSignerBackend)
	if !ok || !backend.ReadSigningSupported() {
		return storage.ReadGrant{}, fail("unsupported", p)
	}
	return backend.PresignRead(ctx, d.key(p, storage.File), options)
}
