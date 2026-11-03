package objectmutation

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func (d *Driver) SignedUploadSupported() bool {
	backend, ok := d.backend.(SignedMultipartBackend)
	return ok && backend.SignedPartsSupported() && d.options.Admission.SignedMultipartParts && d.UploadMaxFileBytes() > 0
}
func validSignedPart(in storage.WriteIntent, p storage.SignedUploadPart) bool {
	count := (in.Size + nativePartSize - 1) / nativePartSize
	expected := nativePartSize
	if int64(p.Number) == count {
		expected = in.Size - int64(p.Number-1)*nativePartSize
	}
	digest, e := base64.StdEncoding.DecodeString(p.MD5)
	hash, herr := hex.DecodeString(p.SHA256)
	return p.Number > 0 && int64(p.Number) <= count && p.Size == expected && e == nil && len(digest) == 16 && base64.StdEncoding.EncodeToString(digest) == p.MD5 && herr == nil && len(hash) == 32 && strings.ToLower(p.SHA256) == p.SHA256
}
func (d *Driver) AuthorizePart(ctx context.Context, in storage.WriteIntent, p storage.SignedUploadPart, metadata storage.UploadMetadata, expires time.Time) (storage.SignedPartGrant, error) {
	var grant storage.SignedPartGrant
	if err := d.validateUploadIntent(in); err != nil {
		return grant, err
	}
	if !d.SignedUploadSupported() {
		return grant, fail("unsupported", in.Target)
	}
	if !validSignedPart(in, p) || !expires.After(time.Now()) || expires.After(time.Now().Add(6*time.Minute)) {
		return grant, fail("invalid_request", in.Target)
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return grant, err
	}
	defer done()
	session, err := d.nativeAllocation(ctx, in, metadata)
	if err != nil {
		return grant, err
	}
	err = metadata.WithUploadMetadata(ctx, d.namespace, in.ID, p.Number, func(record storage.UploadMetadataRecord) error {
		current, e := readNative[nativeSession](ctx, record, "session")
		if e != nil {
			return e
		}
		if current == nil || !sameJSON(current.Intent, in) || current.UploadID != session.UploadID || current.Phase != "receiving" {
			return fail("upload_aborted", in.Target)
		}
		candidate, e := readNative[nativePartReceipt](ctx, record, partMetadataKey(p.Number))
		if e != nil {
			return e
		}
		if candidate == nil {
			// Persist immutable content BEFORE issuing replayable bearer authority. Grants
			// may survive browser cancellation and logout; only this exact slot is writable.
			candidate = &nativePartReceipt{Number: p.Number, Size: p.Size, SHA256: p.SHA256, UploadID: session.UploadID, Uncertain: true, MD5: p.MD5, Signed: true}
			if e = record.Put(ctx, partMetadataKey(p.Number), encodeNative(candidate)); e != nil {
				return e
			}
		}
		if !candidate.Signed || candidate.Number != p.Number || candidate.Size != p.Size || candidate.SHA256 != p.SHA256 || candidate.MD5 != p.MD5 || candidate.UploadID != session.UploadID {
			return fail("part_conflict", in.Target)
		}
		// Renewal must not mutate confirmed Handle bytes. Uncertain is permanently true
		// for signed authority; successful Complete is what consumes the native upload.
		grant, e = d.backend.(SignedMultipartBackend).SignPart(ctx, d.key(in.Target, storage.File), session.UploadID, p.Number, p.Size, p.MD5, expires)
		return e
	})
	return grant, err
}
func (d *Driver) ConfirmPart(ctx context.Context, in storage.WriteIntent, number int, metadata storage.UploadMetadata, accept func(storage.UploadPart) (storage.UploadPart, error)) (storage.UploadPart, error) {
	var result storage.UploadPart
	if err := d.validateUploadIntent(in); err != nil {
		return result, err
	}
	if !d.SignedUploadSupported() {
		return result, fail("unsupported", in.Target)
	}
	count := (in.Size + nativePartSize - 1) / nativePartSize
	if number < 1 || int64(number) > count || accept == nil {
		return result, fail("invalid_request", in.Target)
	}
	ctx, done, err := d.operation(ctx)
	if err != nil {
		return result, err
	}
	defer done()
	err = metadata.WithUploadMetadata(ctx, d.namespace, in.ID, number, func(record storage.UploadMetadataRecord) error {
		session, e := readNative[nativeSession](ctx, record, "session")
		if e != nil {
			return e
		}
		if session == nil || !sameJSON(session.Intent, in) || session.Phase != "receiving" {
			return fail("upload_aborted", in.Target)
		}
		candidate, e := readNative[nativePartReceipt](ctx, record, partMetadataKey(number))
		if e != nil {
			return e
		}
		if candidate == nil || !candidate.Signed || candidate.UploadID != session.UploadID {
			return fail("part_pending", in.Target)
		}
		if candidate.ETag == "" {
			info, found, e := d.backend.(SignedMultipartBackend).QueryPart(ctx, d.key(in.Target, storage.File), session.UploadID, number)
			if e != nil {
				return e
			}
			if !found {
				return fail("part_pending", in.Target)
			}
			md5bytes, e := base64.StdEncoding.DecodeString(candidate.MD5)
			if e != nil {
				return fail("invalid_evidence", in.Target)
			}
			if info.Size != candidate.Size || strings.ToLower(strings.Trim(info.ETag, "\"")) != hex.EncodeToString(md5bytes) {
				return fail("digest_mismatch", in.Target)
			}
			candidate.ETag = info.ETag
			if e = record.Put(ctx, partMetadataKey(number), encodeNative(candidate)); e != nil {
				return e
			}
		}
		result = storage.UploadPart{Number: number, Size: candidate.Size, SHA256: candidate.SHA256, Handle: string(encodeNative(candidate))}
		accepted, e := accept(result)
		if e != nil {
			return e
		}
		if !sameJSON(accepted, result) {
			return fail("invalid_evidence", in.Target)
		}
		return nil
	})
	return result, err
}
