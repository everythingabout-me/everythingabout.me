package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

type boundaryAuth struct {
	calls     int
	principal *auth.Principal
}

func (a *boundaryAuth) AuthenticateFiles(_ context.Context, r *http.Request) (*auth.Principal, fc.CredentialSource, error) {
	a.calls++
	selected, err := fc.SelectCredential(r.Header)
	if err != nil {
		return nil, selected.Source, err
	}
	if selected.Source == fc.BearerCredential && selected.Token != "valid" {
		return nil, selected.Source, fc.NewProblem(401, "unauthenticated", "The token is invalid.")
	}
	return a.principal, selected.Source, nil
}
func (*boundaryAuth) FileCSRFToken(*http.Request) string { return "session-csrf" }
func (*boundaryAuth) ValidateFileCSRF(_ *http.Request, token string) bool {
	return token == "session-csrf"
}

type boundaryFactory struct {
	driver storage.Driver
	calls  int
}

func (f *boundaryFactory) Open(context.Context, string) (storage.Driver, error) {
	f.calls++
	return f.driver, nil
}

type boundaryDriver struct{ closed bool }

func (*boundaryDriver) Stat(context.Context, storage.Path) (storage.Entry, error) {
	return storage.Entry{Kind: storage.Folder}, nil
}
func (*boundaryDriver) List(context.Context, storage.Path, storage.PageRequest) (storage.Page, error) {
	return storage.Page{}, nil
}
func (*boundaryDriver) Open(context.Context, storage.Path, storage.ReadOptions) (storage.Read, error) {
	return storage.Read{}, storage.NewError("not_found", "", nil)
}
func (*boundaryDriver) Describe() storage.Descriptor { return storage.Descriptor{RangeRead: true} }
func (d *boundaryDriver) Close() error               { d.closed = true; return nil }
func TestFilesBoundaryAuthorizationAndCookieMutation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		auth         *string
		origin, csrf string
		want         int
	}{
		{"cookie missing origin", nil, "", "", 403},
		{"cookie missing csrf", nil, "https://files.example", "", 403},
		{"cookie cross origin", nil, "https://evil.example", "session-csrf", 403},
		{"cookie allowed", nil, "https://files.example", "session-csrf", 501},
		{"bearer valid", ptrString("Bearer valid"), "", "", 501},
		{"bearer invalid cookie ignored", ptrString("Bearer invalid"), "https://files.example", "session-csrf", 401},
		{"explicit empty", ptrString(""), "https://files.example", "session-csrf", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authn := &boundaryAuth{principal: &auth.Principal{User: &auth.User{ID: "owner"}}}
			driver := &boundaryDriver{}
			factory := &boundaryFactory{driver: driver}
			s := &Server{cfg: &config.Config{FileCookieOrigins: []string{"https://files.example"}}, filesAuth: authn, trees: factory}
			r := httptest.NewRequest("PUT", "/api/files/content?path=%2Fa", nil)
			r.Header.Set("Cookie", "ambient=valid")
			if tc.auth != nil {
				r.Header.Set("Authorization", *tc.auth)
			}
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.csrf != "" {
				r.Header.Set("X-CSRF-Token", tc.csrf)
			}
			w := httptest.NewRecorder()
			s.filesHandler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status%d body%s", w.Code, w.Body.String())
			}
			var p fc.Problem
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			if p.RequestID == "" || p.Status != tc.want || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("bad Problem %+v", p)
			}
			if tc.want == 501 && !driver.closed {
				t.Fatal("native root leaked")
			}
			if tc.want == 401 && factory.calls != 0 {
				t.Fatal("invalid auth opened native root")
			}
		})
	}
}
func ptrString(s string) *string { return &s }
func TestFilesCORSAuthAndCredentialsAreSeparated(t *testing.T) {
	s := &Server{cfg: &config.Config{FileCookieOrigins: []string{"https://files.example"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	for _, tc := range []struct {
		origin, headers    string
		want               int
		allow, credentials string
	}{
		{"https://third.example", "Authorization, Range, Content-Digest, Idempotency-Key", 204, "*", ""},
		{"https://files.example", "X-CSRF-Token, Content-Type", 204, "https://files.example", "true"},
		{"https://evil.example", "X-CSRF-Token", 403, "", ""},
		{"https://third.example", "Authorization, X-Unsupported", 403, "*", ""},
	} {
		r := httptest.NewRequest("OPTIONS", "/api/files/content", nil)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Access-Control-Request-Method", "PUT")
		r.Header.Set("Access-Control-Request-Headers", tc.headers)
		w := httptest.NewRecorder()
		s.filesCORS(next).ServeHTTP(w, r)
		if w.Code != tc.want || w.Header().Get("Access-Control-Allow-Origin") != tc.allow || w.Header().Get("Access-Control-Allow-Credentials") != tc.credentials {
			t.Fatalf("CORS status%d headers%v", w.Code, w.Header())
		}
		if tc.credentials == "true" && w.Header().Get("Access-Control-Allow-Origin") == "*" {
			t.Fatal("credentialed wildcard")
		}
	}
}
func TestFilesRouterRetiresWildcardAndUsesProblems(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/api/files/Notes/a", 404}, {"GET", "/api/files/file_id", 404}, {"PATCH", "/api/files/content?path=%2Fa", 405}, {"POST", "/api/files/operations/o_id", 405}} {
		w := httptest.NewRecorder()
		s.filesHandler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s: %d", tc.path, w.Code)
		}
		var p fc.Problem
		if json.Unmarshal(w.Body.Bytes(), &p) != nil || p.RequestID == "" {
			t.Fatal("missing problem identity")
		}
		if tc.want == 405 && w.Header().Get("Allow") == "" {
			t.Fatal("missing Allow")
		}
	}
}
func TestFilesBoundaryHasNoWallClockAndStartedResponse(t *testing.T) {
	// A request must not carry a deadline of this service's own choosing: the client's
	// disconnect cancels it, and the work itself is bounded by pages, entries and provider
	// budgets. A clock here is how a 776-folder delete and a 23k-file import both died.
	for _, method := range []string{"GET", "HEAD", "POST", "DELETE"} {
		driver := &boundaryDriver{}
		s := &Server{cfg: &config.Config{}, filesAuth: &boundaryAuth{principal: &auth.Principal{User: &auth.User{ID: "owner"}}}, trees: &boundaryFactory{driver: driver}}
		w := httptest.NewRecorder()
		s.fileWrap(func(w http.ResponseWriter, r *http.Request, _ *fileRequest) error {
			if deadline, ok := r.Context().Deadline(); ok {
				t.Fatalf("%s carries a wall clock that ends at %s", r.URL.Path, deadline)
			}
			return writeFilesJSON(w, 200, map[string]bool{"read": true})
		})(w, func() *http.Request {
			r := httptest.NewRequest(method, "/api/files/search?q=a", nil)
			r.Header.Set("Authorization", "Bearer valid")
			return r
		}())
		if w.Code != 200 || !driver.closed {
			t.Fatal("request did not settle")
		}
	}
	s := &Server{cfg: &config.Config{}, filesAuth: &boundaryAuth{principal: &auth.Principal{User: &auth.User{ID: "owner"}}}, trees: &boundaryFactory{driver: &boundaryDriver{}}}
	w := httptest.NewRecorder()
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("started response was not aborted: %v", recovered)
		}
		if w.Body.String() != "file-bytes" {
			t.Fatalf("error appended to response: %s", w.Body.String())
		}
	}()
	s.fileWrap(func(w http.ResponseWriter, _ *http.Request, _ *fileRequest) error {
		_, _ = io.WriteString(w, "file-bytes")
		return io.ErrUnexpectedEOF
	})(w, httptest.NewRequest("GET", "/api/files/content?path=%2Fa", nil))
}

func TestStatusRecorderFlushAndUnwrap(t *testing.T) {
	w := httptest.NewRecorder()
	r := &statusRecorder{ResponseWriter: w}
	r.Flush()
	if !w.Flushed || r.status != 200 {
		t.Fatal("flushing lost")
	}
	if r.Unwrap() != w {
		t.Fatal("writer unwrap lost")
	}
	r.WriteHeader(206)
	if r.status != 200 {
		t.Fatal("second header overwrote first")
	}
	_, _ = io.WriteString(r, "bytes")
	if w.Body.String() != "bytes" {
		t.Fatal("writer did not forward bytes")
	}
}
