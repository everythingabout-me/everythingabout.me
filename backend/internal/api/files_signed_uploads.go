package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

const filesTransferPrefix = "/api/files/transfer/"

// checkedUploadMetadata rechecks authorization/lifecycle after acquiring the
// native fence, before a delayed request can allocate or issue more authority.
type checkedUploadMetadata struct {
	storage.UploadMetadata
	check func(context.Context) error
}

func (m checkedUploadMetadata) WithUploadMetadata(ctx context.Context, binding, id string, number int, fn func(storage.UploadMetadataRecord) error) error {
	return m.UploadMetadata.WithUploadMetadata(ctx, binding, id, number, func(record storage.UploadMetadataRecord) error {
		if err := m.check(ctx); err != nil {
			return err
		}
		return fn(record)
	})
}
func (s *Server) signedPartContext(r *http.Request, f *fileRequest) (storage.SignedNumberedUploadWriter, fileops.Root, fileops.Actor, fileops.Upload, storage.WriteIntent, int, int64, error) {
	var intent storage.WriteIntent
	native, root, actor, u, err := s.filesUploadContext(r, f)
	if err != nil {
		return nil, root, actor, u, intent, 0, 0, err
	}
	signed, ok := native.(storage.SignedNumberedUploadWriter)
	if !s.cfg.EdgeOneUploadIngress() || !ok || !signed.SignedUploadSupported() {
		return nil, root, actor, u, intent, 0, 0, fc.NewProblem(422, "unsupported_option", "Signed COS part transport is unavailable.")
	}
	if err = filesUploadStateError(u.State); err != nil {
		return nil, root, actor, u, intent, 0, 0, filesMutationProblem(err)
	}
	if u.State != "open" {
		return nil, root, actor, u, intent, 0, 0, fc.NewProblem(409, "commit_in_progress", "The part manifest is frozen.")
	}
	segments := strings.Split(r.URL.Path, "/")
	raw := segments[len(segments)-2]
	number, e := strconv.Atoi(raw)
	if e != nil || number < 1 || number > u.PartCount || strconv.Itoa(number) != raw {
		return nil, root, actor, u, intent, 0, 0, fc.NewProblem(400, "invalid_request", "The part number is outside the session geometry.")
	}
	_ = json.Unmarshal(u.Operation.Intent, &intent)
	intent.ID = u.ID
	target, parseErr := fc.FromRelative(string(intent.Target))
	if parseErr != nil {
		return nil, root, actor, u, intent, 0, 0, parseErr
	}
	if writeErr := f.RequireWrite(target); writeErr != nil {
		return nil, root, actor, u, intent, 0, 0, writeErr
	}
	size := u.PartSize
	if number == u.PartCount {
		size = intent.Size - int64(number-1)*u.PartSize
	}
	return signed, root, actor, u, intent, number, size, nil
}
func signedPartProblem(err error) error {
	if storage.IsCode(err, "part_pending") {
		return fc.NewProblem(409, "part_pending", "The native part has not been confirmed yet.")
	}
	if storage.IsCode(err, "invalid_request") {
		return fc.NewProblem(400, "invalid_request", "The part checksums or geometry are invalid.")
	}
	if storage.IsCode(err, "unsupported") {
		return fc.NewProblem(422, "unsupported_option", "Signed COS parts are not admitted.")
	}
	return filesUploadSpoolProblem(err)
}
func (s *Server) handleFilesAuthorizeUploadPart(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	signed, root, actor, u, intent, number, size, err := s.signedPartContext(r, f)
	if err != nil {
		return err
	}
	var req struct {
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		MD5    string `json:"md5"`
	}
	if err = strictFilesJSON(r, &req); err != nil {
		return err
	}
	if req.Size != size {
		return fc.NewProblem(400, "length_mismatch", "The part size must match its session geometry.")
	}
	metadata := checkedUploadMetadata{UploadMetadata: s.filesJournal, check: func(ctx context.Context) error {
		current, e := s.filesJournal.GetUpload(ctx, root, actor, u.ID)
		if e != nil {
			return e
		}
		if current.State != "open" {
			return fileops.E("commit_in_progress", nil)
		}
		return nil
	}}
	expires := minTime(time.Now().UTC().Add(5*time.Minute), u.ExpiresAt)
	grant, err := signed.AuthorizePart(r.Context(), intent, storage.SignedUploadPart{Number: number, Size: size, SHA256: req.SHA256, MD5: req.MD5}, metadata, expires)
	if err != nil {
		return signedPartProblem(err)
	}
	if len(grant.Headers) != 2 || grant.Headers["Content-MD5"] != req.MD5 || grant.Headers["Content-Type"] != "application/octet-stream" || grant.ExpiresAt.After(expires) || !grant.ExpiresAt.After(time.Now()) {
		return fc.NewProblem(503, "temporarily_unavailable", "The native signed request constraints are invalid.")
	}
	private, e := url.Parse(grant.URL)
	if e != nil || private.Scheme != "https" && private.Scheme != "http" || private.Host == "" || private.User != nil || private.Fragment != "" || grant.Method != "PUT" || !strings.HasPrefix(private.EscapedPath(), "/") {
		return fc.NewProblem(503, "temporarily_unavailable", "The native signed request is invalid.")
	}
	// Prefix the exact escaped native path. EO strips only this fixed prefix and
	// preserves the suffix/query; never decode and re-encode native filenames here.
	target := filesTransferPrefix + strings.TrimPrefix(private.EscapedPath(), "/")
	if private.RawQuery != "" {
		target += "?" + private.RawQuery
	}
	return writeFilesJSON(w, 200, map[string]any{"method": grant.Method, "url": target, "headers": grant.Headers, "expiresAt": grant.ExpiresAt})
}
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
func (s *Server) handleFilesConfirmUploadPart(w http.ResponseWriter, r *http.Request, f *fileRequest) error {
	signed, root, actor, u, intent, number, _, err := s.signedPartContext(r, f)
	if err != nil {
		return err
	}
	// Confirmation has no client ETag authority and accepts no payload fields.
	if r.ContentLength > 0 {
		var req struct{}
		if err = strictFilesJSON(r, &req); err != nil {
			return err
		}
	}
	var receipt fileops.PartReceipt
	var replay bool
	_, err = signed.ConfirmPart(r.Context(), intent, number, s.filesJournal, func(part storage.UploadPart) (storage.UploadPart, error) {
		var e error
		receipt, replay, e = s.filesJournal.AcceptUploadPart(r.Context(), root, actor, u.ID, fileops.PartReceipt{Number: part.Number, Size: part.Size, SHA256: part.SHA256, Handle: part.Handle})
		return storage.UploadPart{Number: receipt.Number, Size: receipt.Size, SHA256: receipt.SHA256, Handle: receipt.Handle}, e
	})
	if err != nil {
		return signedPartProblem(err)
	}
	status := 201
	if replay {
		status = 200
	}
	return writeFilesJSON(w, status, filesUploadReceipt(receipt))
}
