package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
)

const (
	// lifetime of the presigned upload URLs, and of the upload session itself.
	uploadTTL = time.Hour
	// limit for a single PutObject; past this, go multipart.
	uploadSingleMax = int64(5) << 30
	// per-object cap, kept conservative.
	uploadMaxObject = int64(5) << 40 // 5 TiB
	// minimum part size (COS wants at least 1 MiB for every part but the last).
	uploadMinPartSize = int64(5) << 20
	// caps how many part URLs one response carries, to keep the response small.
	uploadMaxParts = 1000
	isoFormat      = "2006-01-02T15:04:05.000Z"
)

type apiUploadBody struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Mime      string `json:"mime"`
	Overwrite bool   `json:"overwrite"`
}

type apiCompleteBody struct {
	UploadID string `json:"uploadId"`
	Parts    []struct {
		PartNumber int    `json:"partNumber"`
		ETag       string `json:"etag"`
	} `json:"parts"`
}

type uploadSession struct {
	ID               string
	UserID           string
	Path             string
	Size             int64
	Mime             string
	PartSize         int64
	ProviderUploadID string
	ExpiresAt        time.Time
	Completed        bool
	Cancelled        bool
}

func newUploadID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "u_" + hex.EncodeToString(buf), nil
}

// partSizeFor picks a part size that keeps the part count at or below uploadMaxParts.
func (s *Server) partSizeFor(size int64) int64 {
	partSize := s.cfg.UploadPartSize
	if partSize < uploadMinPartSize {
		partSize = uploadMinPartSize
	}
	if count := (size + partSize - 1) / partSize; count > uploadMaxParts {
		partSize = (size + uploadMaxParts - 1) / uploadMaxParts
		if partSize < uploadMinPartSize {
			partSize = uploadMinPartSize
		}
	}
	return partSize
}

