package webui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// File detail routes are real visual paths and usually carry an extension, so they must get the
// SPA fallback rather than the "missing asset" 404.
func TestSPAFallbackServesFileDetailPaths(t *testing.T) {
	root := t.TempDir()
	index := filepath.Join(root, "index.html")
	if err := os.WriteFile(index, []byte("<!doctype html>spa"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	cases := []struct {
		path   string
		status int
		body   string
	}{
		{path: "/", status: http.StatusOK, body: "<!doctype html>spa"},
		{path: "/files", status: http.StatusOK, body: "<!doctype html>spa"},
		{path: "/files/Photos/2024/a.jpg", status: http.StatusOK, body: "<!doctype html>spa"},
		{path: "/files/%E7%BE%BD%E6%AF%9B%E7%90%83/a.pdf", status: http.StatusOK, body: "<!doctype html>spa"},
		{path: "/assets/app.js", status: http.StatusNotFound},
		{path: "/missing.png", status: http.StatusNotFound},
	}

	handler := New(root)
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
			if tc.body != "" && !strings.Contains(recorder.Body.String(), tc.body) {
				t.Fatalf("body = %q, want it to contain %q", recorder.Body.String(), tc.body)
			}
		})
	}
}
