package filecontract_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
)

// This is a route-shape acceptance fixture, not an application router. The table
// mirrors api.md's resource table and has no storage/auth/service mocks. P2 can
// replace these marker handlers with production handlers and retain these cases.
func TestCanonicalRouteFamily(t *testing.T) {
	routes := []struct {
		method, suffix, target, workflowKey, workflowID string
	}{
		{"GET", "/me", "/me", "", ""},
		{"GET", "/capabilities", "/capabilities?path=%2FNotes", "", ""},
		{"GET", "", "?path=%2FNotes%2Fa", "", ""},
		{"GET", "/list", "/list?path=%2FNotes", "", ""},
		{"GET", "/search", "/search?q=a&path=%2FNotes", "", ""},
		{"GET", "/content", "/content?path=%2FNotes%2Fa", "", ""},
		{"HEAD", "/content", "/content?path=%2FNotes%2Fa", "", ""},
		{"PUT", "/content", "/content?path=%2FNotes%2Fa", "", ""},
		{"POST", "", "", "", ""},
		{"POST", "/move", "/move", "", ""},
		{"POST", "/copy", "/copy", "", ""},
		{"DELETE", "", "?path=%2FNotes%2Fa&recursive=true", "", ""},
		{"GET", "/thumbnail", "/thumbnail?path=%2FNotes%2Fa&width=320&format=webp", "", ""},
		{"POST", "/uploads", "/uploads", "", ""},
		{"GET", "/uploads", "/uploads", "", ""},
		{"GET", "/uploads/{uploadId}", "/uploads/u_fixture", "uploadId", "u_fixture"},
		{"PUT", "/uploads/{uploadId}/parts/{number}", "/uploads/u_fixture/parts/1", "uploadId", "u_fixture"},
		{"GET", "/uploads/{uploadId}/parts", "/uploads/u_fixture/parts?limit=200", "uploadId", "u_fixture"},
		{"POST", "/uploads/{uploadId}/commit", "/uploads/u_fixture/commit", "uploadId", "u_fixture"},
		{"DELETE", "/uploads/{uploadId}", "/uploads/u_fixture", "uploadId", "u_fixture"},
		{"GET", "/operations/{operationId}", "/operations/o_fixture", "operationId", "o_fixture"},
		{"POST", "/operations/{operationId}/cancel", "/operations/o_fixture/cancel", "operationId", "o_fixture"},
	}
	mux := http.NewServeMux()
	for _, route := range routes {
		pattern := route.method + " " + fc.Base + route.suffix
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Fixture-Pattern", pattern)
			if route.workflowKey != "" {
				w.Header().Set("X-Fixture-Workflow", r.PathValue(route.workflowKey))
			}
			if route.suffix == "/uploads/{uploadId}/parts/{number}" {
				w.Header().Set("X-Fixture-Part", r.PathValue("number"))
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	for _, route := range routes {
		t.Run(route.method+" "+route.target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(route.method, fc.Base+route.target, nil))
			if recorder.Code != 204 || recorder.Header().Get("X-Fixture-Pattern") != route.method+" "+fc.Base+route.suffix {
				t.Fatalf("route mismatch: status %d, pattern %q", recorder.Code, recorder.Header().Get("X-Fixture-Pattern"))
			}
			if recorder.Header().Get("X-Fixture-Workflow") != route.workflowID {
				t.Fatalf("workflow ID = %q; want %q", recorder.Header().Get("X-Fixture-Workflow"), route.workflowID)
			}
			if route.suffix == "/uploads/{uploadId}/parts/{number}" && recorder.Header().Get("X-Fixture-Part") != "1" {
				t.Fatal("part number not routed")
			}
		})
	}
	// File names can equal route resource names: they remain literal query values,
	// never route-segment file identities or competing wildcard paths.
	for _, path := range []string{"/list", "/content", "/uploads/u_fixture", "/operations/o_fixture", "/fileId", "/cafe\u0301%2F+.md"} {
		target := fc.Base + "?" + url.Values{"path": {path}}.Encode()
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest("GET", target, nil))
		if recorder.Code != 204 || recorder.Header().Get("X-Fixture-Pattern") != "GET /api/files" {
			t.Fatalf("query file %q did not reach metadata resource", path)
		}
	}
	for _, test := range []struct {
		method, target string
		status         int
	}{
		{"PATCH", "/api/files/content?path=%2Fa", 405},
		{"DELETE", "/api/files/move", 405},
		{"PUT", "/api/files/uploads/u_fixture/commit", 405},
		{"POST", "/api/files/operations/o_fixture", 405},
		{"GET", "/api/files/Notes/a", 404},
		{"GET", "/api/files/file_fixture", 404},
		{"GET", "/api/fs/list", 404},
		{"GET", "/api/web/files", 404},
		{"GET", "/api/files/v1/list", 404},
	} {
		t.Run(test.method+" "+test.target, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(test.method, test.target, nil))
			if recorder.Code != test.status {
				t.Fatalf("status = %d; want %d", recorder.Code, test.status)
			}
			if test.status == 405 && recorder.Header().Get("Allow") == "" {
				t.Fatal("method rejection missing Allow")
			}
		})
	}
}