// handleAPIUpload creates an upload session and hands out presigned URLs.
// Small files get one PUT URL; files over the threshold get a set of part URLs.
func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiUploadBody](r)
	if err != nil {
		return err
	}

	path, err := paths.Normalize(body.Path)
	if err != nil {
		return err
	}
	if path == "" {
		return httpx.BadRequest("path cannot be empty")
	}
	if err := p.RequireWrite(path); err != nil {
		return err
	}
	unlock, err := s.lockBrowserMutation(r.Context(), p.User.ID, path)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.assertParentDirs(r.Context(), p.User.ID, path); err != nil {
		return err
	}
	if body.Size <= 0 {
		return httpx.BadRequest("invalid file size")
	}
	if body.Size > uploadMaxObject {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "a single object is limited to 5 TiB")
	}

	mime := strings.TrimSpace(body.Mime)
	if mime == "" {
		mime = "application/octet-stream"
	}

	existing, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.IsDir {
		return httpx.Conflict("a directory with the same name already exists")
	}
	if existing != nil && !body.Overwrite {
		return httpx.Conflict("file already exists; set overwrite: true in the request body to overwrite")
	}
	if err := s.checkQuota(r.Context(), p.User.ID, body.Size, existing); err != nil {
		return err
	}
	// move aside any historical soft-deleted row on this path, or the new object overwrites it.

	// only one unfinished upload per path.
	if err := s.cancelPendingUploads(r.Context(), p.User.ID, path); err != nil {
		return err
	}
	s.cleanupExpiredUploads(r.Context())

	id, err := newUploadID()
	if err != nil {
		return err
	}
	expiresAt := time.Now().Add(uploadTTL).UTC()
	key := paths.KeyFor(path, p.User.ID)

	threshold := s.cfg.MultipartThreshold
	if threshold <= 0 {
		threshold = uploadSingleMax
	}

	if body.Size <= threshold {
		if _, err := s.db.Pool().Exec(r.Context(),
			`insert into upload_sessions (id, user_id, path, size, mime, part_size, expires_at)
			 values ($1, $2::uuid, $3, $4, $5, $4, $6)`,
			id, p.User.ID, path, body.Size, mime, expiresAt); err != nil {
			return err
		}
		url, err := s.storage.PutURL(r.Context(), key, int(uploadTTL.Seconds()))
		if err != nil {
			return err
		}
		s.recordAudit(r, p, "upload", path, http.StatusOK, map[string]any{
			"size": body.Size, "uploadId": id, "mode": "single",
		})
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"uploadId":  id,
			"mode":      "single",
			"partSize":  body.Size,
			"expiresAt": expiresAt.Format(isoFormat),
			"parts": []map[string]any{{
				"partNumber": 1,
				"url":        url,
				"headers":    map[string]string{"Content-Type": mime},
			}},
		})
		return nil
	}

	// multipart upload: create the multipart in COS first, then sign one PUT URL per part.
	partSize := s.partSizeFor(body.Size)
	count := int((body.Size + partSize - 1) / partSize)
	providerID, err := s.storage.InitiateMultipart(r.Context(), key)
	if err != nil {
		return err
	}

	parts := make([]map[string]any, 0, count)
	for number := 1; number <= count; number++ {
		url, err := s.storage.MultipartPartURL(
			r.Context(), key, providerID, number, int(uploadTTL.Seconds()),
		)
		if err != nil {
			// if signing fails, do not leave a half-dead multipart behind in the bucket.
			_ = s.storage.AbortMultipart(context.Background(), key, providerID)
			return err
		}
		parts = append(parts, map[string]any{
			"partNumber": number,
			"url":        url,
			"headers":    map[string]string{"Content-Type": mime},
		})
	}

	if _, err := s.db.Pool().Exec(r.Context(),
		`insert into upload_sessions (id, user_id, path, size, mime, part_size, expires_at, provider_upload_id)
		 values ($1, $2::uuid, $3, $4, $5, $6, $7, $8)`,
		id, p.User.ID, path, body.Size, mime, partSize, expiresAt, providerID); err != nil {
		_ = s.storage.AbortMultipart(context.Background(), key, providerID)
		return err
	}

	s.recordAudit(r, p, "upload", path, http.StatusOK, map[string]any{
		"size": body.Size, "uploadId": id, "mode": "multipart", "parts": count,
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"uploadId":  id,
		"mode":      "multipart",
		"partSize":  partSize,
		"expiresAt": expiresAt.Format(isoFormat),
		"parts":     parts,
	})
	return nil
}

