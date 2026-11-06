package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/config"
)

type gatewayUnreadBody struct{ reads int }

func (b *gatewayUnreadBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*gatewayUnreadBody) Close() error               { return nil }

func TestTransferPrefixNeverReachesApplication(t *testing.T) {
	s := &Server{cfg: &config.Config{FileUploadIngress: "edgeone"}}
	body := &gatewayUnreadBody{}
	r := httptest.NewRequest("PUT", "/api/files/transfer/drive/owner/file?partNumber=1&uploadId=private", body)
	r.ContentLength = 1 << 40
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	uploadsHTTPProblem(t, w, 503, "edgeone_route_required")
	if body.reads != 0 {
		t.Fatal("application consumed misrouted upload bytes")
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })
		w := httptest.NewRecorder()
		s.fileIngressHandler(next).ServeHTTP(w, httptest.NewRequest(method, "/api/files/uploads/u_test", nil))
		if !called || w.Code != 204 {
			t.Fatal("application upload controls were disabled")
		}
	}
	for _, mode := range []string{"", "embedded"} {
		s.cfg.FileUploadIngress = mode
		called := false
		s.fileIngressHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("PUT", "/api/files/uploads/u_test/parts/1", nil))
		if !called {
			t.Fatal("embedded byte receiver was disabled")
		}
	}
}

func TestSignedTransferPrefixNeverFallsThroughToApplication(t *testing.T) {
	for _, mode := range []string{"embedded", "edgeone"} {
		s := &Server{cfg: &config.Config{FileUploadIngress: mode}}
		for _, method := range []string{"GET", "PUT", "POST", "DELETE"} {
			for _, path := range []string{"/api/files/transfer", "/api/files/transfer/file"} {
				body := &gatewayUnreadBody{}
				called := false
				next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })
				w := httptest.NewRecorder()
				s.fileIngressHandler(next).ServeHTTP(w, httptest.NewRequest(method, path, body))
				if called || body.reads != 0 || w.Code != 503 {
					t.Fatalf("%s %s reached application in %s", method, path, mode)
				}
			}
		}
	}
}
