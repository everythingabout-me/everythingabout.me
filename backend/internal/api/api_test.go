package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
)

// checks a few things about /api/files/{path...}: slashes kept, %20 decoded, HEAD routed before GET.
func TestFilesRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "GET:%s", r.PathValue("path"))
	})
	mux.HandleFunc("HEAD /api/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Handler", "head")
		w.WriteHeader(http.StatusOK)
	})

	cases := []struct {
		name       string
		method     string
		target     string
		wantBody   string
		wantHeader string
	}{
		{"multi-level path", http.MethodGet, "/api/files/Photos/2024/a.jpg", "GET:Photos/2024/a.jpg", ""},
		{"percent-encoded", http.MethodGet, "/api/files/A%20B.txt", "GET:A B.txt", ""},
		{"non-ASCII path", http.MethodGet, "/api/files/%E7%BE%BD%E6%AF%9B%E7%90%83/a.mp4", "GET:羽毛球/a.mp4", ""},
		{"HEAD has its own handler", http.MethodHead, "/api/files/a.jpg", "", "head"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.target, nil)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)

			if tc.wantHeader != "" {
				if got := recorder.Header().Get("X-Handler"); got != tc.wantHeader {
					t.Fatalf("handler = %q, want %q", got, tc.wantHeader)
				}
				return
			}
			if got := recorder.Body.String(); got != tc.wantBody {
				t.Fatalf("body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

func TestIsAPIPath(t *testing.T) {
	yes := []string{
		"/capabilities", "/list", "/me", "/mkdir", "/move", "/copy", "/search",
		"/api/files", "/api/files/a/b", "/upload", "/upload/complete", "/upload/u_1",
		"/move/m_1", "/copy/c_1", "/copy/c_1/cancel", "/s/abcd2345efgh2345",
	}
	no := []string{"/", "/api/fs/list", "/files", "/filesx", "/api/filesx", "/login", "/capabilitiesx"}

	for _, path := range yes {
		if !isAPIPath(path) {
			t.Errorf("%s should be treated as an API path", path)
		}
	}
	for _, path := range no {
		if isAPIPath(path) {
			t.Errorf("%s should not be treated as an API path", path)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	row := nodes.Row{Node: nodes.Node{ID: "7f3a9c2b-0000-0000-0000-000000000000", Name: "a.txt"}}
	encoded := encodeCursor(&row)
	decoded, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if decoded.ID != row.ID || decoded.IsDir != row.IsDir || decoded.Name != row.Name {
		t.Fatalf("round trip mismatch: %+v", decoded)
	}

	if _, err := decodeCursor("not-base64!!"); err == nil {
		t.Fatal("invalid cursor should return an error")
	}
	if cursor, err := decodeCursor(""); err != nil || cursor != nil {
		t.Fatal("empty cursor should return (nil, nil)")
	}
}

func TestNewUploadID(t *testing.T) {
	id, err := newUploadID()
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if len(id) != 2+24 || id[:2] != "u_" {
		t.Fatalf("unexpected shape: %q", id)
	}
	other, err := newUploadID()
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if id == other {
		t.Fatal("two generated uploadIds are identical")
	}
}

func TestScopePrefix(t *testing.T) {
	limited := &auth.Principal{Token: &auth.Token{Prefix: "Apps/x", CanRead: true, CanWrite: true}}
	full := &auth.Principal{}

	cases := []struct {
		name      string
		principal *auth.Principal
		request   string
		want      string
		wantErr   bool
	}{
		{"token without path uses its own prefix", limited, "", "Apps/x", false},
		{"token path inside the prefix", limited, "Apps/x/sub", "Apps/x/sub", false},
		{"token path outside the prefix", limited, "Apps/y", "", true},
		{"full access, empty path", full, "", "", false},
		{"full access, arbitrary path", full, "any/path", "any/path", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scopePrefix(tc.principal, tc.request)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParsePrefer(t *testing.T) {
	cases := []struct {
		name      string
		header    string
		requested bool
		wait      time.Duration
	}{
		{"no header", "", false, 0},
		{"async only", "respond-async", true, 0},
		{"comma separated wait", "respond-async, wait=10", true, 10 * time.Second},
		{"semicolon separated wait", "respond-async; wait=3", true, 3 * time.Second},
		{"wait 0", "respond-async,wait=0", true, 0},
		{"wait above the cap", "respond-async, wait=999", true, maxAsyncWait},
		{"wait is not a number", "respond-async, wait=abc", true, 0},
		{"unknown preference", "return=minimal", false, 0},
		{"case insensitive", "Respond-Async, Wait=5", true, 5 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePrefer(tc.header)
			if got.Requested != tc.requested || got.Wait != tc.wait {
				t.Fatalf("parsePrefer(%q) = %+v, want requested=%v wait=%v",
					tc.header, got, tc.requested, tc.wait)
			}
		})
	}
}

func TestPreferBudget(t *testing.T) {
	if got := (preferAsync{}).Budget(); got != defaultSyncBudget {
		t.Fatalf("without Prefer it should wait %v, got %v", defaultSyncBudget, got)
	}
	if got := (preferAsync{Requested: true}).Budget(); got != 0 {
		t.Fatalf("respond-async without wait should return 202 immediately, waited %v", got)
	}
	if got := (preferAsync{Requested: true, Wait: 5 * time.Second}).Budget(); got != 5*time.Second {
		t.Fatalf("wait=5 should wait 5s, got %v", got)
	}
}

func TestSourceAndTarget(t *testing.T) {
	full := &auth.Principal{}
	scoped := &auth.Principal{Token: &auth.Token{Prefix: "Apps/x", CanRead: true, CanWrite: true}}

	cases := []struct {
		name    string
		p       *auth.Principal
		from    string
		to      string
		wantErr bool
	}{
		{"normal", full, "a.md", "b/a.md", false},
		{"root is rejected", full, "", "b", true},
		{"path unchanged", full, "a.md", "a.md", true},
		{"target inside source", full, "Docs", "Docs/sub", true},
		{"outside token scope", scoped, "Apps/y/a.md", "Apps/y/b.md", true},
		{"inside token scope", scoped, "Apps/x/a.md", "Apps/x/b.md", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := sourceAndTarget(tc.from, tc.to, tc.p)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	s := &Server{cfg: &config.Config{
		QuotaBytes:         1024,
		MultipartThreshold: 5 << 30,
		UploadPartSize:     64 << 20,
	}}
	recorder := httptest.NewRecorder()
	if err := s.handleAPICapabilities(recorder, httptest.NewRequest(http.MethodGet, "/capabilities", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	for _, key := range []string{
		"maxPutBytes", "maxUploadBytes", "multipartThreshold", "partSize",
		"maxParts", "list", "rateLimit", "quotaBytes",
	} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing field %s", key)
		}
	}
	if got["quotaBytes"].(float64) != 1024 {
		t.Errorf("quotaBytes = %v", got["quotaBytes"])
	}
	if async, ok := got["async"].(map[string]any); !ok || async["value"] != "respond-async" {
		t.Errorf("unexpected async field: %v", got["async"])
	}
}

func TestWriteJobResult(t *testing.T) {
	cases := []struct {
		name     string
		job      *jobs.Job
		status   int
		location bool
	}{
		{"still running", &jobs.Job{ID: "m_1", Status: "running"}, http.StatusAccepted, true},
		{"done", &jobs.Job{ID: "m_1", Status: "done"}, http.StatusOK, true},
		{"cancelled", &jobs.Job{ID: "m_1", Status: "cancelled"}, http.StatusOK, true},
		{"failed", &jobs.Job{ID: "m_1", Status: "failed", Error: "boom"}, http.StatusInternalServerError, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeJobResult(recorder, "move", tc.job)

			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.status)
			}
			location := recorder.Header().Get("Location")
			if tc.location && location != "/move/m_1" {
				t.Fatalf("Location = %q", location)
			}
			if tc.status == http.StatusAccepted && recorder.Header().Get("Retry-After") == "" {
				t.Fatal("202 should include Retry-After")
			}
		})
	}
}

// ServeMux panics on a route conflict inside Handler; this test is the gate for that.
func TestHandlerRegistersRoutes(t *testing.T) {
	if handler := (&Server{}).Handler(); handler == nil {
		t.Fatal("Handler returned nil")
	}
}

func TestSaveLinkPrompt(t *testing.T) {
	prompt := saveLinkPrompt("https://everythingabout.me", "abcd2345efgh2345")
	for _, want := range []string{
		"Token: abcd2345efgh2345",
		"PUT https://everythingabout.me/api/files/Inbox/2026/10/28/note.md",
		"Content-Type: text/markdown",
		"POST https://everythingabout.me/upload",
		"Authorization: Bearer abcd2345efgh2345",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "POST https://everythingabout.me/s/") {
		t.Errorf("the link page must not be a write endpoint:\n%s", prompt)
	}
	if strings.Contains(prompt, "confirmation") {
		t.Errorf("prompt should not tell the agent what to confirm:\n%s", prompt)
	}
}

func TestSaveLinkHTML(t *testing.T) {
	var buf bytes.Buffer
	err := saveLinkTemplate.Execute(&buf, map[string]any{
		"Code":      "abcd2345efgh2345",
		"URL":       "https://everythingabout.me/s/abcd2345efgh2345",
		"Prompt":    saveLinkPrompt("https://everythingabout.me", "abcd2345efgh2345"),
		"LastUsed":  nil,
		"CreatedAt": "2026-10-27T23:00:00Z",
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "保存链接") || !strings.Contains(body, "abcd2345efgh2345") {
		t.Fatalf("unexpected html: %s", body)
	}
	if strings.Contains(body, "<content>") {
		t.Fatal("prompt should be escaped in the html page")
	}
}

func TestSaveLinkPostIsRejected(t *testing.T) {
	handler := (&Server{}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/s/abcd2345efgh2345", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestConnectPayload(t *testing.T) {
	token := &auth.Token{
		ID:       "ab12cd34ef56",
		Name:     "Obsidian",
		Kind:     auth.KindApp,
		Prefix:   "Apps/obsidian",
		CanRead:  true,
		CanWrite: true,
	}
	payload, err := buildConnectPayload("https://everythingabout.me", token, "eam_ab12cd34ef56_secret")
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	if decoded["v"] != float64(1) || decoded["type"] != "eam.connect" {
		t.Fatalf("unexpected envelope: %s", payload)
	}
	if decoded["api"] != "https://everythingabout.me" || decoded["token"] != "eam_ab12cd34ef56_secret" {
		t.Fatalf("api/token missing: %s", payload)
	}
	if decoded["id"] != "ab12cd34ef56" {
		t.Fatalf("id missing: %s", payload)
	}
	// The page can change these after the QR exists, so they come from GET /me instead of
	// being frozen into the code.
	for _, key := range []string{"name", "scope", "read", "write", "events", "expiresAt"} {
		if _, ok := decoded[key]; ok {
			t.Errorf("the QR must not carry %q; it can change after the QR is drawn", key)
		}
	}
	if len(decoded) != 5 {
		t.Fatalf("unexpected payload shape: %s", payload)
	}
	if strings.Contains(payload, "user@example.com") {
		t.Fatal("the payload must not carry account identity")
	}
}
