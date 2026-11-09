package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/sizeformat"
	"everythingabout.me/backend/internal/storage"
)

const (
	apiDefaultLimit = 1000
	apiMaxLimit     = 5000
	// limit for a single PUT; bigger files go through POST /upload for presigned URLs.
	apiMaxPutSize = 32 << 20
)

// apiNode is the file/directory shape of the public API.
type apiNode struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	Parent     string  `json:"parent"`
	Type       string  `json:"type"`
	Size       int64   `json:"size"`
	Mime       *string `json:"mime,omitempty"`
	ETag       *string `json:"etag,omitempty"`
	SHA256     string  `json:"sha256,omitempty"`
	CreatedAt  string  `json:"createdAt"`
	ModifiedAt string  `json:"modifiedAt"`
}

func toAPINode(row *nodes.Row) apiNode {
	kind := "file"
	if row.IsDir {
		kind = "dir"
	}
	node := apiNode{
		Name:       row.Name,
		Path:       row.Path,
		Parent:     row.Parent,
		Type:       kind,
		Size:       row.Size,
		Mime:       row.Mime,
		ETag:       row.ETag,
		CreatedAt:  row.CreatedAt,
		ModifiedAt: row.UpdatedAt,
	}
	if len(row.Meta) > 0 {
		var meta struct {
			SHA256 string `json:"sha256"`
		}
		if json.Unmarshal(row.Meta, &meta) == nil {
			node.SHA256 = meta.SHA256
		}
	}
	return node
}

func toAPINodes(rows []nodes.Row) []apiNode {
	out := make([]apiNode, 0, len(rows))
	for i := range rows {
		out = append(out, toAPINode(&rows[i]))
	}
	return out
}

// ---- errors ----

func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusPreconditionFailed:
		return "precondition_failed"
	case http.StatusLengthRequired:
		return "length_required"
	case http.StatusGone:
		return "gone"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusInsufficientStorage:
		return "quota_exceeded"
	default:
		return "internal"
	}
}

// writeAPIError writes the public API error body: {"error":"code","message":"..."}.
func writeAPIError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal"
	message := "internal server error"

	var apiErr *httpx.Error
	var pathErr *paths.Error
	switch {
	case errors.As(err, &apiErr):
		status = apiErr.Status
		code = codeForStatus(status)
		message = apiErr.Message
	case errors.As(err, &pathErr):
		status = http.StatusBadRequest
		code = "invalid_path"
		message = pathErr.Message
	default:
		slog.Error("api", "error", err)
		message = err.Error()
	}

	httpx.WriteJSON(w, status, map[string]string{"error": code, "message": message})
}

// ---- pagination cursors ----

type cursorPayload struct {
	IsDir bool   `json:"d"`
	Name  string `json:"n"`
	ID    string `json:"i"`
}

func encodeCursor(row *nodes.Row) string {
	raw, _ := json.Marshal(cursorPayload{IsDir: row.IsDir, Name: row.Name, ID: row.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(raw string) (*nodes.Cursor, error) {
	if raw == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, httpx.BadRequest("invalid cursor")
	}
	var payload cursorPayload
	if err := json.Unmarshal(data, &payload); err != nil || payload.ID == "" {
		return nil, httpx.BadRequest("invalid cursor")
	}
	return &nodes.Cursor{IsDir: payload.IsDir, Name: payload.Name, ID: payload.ID}, nil
}

func parseLimit(raw string) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return apiDefaultLimit
	}
	if value > apiMaxLimit {
		return apiMaxLimit
	}
	return value
}

// principal resolves a Bearer token or the cookie session.
func (s *Server) principal(r *http.Request) (*auth.Principal, error) {
	return s.auth.Authenticate(r.Context(), r)
}

// ---- GET /list ----

func (s *Server) handleAPIList(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	path, err := paths.Normalize(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	if err := p.RequireRead(path); err != nil {
		return err
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	limit := parseLimit(r.URL.Query().Get("limit"))
	// fetch one extra row to know whether there is a next page.
	items, err := s.nodes.ListPage(r.Context(), p.User.ID, path, limit+1, cursor)
	if err != nil {
		return err
	}

	var next *string
	if len(items) > limit {
		items = items[:limit]
		if len(items) > 0 {
			encoded := encodeCursor(&items[len(items)-1])
			next = &encoded
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"path":     path,
		"writable": s.cfg.BackendName() == "cos" && !s.cfg.Storage.CoordinatedWrites && p.CanWrite(path),
		"items":    toAPINodes(items),
		"cursor":   next,
	})
	return nil
}

// ---- GET /me ----

func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	bytes, files, err := s.nodes.Usage(r.Context(), p.User.ID)
	if err != nil {
		return err
	}

	var token any
	if p.Token != nil {
		token = map[string]any{
			"name":          p.Token.Name,
			"prefix":        p.Token.Prefix,
			"read":          p.Token.CanRead,
			"write":         p.Token.CanWrite,
			"events":        p.Token.CanEvents,
			"changes":       p.Token.CanChanges,
			"subscribe":     p.Token.CanSubscribe,
			"policyVersion": p.Token.PolicyVersion,
			"expiresAt":     p.Token.ExpiresAt,
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user":  map[string]any{"email": p.User.Email},
		"token": token,
		"usage": map[string]any{"bytes": bytes, "files": files},
	})
	return nil
}

// ---- /api/files/<path> ----

// filePath extracts and normalizes the path from the URL.
func filePath(r *http.Request) (string, error) {
	path, err := paths.Normalize(r.PathValue("path"))
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", httpx.BadRequest("path cannot be empty")
	}
	return path, nil
}

