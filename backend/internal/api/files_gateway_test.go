package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
)

type gatewayUnreadBody struct{ reads int }

func (b *gatewayUnreadBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*gatewayUnreadBody) Close() error               { return nil }

func TestExternalIngressApplicationNeverConsumesUploadBodies(t *testing.T) {
	s := &Server{cfg: &config.Config{FileUploadIngress: "external"}}
	for _, route := range []string{"/api/files/uploads/u_test/parts/1", "/api/files/content?path=%2Ffile"} {
		body := &gatewayUnreadBody{}
		r := httptest.NewRequest("PUT", route, body)
		r.ContentLength = 1 << 40
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		uploadsHTTPProblem(t, w, 503, "upload_gateway_required")
		if body.reads != 0 {
			t.Fatal("application consumed misrouted upload bytes")
		}
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

func TestUploadGatewayExposesNoControlAccountOrWebRoutes(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 404}, {"GET", "/api/health", 404}, {"GET", "/api/auth/me", 404},
		{"GET", "/api/files/uploads", 404}, {"POST", "/api/files/uploads", 404},
		{"POST", "/api/files/uploads/u_test/commit", 404}, {"DELETE", "/api/files/uploads/u_test", 404},
		{"GET", "/api/files/uploads/u_test/parts", 404}, {"PUT", "/api/files/content?path=%2Fa", 404},
		{"GET", "/api/files/uploads/u_test/parts/1", 405}, {"POST", "/api/files/uploads/u_test/parts/1", 405},
	} {
		body := &gatewayUnreadBody{}
		w := httptest.NewRecorder()
		s.UploadGatewayHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, body))
		if w.Code != tc.status || body.reads != 0 {
			t.Fatalf("%s %s: HTTP%d reads%d", tc.method, tc.path, w.Code, body.reads)
		}
		if tc.status == 405 && w.Header().Get("Allow") != "PUT" {
			t.Fatal("gateway advertised unrelated methods")
		}
	}
	if s.fileWorkers != nil {
		t.Fatal("gateway started application workers")
	}
}

func TestUploadGatewayUsesFullFileAuthenticationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, bearer, origin, csrf string
		status                     int
	}{
		{"cookie no origin", "", "", "", 403},
		{"cookie wrong csrf", "", "https://files.example", "wrong", 403},
		{"cookie approved", "", "https://files.example", "session-csrf", 501},
		{"bearer approved", "Bearer valid", "", "", 501},
		{"invalid bearer never falls back", "Bearer invalid", "https://files.example", "session-csrf", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authn := &boundaryAuth{principal: &auth.Principal{User: &auth.User{ID: "owner"}}}
			factory := &boundaryFactory{driver: &boundaryDriver{}}
			s := &Server{cfg: &config.Config{FileUploadIngress: "external", FileCookieOrigins: []string{"https://files.example"}}, filesAuth: authn, trees: factory}
			body := &gatewayUnreadBody{}
			r := httptest.NewRequest("PUT", "/api/files/uploads/u_test/parts/1", body)
			r.Header.Set("Cookie", "ambient=valid")
			if tc.bearer != "" {
				r.Header.Set("Authorization", tc.bearer)
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-CSRF-Token", tc.csrf)
			w := httptest.NewRecorder()
			s.UploadGatewayHandler().ServeHTTP(w, r)
			if w.Code != tc.status || body.reads != 0 {
				t.Fatalf("HTTP%d reads%d: %s", w.Code, body.reads, w.Body.String())
			}
			if tc.status == 401 && factory.calls != 0 {
				t.Fatal("invalid identity opened storage")
			}
		})
	}
}

// This test covers the split HTTP/auth boundary using disposable Local storage.
// Native object tests separately prove independent-host part metadata and commit.
func TestUploadGatewayPostgresSharedAuthenticationAndReceipts(t *testing.T) {
	f := newFileWorkerFixture(t)
	f.cfg.FileCookieOrigins = []string{"https://files.example"}
	h := newUploadsHTTPFixture(t, f)
	h.server.cfg.FileUploadIngress = "external"
	h.handler = h.server.Handler()
	capabilities := uploadsHTTPJSON(t, h.request("GET", "/api/files/capabilities?path=%2F", h.bearer, nil, nil), 200)
	if capabilities["maxSinglePutBytes"] != float64(0) || capabilities["uploads"] != true {
		t.Fatalf("external upload capabilities permit application bytes or disable sessions: %v", capabilities)
	}
	id := h.newUpload(t, "/gateway-file", "gateway-key", 4)
	request := func(handler http.Handler, credential string, headers http.Header, declared int64) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "https://files.example"+uploadRoute(id, "/parts/1"), bytes.NewReader([]byte("data"))).WithContext(f.ctx)
		r.ContentLength = declared
		r.Header = headers.Clone()
		if r.Header == nil {
			r.Header = make(http.Header)
		}
		if credential == "cookie" {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: h.cookie})
		} else if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	gateway := f.server()
	gatewayHandler := gateway.UploadGatewayHandler()
	uploadsHTTPProblem(t, request(h.handler, h.bearer, nil, 4), 503, "upload_gateway_required")
	uploadsHTTPProblem(t, request(gatewayHandler, "", nil, 4), 401, "unauthenticated")
	uploadsHTTPProblem(t, request(gatewayHandler, "cookie", nil, 4), 403, "forbidden")
	csrf := uploadsHTTPJSON(t, h.request("GET", "/api/files/me", "cookie", nil, nil), 200)["csrfToken"].(string)
	cookieHeaders := http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}}
	unrelated, _ := f.token(t, h.root.OwnerID, "", true, true)
	uploadsHTTPProblem(t, request(gatewayHandler, unrelated, nil, 4), 403, "forbidden")
	other, _ := f.owner(t)
	otherToken, _ := f.token(t, other.OwnerID, "", true, true)
	uploadsHTTPProblem(t, request(gatewayHandler, otherToken, nil, 4), 404, "not_found")
	uploadsHTTPProblem(t, request(gatewayHandler, h.bearer, nil, filesUploadPartSize+1), 400, "length_mismatch")
	first := uploadsHTTPJSON(t, request(gatewayHandler, h.bearer, nil, 4), 201)
	if first["size"] != float64(4) || first["sha256"] == "" {
		t.Fatal("gateway returned no durable receipt")
	}
	for _, secret := range []string{"Handle", "NativeRef", "Checkpoint"} {
		if _, ok := first[secret]; ok {
			t.Fatal("gateway exposed native identity")
		}
	}
	// A gateway cold start uses the shared DB receipt and can replay the same bytes.
	gateway = f.server()
	uploadsHTTPJSON(t, request(gateway.UploadGatewayHandler(), "cookie", cookieHeaders, 4), 200)
	status := uploadsHTTPJSON(t, h.request("GET", uploadRoute(id, ""), h.bearer, nil, nil), 200)
	if status["receivedParts"] != float64(1) || status["receivedBytes"] != float64(4) {
		t.Fatal("application did not observe gateway receipt")
	}
	if gateway.fileWorkers != nil {
		t.Fatal("part gateway started commit workers")
	}
	if err := f.ctx.Err(); err != nil {
		t.Fatal(err)
	}
	// There is no hidden server-side control interface on the gateway.
	w := httptest.NewRecorder()
	gateway.UploadGatewayHandler().ServeHTTP(w, httptest.NewRequest("POST", uploadRoute(id, "/commit"), nil).WithContext(context.Background()))
	var problem fc.Problem
	if json.Unmarshal(w.Body.Bytes(), &problem) != nil || problem.Status != 404 {
		t.Fatal("gateway exposed commit")
	}
}
