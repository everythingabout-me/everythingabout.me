package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

const filesPartSignLimit = 256

func filesUploadIntent(u fileops.Upload) (storage.WriteIntent, error) {
	var intent storage.WriteIntent
	if err := json.Unmarshal(u.Operation.Intent, &intent); err != nil {
		return intent, fc.NewProblem(503, "temporarily_unavailable", "The upload intent is unavailable.")
	}
	return intent, nil
}

func (s *Server) filesPlannedWriter(native fileops.NativeActions) (storage.PlannedUploadWriter, error) {
	planned, ok := native.(storage.PlannedUploadWriter)
	if !ok {
		return nil, fc.NewProblem(422, "unsupported_option", "This storage connection does not support class A presigned parts.")
	}
	return planned, nil
}

func (s *Server) filesPartGrantExpiry(u fileops.Upload) time.Time {
	expires := time.Now().UTC().Add(filesReadGrantWindow)
	if u.ExpiresAt.Before(expires) {
		return u.ExpiresAt
	}
	return expires
}

// filesBeginPlannedUpload opens the native multipart allocation and returns the
// first signed window. The handle stays in the driver's durable metadata.
func (s *Server) filesBeginPlannedUpload(ctx context.Context, native fileops.NativeActions, u fileops.Upload) (map[string]any, error) {
	planned, err := s.filesPlannedWriter(native)
	if err != nil {
		return nil, err
	}
	intent, err := filesUploadIntent(u)
	if err != nil {
		return nil, err
	}
	plan, err := planned.BeginPlannedUpload(ctx, intent, s.filesJournal, s.filesPartGrantExpiry(u))
	if err != nil {
		return nil, filesMutationProblem(err)
	}
	if plan.PartSize != u.PartSize {
		return nil, fc.NewProblem(503, "temporarily_unavailable", "The upload part geometry no longer matches the session.")
	}
	parts := make([]any, 0, len(plan.Parts))
	for _, grant := range plan.Parts {
		payload, err := s.filesPartGrantPayload(grant)
		if err != nil {
			return nil, err
		}
		parts = append(parts, payload)
	}
	return map[string]any{
		"kind": storage.TransferPresignedParts, "parts": parts,
		"signUrl": fc.Base + "/uploads/" + u.ID + "/parts/sign", "expiresAt": plan.ExpiresAt.UTC(),
	}, nil
}

func (s *Server) filesUploadTransport(ctx context.Context, native fileops.NativeActions, u fileops.Upload) (any, error) {
	if s.filesTransferClass() == storage.TransferPresignedParts {
		return s.filesBeginPlannedUpload(ctx, native, u)
	}
	return map[string]any{"kind": storage.TransferRelay, "url": fc.Base + "/uploads/" + u.ID + "/parts"}, nil
}

func (s *Server) handleFilesSignUploadParts(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	if s.filesTransferClass() != storage.TransferPresignedParts {
		return fc.NewProblem(422, "unsupported_option", "This connection does not use class A presigned parts.")
	}
	native, _, _, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return err
	}
	if err = filesUploadStateError(u.State); err != nil {
		return filesMutationProblem(err)
	}
	if err = filesUploadExpired(u); err != nil {
		return filesMutationProblem(err)
	}
	if u.State != "open" {
		return fc.NewProblem(409, "commit_in_progress", "The part manifest is frozen.")
	}
	var req struct {
		Numbers []int `json:"numbers"`
	}
	if err = strictFilesJSON(r, &req); err != nil {
		return err
	}
	if len(req.Numbers) == 0 || len(req.Numbers) > filesPartSignLimit {
		return fc.NewProblem(400, "invalid_request", "Between one and 256 part numbers are required.")
	}
	planned, err := s.filesPlannedWriter(native)
	if err != nil {
		return err
	}
	intent, err := filesUploadIntent(u)
	if err != nil {
		return err
	}
	expires := s.filesPartGrantExpiry(u)
	grants, err := planned.SignUploadParts(r.Context(), intent, req.Numbers, s.filesJournal, expires)
	if err != nil {
		return filesMutationProblem(err)
	}
	parts := make([]any, 0, len(grants))
	for _, grant := range grants {
		payload, err := s.filesPartGrantPayload(grant)
		if err != nil {
			return err
		}
		parts = append(parts, payload)
	}
	return writeFilesJSON(w, 200, map[string]any{"parts": parts, "expiresAt": expires.UTC()})
}

// filesPlannedParts returns every part the provider actually holds for a class A
// session. The listing is provider evidence, never a receipt or a content digest.
func (s *Server) filesPlannedParts(ctx context.Context, native fileops.NativeActions, u fileops.Upload) ([]storage.NativeUploadPart, error) {
	planned, err := s.filesPlannedWriter(native)
	if err != nil {
		return nil, err
	}
	intent, err := filesUploadIntent(u)
	if err != nil {
		return nil, err
	}
	parts := []storage.NativeUploadPart{}
	after := 0
	for {
		page, more, err := planned.ListUploadParts(ctx, intent, after, 1000, s.filesJournal)
		if err != nil {
			if storage.IsCode(err, "not_found") {
				return parts, nil
			}
			return nil, filesMutationProblem(err)
		}
		if len(page) == 0 && more {
			return nil, fc.NewProblem(502, "storage_failure", "The provider part listing did not advance.")
		}
		parts = append(parts, page...)
		if !more {
			break
		}
		after = page[len(page)-1].Number
	}
	return parts, nil
}

func filesPlannedCoverage(intent storage.WriteIntent, parts []storage.NativeUploadPart) error {
	count := int((intent.Size + filesUploadPartSize - 1) / filesUploadPartSize)
	if len(parts) != count {
		return fileops.E("upload_incomplete", nil)
	}
	for index, part := range parts {
		number := index + 1
		expected := filesUploadPartSize
		if number == count {
			expected = intent.Size - int64(number-1)*filesUploadPartSize
		}
		if part.Number != number || part.Size != expected {
			return fileops.E("upload_incomplete", nil)
		}
	}
	return nil
}