func (s *Server) handleAPIGetFile(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	path, err := filePath(r)
	if err != nil {
		return err
	}
	if err := p.RequireRead(path); err != nil {
		return err
	}

	node, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if node == nil {
		return httpx.NotFound("file not found")
	}
	if node.IsDir {
		return httpx.BadRequest("this is a directory; use /list")
	}

	query := r.URL.Query()
	if query.Get("meta") == "1" {
		// two entry points for one thing is the easiest fat an API grows; this one is cut.
		return httpx.BadRequest("?meta=1 was removed; use HEAD /api/files/<path> for metadata")
	}

	url, err := s.storage.GetURL(r.Context(), node.Key, storage.URLOptions{
		DownloadName: node.Name,
		ContentType:  deref(node.Mime),
	})
	if err != nil {
		return err
	}
	if query.Get("json") == "1" {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"url":  url,
			"name": node.Name,
			"size": node.Size,
			"mime": node.Mime,
		})
		return nil
	}
	http.Redirect(w, r, url, http.StatusFound)
	return nil
}

func (s *Server) handleAPIHeadFile(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	path, err := filePath(r)
	if err != nil {
		return err
	}
	if err := p.RequireRead(path); err != nil {
		return err
	}

	node, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if node == nil {
		return httpx.NotFound("file not found")
	}

	w.Header().Set("Content-Length", strconv.FormatInt(node.Size, 10))
	if node.Mime != nil && *node.Mime != "" {
		w.Header().Set("Content-Type", *node.Mime)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if node.ETag != nil && *node.ETag != "" {
		w.Header().Set("ETag", `"`+*node.ETag+`"`)
	}
	if modified, err := time.Parse(time.RFC3339, node.UpdatedAt); err == nil {
		w.Header().Set("Last-Modified", modified.UTC().Format(http.TimeFormat))
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) handleAPIPutFile(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	path, err := filePath(r)
	if err != nil {
		return err
	}
	if err := p.RequireWrite(path); err != nil {
		return err
	}

	size := r.ContentLength
	if size < 0 {
		return httpx.NewError(http.StatusLengthRequired, "PUT requires Content-Length; use POST /upload for large files")
	}
	if size > apiMaxPutSize {
		return httpx.NewError(http.StatusRequestEntityTooLarge,
			"exceeds 32MiB; use POST /upload to get a presigned URL and upload to COS directly")
	}

	existing, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.IsDir {
		return httpx.Conflict("a directory with the same name already exists")
	}

	// no overwrite by default: either an explicit ?overwrite=1 or a matching If-Match.
	overwrite := r.URL.Query().Get("overwrite") == "1"
	match := strings.TrimSpace(r.Header.Get("If-Match"))
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == "*" && existing != nil {
		return httpx.NewError(http.StatusPreconditionFailed, "file already exists")
	}
	if match != "" {
		want := strings.Trim(match, `"`)
		if existing == nil || existing.ETag == nil || *existing.ETag != want {
			return httpx.NewError(http.StatusPreconditionFailed, "file changed; ETag mismatch")
		}
	} else if existing != nil && !overwrite {
		return httpx.Conflict("file already exists; add ?overwrite=1 (or send If-Match) to overwrite")
	}

	if err := s.checkQuota(r.Context(), p.User.ID, size, existing); err != nil {
		return err
	}

	mime := r.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/octet-stream"
	}

	if err := s.assertParentDirs(r.Context(), p.User.ID, path); err != nil {
		return err
	}
	if err := s.nodes.EnsureParents(r.Context(), path, p.User.ID); err != nil {
		return err
	}

	info, err := s.storage.Put(r.Context(), paths.KeyFor(path, p.User.ID), r.Body, size, mime)
	if err != nil {
		return err
	}

	var etag, storageClass *string
	if info.ETag != "" {
		etag = &info.ETag
	}
	if info.StorageClass != "" {
		storageClass = &info.StorageClass
	}

	var modified *time.Time
	if !info.LastModified.IsZero() {
		modified = &info.LastModified
	}

	node, err := s.nodes.UpsertFile(r.Context(), nodes.UpsertInput{
		OwnerID:          p.User.ID,
		Path:             path,
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

	status := http.StatusOK
	if existing == nil {
		status = http.StatusCreated
	}
	if node.ETag != nil {
		w.Header().Set("ETag", `"`+*node.ETag+`"`)
	}
	s.recordAudit(r, p, "put", path, status, map[string]any{"size": info.Size})
	httpx.WriteJSON(w, status, toAPINode(node))
	return nil
}

func (s *Server) handleAPIDeleteFile(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	path, err := filePath(r)
	if err != nil {
		return err
	}
	if err := nodes.AssertNotRoot(path); err != nil {
		return err
	}
	if err := p.RequireWrite(path); err != nil {
		return err
	}

	node, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if node == nil {
		return httpx.NotFound("file not found")
	}
	if node.IsDir && r.URL.Query().Get("recursive") != "1" {
		return httpx.Conflict("deleting a directory requires ?recursive=1")
	}

	count, err := s.nodes.Purge(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if count == 0 {
		return httpx.NotFound("nothing to delete")
	}
	s.timeline.Notify(p.User.ID)
	s.recordAudit(r, p, "delete", path, http.StatusNoContent, map[string]any{
		"count":     count,
		"recursive": r.URL.Query().Get("recursive") == "1",
	})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- POST /move ----

type apiMoveBody struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) handleAPIMove(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiMoveBody](r)
	if err != nil {
		return err
	}
	from, to, err := sourceAndTarget(body.From, body.To, p)
	if err != nil {
		return err
	}
	unlock, err := s.lockBrowserMutation(r.Context(), p.User.ID, from, to)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.assertParentDirs(r.Context(), p.User.ID, to); err != nil {
		return err
	}
	pref := parsePrefer(r.Header.Get("Prefer"))

	source, err := s.nodes.Live(r.Context(), from, p.User.ID)
	if err != nil {
		return err
	}
	if source == nil {
		return httpx.NotFound("source file not found")
	}
	target, err := s.nodes.Live(r.Context(), to, p.User.ID)
	if err != nil {
		return err
	}

	if source.IsDir {
		if target != nil {
			return httpx.Conflict("target path already exists")
		}
		job, err := s.runJob(r, jobs.OpMove, p.User.ID, from, to, pref)
		if err != nil {
			return err
		}
		s.recordAudit(r, p, "move", from, auditStatus(job), map[string]any{"to": to, "job": job.ID})
		writeJobResult(w, "move", job)
		return nil
	}

	if target != nil {
		if !body.Overwrite {
			return httpx.Conflict("target path already exists")
		}
		if target.IsDir {
			return httpx.Conflict("target is a directory")
		}
		// overwrite means the target is purged outright; there is no trash.
		if _, err := s.nodes.Purge(r.Context(), to, p.User.ID); err != nil {
			return err
		}
	}

	if err := s.nodes.EnsureParents(r.Context(), to, p.User.ID); err != nil {
		return err
	}

	targetKey := paths.KeyFor(to, p.User.ID)
	if err := s.storage.Copy(r.Context(), source.Key, targetKey); err != nil {
		return err
	}
	// copy first, delete the source after: if the copy fails, the source is still there.
	if err := s.storage.Remove(r.Context(), source.Key); err != nil {
		slog.Warn("move: failed to delete source object", "key", source.Key, "error", err)
	}

	node, err := s.nodes.Move(r.Context(), source.ID, to, p.User.ID)
	if err != nil {
		return err
	}
	s.recordAudit(r, p, "move", from, http.StatusOK, map[string]any{"to": to})
	httpx.WriteJSON(w, http.StatusOK, toAPINode(node))
	return nil
}

// checkQuota reports whether a write would exceed the quota. existing is the file about to be
// overwritten (pass nil if none).
func (s *Server) checkQuota(ctx context.Context, userID string, incoming int64, existing *nodes.Row) error {
	quota := s.cfg.QuotaBytes
	if quota <= 0 {
		return nil
	}
	used, _, err := s.nodes.Usage(ctx, userID)
	if err != nil {
		return err
	}
	var old int64
	if existing != nil && !existing.IsDir {
		old = existing.Size
	}
	total := used - old + incoming
	if total > quota {
		return httpx.NewError(http.StatusInsufficientStorage,
			fmt.Sprintf("not enough space: need %s, quota is %s", sizeformat.Bytes(total), sizeformat.Bytes(quota)))
	}
	return nil
}
