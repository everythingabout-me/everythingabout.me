package api

import (
	"bytes"
	"encoding/json"
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

func TestIsAPIPath(t *testing.T) {
	yes := []string{"/me", "/api/files", "/api/files/content", "/events", "/events/e_fixture", "/s/abcd2345efgh2345"}
	no := []string{"/", "/api/fs/list", "/files", "/filesx", "/api/filesx", "/login", "/capabilities", "/list", "/mkdir", "/move", "/copy", "/upload", "/upload/u_fixture", "/search"}

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
	prompt := saveLinkPrompt("https://everythingabout.me", "abcd2345efgh2345", "C")
	for _, want := range []string{
		"Token:    abcd2345efgh2345",
		"Granted:  read + write",
		"Scope:    / (the whole drive)",
		"$DRIVE/api/files/content?path=%2Fnote.md",
		"Content-Type: text/markdown",
		"If-None-Match: *",
		"Idempotency-Key:",
		"POST $DRIVE/api/files/uploads",
		"$DRIVE/api/files/uploads/<id>/parts/<number>",
		"$DRIVE/api/files/uploads/<id>/commit",
		"/api/files/operations/<operationId>",
		`"parents":true`,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "presigned-parts") {
		t.Errorf("class C prompt advertises class A:\n%s", prompt)
	}
	if strings.Contains(prompt, "## Events") {
		t.Errorf("save link advertises events it cannot author:\n%s", prompt)
	}
	if strings.Contains(prompt, "confirmation") {
		t.Errorf("prompt should not tell the agent what to confirm:\n%s", prompt)
	}
}

func TestPromptCapabilitiesBeforeWrite(t *testing.T) {
	prompt := tokenPrompt("https://files.example", "test-token", &auth.Token{CanRead: true, CanWrite: true, Prefix: "Apps/ +%2F?# "}, "C")
	check := "$DRIVE/api/files/capabilities?path=%2FApps%2F+%2B%252F%3F%23+"
	if !strings.Contains(prompt, check) {
		t.Fatalf("capabilities did not use the scope root: %s", prompt)
	}
	if strings.Index(prompt, check) > strings.Index(prompt, "## Write") {
		t.Fatal("capabilities check must precede the write section")
	}
	if strings.Count(prompt, "/api/files/capabilities?path=") != 1 {
		t.Fatal("prompt contains a competing capability target")
	}
}

func TestTokenPromptWithoutFileReads(t *testing.T) {
	writeOnly := tokenPrompt("https://files.example", "test-token", &auth.Token{CanWrite: true, Prefix: "Apps/writer"}, "C")
	for _, unwanted := range []string{"/api/files/me", "/api/files/capabilities", "## Read"} {
		if strings.Contains(writeOnly, unwanted) {
			t.Fatalf("write-only prompt advertised %q:\n%s", unwanted, writeOnly)
		}
	}
	for _, required := range []string{"This connection is write-only", "File introspection returns 403", "use a resumable", `"parents":true`} {
		if !strings.Contains(writeOnly, required) {
			t.Fatalf("write-only prompt omits %q:\n%s", required, writeOnly)
		}
	}

	eventsOnly := tokenPrompt("https://files.example", "test-token", &auth.Token{CanEvents: true}, "C")
	if strings.Contains(eventsOnly, "/api/files/") || strings.Contains(eventsOnly, "## Read") || strings.Contains(eventsOnly, "## Write") {
		t.Fatalf("events-only prompt advertised file requests:\n%s", eventsOnly)
	}
	for _, required := range []string{"## Events", "POST $DRIVE/events", "external_id"} {
		if !strings.Contains(eventsOnly, required) {
			t.Fatalf("events-only prompt omits %q:\n%s", required, eventsOnly)
		}
	}
}

func TestSaveLinkHTML(t *testing.T) {
	var buf bytes.Buffer
	copy := linkPageCopyFor("zh", true)
	err := linkTemplate.Execute(&buf, linkPageData(copy, saveLinkPrompt("https://everythingabout.me", "abcd2345efgh2345", "C"), "2026-10-27T23:00:00Z", nil))
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	body := buf.String()
	if !strings.Contains(body, "保存链接") || !strings.Contains(body, "abcd2345efgh2345") {
		t.Fatalf("unexpected html: %s", body)
	}
	if !strings.Contains(body, `<time datetime="2026-10-27T23:00:00Z">`) {
		t.Fatalf("timestamps should stay machine-readable: %s", body)
	}
	if strings.Contains(body, "<content>") {
		t.Fatal("prompt should be escaped in the html page")
	}
}

func TestLinkPageLanguage(t *testing.T) {
	for _, tc := range []struct {
		accept string
		lang   string
	}{
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh"},
		{"en-US,en;q=0.9", "en"},
		{"fr-FR,fr;q=0.9", "en"},
		{"", "en"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/s/x", nil)
		if tc.accept != "" {
			r.Header.Set("Accept-Language", tc.accept)
		}
		if got := linkPageLang(r); got != tc.lang {
			t.Errorf("Accept-Language %q: got %q want %q", tc.accept, got, tc.lang)
		}
	}
	if got := linkPageCopyFor("en", false).Title; got != "Connection instructions" {
		t.Fatalf("unexpected English title: %q", got)
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

func TestTokenPrompt(t *testing.T) {
	expires := "2026-10-29T13:45:00.000Z"
	scoped := &auth.Token{
		Prefix:    "Apps/obsidian",
		CanRead:   true,
		CanWrite:  true,
		CanEvents: true,
		ExpiresAt: &expires,
	}
	prompt := tokenPrompt("https://everythingabout.me", "eam_ab12cd34ef56_secret", scoped, "C")
	for _, want := range []string{
		"API:      https://everythingabout.me",
		"Token:    eam_ab12cd34ef56_secret",
		"Granted:  read + write + events",
		"Scope:    /Apps/obsidian",
		"Expires:  2026-10-29T13:45:00.000Z",
		"/api/files/list?path=%2FApps%2Fobsidian",
		"/api/files/content?path=%2FApps%2Fobsidian%2Fnote.md",
		"If `write` is false, stop and tell the owner",
		"## Events",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt is missing %q:\n%s", want, prompt)
		}
	}

	whole := tokenPrompt("https://everythingabout.me", "eam_plain", &auth.Token{CanRead: true}, "A")
	for _, want := range []string{"Granted:  read\n", "Scope:    / (the whole drive)", "Expires:  never", "$DRIVE/api/files/list?path=%2F", "409 `grant_required`"} {
		if !strings.Contains(whole, want) {
			t.Errorf("the whole-drive prompt is missing %q:\n%s", want, whole)
		}
	}
}
