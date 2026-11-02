package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
)

func TestBrowserDeletionProgressRequiresCookie(t *testing.T) {
	cfg := &config.Config{AppSecret: "delete-progress-test"}
	server := &Server{cfg: cfg, auth: auth.New(nil, cfg)}
	for _, path := range []string{"/api/fs/deletions", "/api/fs/delete/d_example"} {
		for _, bearer := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, "https://files.example"+path, nil)
			if bearer {
				r.Header.Set("Authorization", "Bearer eam_live_test")
			}
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"ok":false`) {
				t.Fatalf("GET %s bearer=%v: HTTP=%d body=%s; want cookie authentication error", path, bearer, w.Code, w.Body.String())
			}
		}
	}
}