// handleAPIComplete runs after the client has sent all bytes: verify the object, then index it.
func (s *Server) handleAPIComplete(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiCompleteBody](r)
	if err != nil {
		return err
	}
	if strings.TrimSpace(body.UploadID) == "" {
		return httpx.BadRequest("uploadId cannot be empty")
	}

	session, err := s.loadUploadSession(r.Context(), body.UploadID)
	if err != nil {
		return err
	}
	// someone else's session is also 404, so existence never leaks.
	if session == nil || session.UserID != p.User.ID {
		return httpx.NotFound("upload session not found")
	}
	unlock, err := s.lockBrowserMutation(r.Context(), p.User.ID, session.Path)
	if err != nil {
		return err
	}
	defer unlock()
	// Reload after admission so a preceding completion/cancellation is observed.
	session, err = s.loadUploadSession(r.Context(), body.UploadID)
	if err != nil {
		return err
	}
	if session == nil || session.UserID != p.User.ID {
		return httpx.NotFound("upload session not found")
	}
	if session.Completed {
		return httpx.Conflict("this upload already finished")
	}
	if session.Cancelled {
		return httpx.Conflict("this upload was superseded by a newer upload to the same path")
	}
	if time.Now().After(session.ExpiresAt) {
		return httpx.NewError(http.StatusGone, "upload session expired; start a new one")
	}
	if err := p.RequireWrite(session.Path); err != nil {
		return err
	}

	key := paths.KeyFor(session.Path, p.User.ID)

	var info *storage.ObjectInfo
	if session.ProviderUploadID != "" {
		parts, err := completeParts(body.Parts)
		if err != nil {
			return err
		}
		info, err = s.storage.CompleteMultipart(r.Context(), key, session.ProviderUploadID, parts)
		if err != nil {
			return err
		}
	} else {
		info, err = s.storage.Head(r.Context(), key)
		if err != nil {
			return err
		}
	}
	if info == nil {
		return httpx.BadRequest("object not found; the upload may not be finished")
	}
	if info.Size != session.Size {
		return httpx.BadRequest(fmt.Sprintf(
			"size mismatch: session declared %d bytes, object has %d bytes", session.Size, info.Size))
	}

	if err := s.nodes.EnsureParents(r.Context(), session.Path, p.User.ID); err != nil {
		return err
	}

	var etag, storageClass *string
	if info.ETag != "" {
		etag = &info.ETag
	}
	if info.StorageClass != "" {
		storageClass = &info.StorageClass
	}
	mime := session.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}

	var modified *time.Time
	if !info.LastModified.IsZero() {
		modified = &info.LastModified
	}

	node, err := s.nodes.UpsertFile(r.Context(), nodes.UpsertInput{
		OwnerID:          p.User.ID,
		Path:             session.Path,
		Size:             info.Size,
		Mime:             &mime,
		ETag:             etag,
		StorageClass:     storageClass,
		SourceModifiedAt: modified,
		Meta:             map[string]any{"mime": mime},
	})
	if err != nil {
		return err
	}
	s.timeline.Notify(p.User.ID)

	if _, err := s.db.Pool().Exec(r.Context(),
		`update upload_sessions set completed_at = now() where id = $1`, session.ID); err != nil {
		// the index is already written; failing here only weakens idempotency checks,
		// not worth failing the request over
		slog.Warn("upload: failed to mark session complete", "uploadId", session.ID, "error", err)
	}

	if node.ETag != nil {
		w.Header().Set("ETag", `"`+*node.ETag+`"`)
	}
	s.recordAudit(r, p, "upload_complete", session.Path, http.StatusCreated, map[string]any{
		"size": info.Size, "uploadId": session.ID, "mode": sessionMode(session),
	})
	httpx.WriteJSON(w, http.StatusCreated, toAPINode(node))
	return nil
}

func sessionMode(session *uploadSession) string {
	if session.ProviderUploadID != "" {
		return "multipart"
	}
	return "single"
}

func completeParts(raw []struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}) ([]storage.MultipartPart, error) {
	if len(raw) == 0 {
		return nil, httpx.BadRequest("multipart upload requires parts (partNumber and etag for each part)")
	}
	parts := make([]storage.MultipartPart, 0, len(raw))
	seen := make(map[int]bool, len(raw))
	for _, part := range raw {
		etag := strings.Trim(strings.TrimSpace(part.ETag), `"`)
		if part.PartNumber <= 0 || etag == "" {
			return nil, httpx.BadRequest("parts contains an invalid part")
		}
		if seen[part.PartNumber] {
			return nil, httpx.BadRequest("parts contains duplicate part numbers")
		}
		seen[part.PartNumber] = true
		parts = append(parts, storage.MultipartPart{PartNumber: part.PartNumber, ETag: etag})
	}
	return parts, nil
}

