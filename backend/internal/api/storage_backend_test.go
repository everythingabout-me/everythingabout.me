package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func TestReadOnlyBackendRejectsLegacyEffectsBeforeServices(t *testing.T) {
	// Nil services make accidental handler execution fail visibly. This exercises
	// the actual router without a database, COS configuration or provider requests.
	for _, backend := range []string{"local", "s3"} {
		t.Run(backend, func(t *testing.T) {
			s := &Server{cfg: &config.Config{Storage: config.Storage{Backend: backend}}}
			handler := s.Handler()
			for _, tc := range []struct{ method, path string }{{"GET", "/capabilities"}, {"GET", "/api/fs/rename/m_1"}, {"HEAD", "/api/fs/rename/m_1"}, {"POST", "/api/fs/mkdir"}, {"POST", "/api/fs/upload/sign"}, {"POST", "/api/fs/upload/confirm"}, {"POST", "/api/fs/rename"}, {"POST", "/api/fs/delete"}, {"GET", "/api/fs/download?path=a"}, {"PUT", "/api/files/a"}, {"DELETE", "/api/files/a"}, {"GET", "/api/files/a"}, {"POST", "/upload"}, {"POST", "/upload/complete"}, {"POST", "/move"}, {"GET", "/move/m_1"}, {"POST", "/copy"}, {"POST", "/mkdir"}, {"POST", "/api/timeline/rebuild"}} {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
				want := http.StatusNotImplemented
				if strings.HasPrefix(tc.path, "/api/files/") {
					want = http.StatusNotFound
				}
				if recorder.Code != want {
					t.Fatalf("%s %s: status %d body=%s", tc.method, tc.path, recorder.Code, recorder.Body.String())
				}
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest("GET", "/api/health", nil))
			if recorder.Code != 200 {
				t.Fatal("local process health unavailable")
			}
		})
	}
}
func TestCOSLegacyRouteAdmissionUnchanged(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	for _, path := range []string{"/api/fs/mkdir", "/api/files/a", "/upload", "/copy", "/api/timeline/rebuild"} {
		if err := s.assertBackendRoute(httptest.NewRequest("POST", path, nil)); err != nil {
			t.Fatalf("COS route unexpectedly gated: %s %v", path, err)
		}
	}
}
