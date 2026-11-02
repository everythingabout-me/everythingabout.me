package api

import (
	"net/http"
	"strings"

	"everythingabout.me/backend/internal/httpx"
)

// The legacy runtime is still COS-specific. Read-only P1 backends must not enter
// handlers that change the index before a provider operation or mint direct grants.
func (s *Server) assertBackendRoute(r *http.Request) error {
	if s.cfg.BackendName() == "cos" && !s.cfg.Storage.CoordinatedWrites {
		return nil
	}
	unsafe := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	blocked := false
	path := r.URL.Path
	if strings.HasPrefix(path, "/api/fs/") {
		blocked = unsafe || path == "/api/fs/download" || strings.HasPrefix(path, "/api/fs/rename/")
	}
	if strings.HasPrefix(path, "/api/files/") {
		blocked = true
	}
	for _, prefix := range []string{"/move", "/copy", "/upload", "/mkdir"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			blocked = true
		}
	}
	if path == "/api/timeline/rebuild" || path == "/capabilities" {
		blocked = true
	}
	if blocked {
		return httpx.NewError(http.StatusNotImplemented, "this file operation is unavailable for the read-only storage backend during migration")
	}
	return nil
}