// cancelPendingUploads marks unfinished sessions for the same path as cancelled and aborts
// the COS multipart (otherwise the parts keep taking up space).
func (s *Server) cancelPendingUploads(ctx context.Context, userID, path string) error {
	rows, err := s.db.Pool().Query(ctx,
		`select id, provider_upload_id from upload_sessions
		  where user_id = $1::uuid and path = $2
		    and completed_at is null and cancelled_at is null`, userID, path)
	if err != nil {
		return err
	}
	type pending struct{ id, provider string }
	var list []pending
	for rows.Next() {
		var item pending
		if err := rows.Scan(&item.id, &item.provider); err != nil {
			rows.Close()
			return err
		}
		list = append(list, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, item := range list {
		if item.provider != "" {
			if err := s.storage.AbortMultipart(ctx, paths.KeyFor(path, userID), item.provider); err != nil {
				slog.Warn("upload: failed to abort previous multipart upload", "providerUploadId", item.provider, "error", err)
			}
		}
	}
	_, err = s.db.Pool().Exec(ctx,
		`update upload_sessions set cancelled_at = now()
		  where user_id = $1::uuid and path = $2
		    and completed_at is null and cancelled_at is null`, userID, path)
	return err
}

// cleanupExpiredUploads aborts multipart sessions that expired more than a day ago.
// Keep session evidence: an expired URL does not prove an in-flight PUT has settled.
func (s *Server) cleanupExpiredUploads(ctx context.Context) {
	rows, err := s.db.Pool().Query(ctx,
		`select id, path, user_id::text, provider_upload_id from upload_sessions
		  where expires_at < now() - interval '1 day'
		    and completed_at is null and provider_upload_id <> ''`)
	if err != nil {
		return
	}
	type expired struct{ id, path, userID, provider string }
	var list []expired
	for rows.Next() {
		var item expired
		if err := rows.Scan(&item.id, &item.path, &item.userID, &item.provider); err != nil {
			rows.Close()
			return
		}
		list = append(list, item)
	}
	rows.Close()

	for _, item := range list {
		if err := s.storage.AbortMultipart(ctx, paths.KeyFor(item.path, item.userID), item.provider); err != nil {
			slog.Warn("upload: failed to abort expired multipart upload", "uploadId", item.id, "error", err)
		}
	}
}

func (s *Server) loadUploadSession(ctx context.Context, id string) (*uploadSession, error) {
	var (
		session     uploadSession
		completedAt *time.Time
		cancelledAt *time.Time
	)
	err := s.db.Pool().QueryRow(ctx,
		`select id, user_id::text, path, size, mime, part_size, provider_upload_id,
		        expires_at, completed_at, cancelled_at
		   from upload_sessions
		  where id = $1`, id).
		Scan(&session.ID, &session.UserID, &session.Path, &session.Size, &session.Mime,
			&session.PartSize, &session.ProviderUploadID, &session.ExpiresAt,
			&completedAt, &cancelledAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	session.Completed = completedAt != nil
	session.Cancelled = cancelledAt != nil
	return &session, nil
}

// ---- GET /upload/{id} ----

// handleAPIUploadStatus lets a client that lost the connection ask "how far did this get?"
// before sending the remaining parts. The server tracks no parts; it asks COS directly (ListParts).
func (s *Server) handleAPIUploadStatus(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	session, err := s.loadUploadSession(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	// someone else's session is also 404, so existence never leaks.
	if session == nil || session.UserID != p.User.ID {
		return httpx.NotFound("upload session not found")
	}
	if err := p.RequireWrite(session.Path); err != nil {
		return err
	}

	status := "pending"
	switch {
	case session.Completed:
		status = "completed"
	case session.Cancelled:
		status = "cancelled"
	case time.Now().After(session.ExpiresAt):
		status = "expired"
	}

	received := make([]storage.PartInfo, 0, 8)
	if status == "pending" && session.ProviderUploadID != "" {
		parts, err := s.storage.ListParts(
			r.Context(), paths.KeyFor(session.Path, p.User.ID), session.ProviderUploadID)
		if err != nil {
			return err
		}
		received = append(received, parts...)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"uploadId":  session.ID,
		"path":      session.Path,
		"mode":      sessionMode(session),
		"size":      session.Size,
		"partSize":  session.PartSize,
		"expiresAt": session.ExpiresAt.UTC().Format(isoFormat),
		"status":    status,
		"received":  received,
	})
	return nil
}
