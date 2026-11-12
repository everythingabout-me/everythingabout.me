package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/ratelimit"
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

func TestFileAPIBudgetIsSeparateFromTheAPIBudget(t *testing.T) {
	s := &Server{limiter: ratelimit.New()}
	r := httptest.NewRequest(http.MethodGet, "/api/files/list", nil)
	for i := 0; i < apiRatePerMinute; i++ {
		if err := s.limitAPI(httptest.NewRecorder(), r, "api", "", apiRatePerMinute); err != nil {
			t.Fatalf("request %d inside the API budget was refused: %v", i+1, err)
		}
	}
	if err := s.limitAPI(httptest.NewRecorder(), r, "api", "", apiRatePerMinute); err == nil {
		t.Fatal("the API budget admitted a request past its limit")
	}
	if err := s.limitAPI(httptest.NewRecorder(), r, "files", "u:owner-1", filesRatePerMinute); err != nil {
		t.Fatalf("the file API inherited the spent API budget: %v", err)
	}
	// An address is not a user: two owners behind one NAT must not share a budget.
	for i := 0; i < filesRatePerMinute; i++ {
		s.limitAPI(httptest.NewRecorder(), r, "files", "u:owner-1", filesRatePerMinute)
	}
	if err := s.limitAPI(httptest.NewRecorder(), r, "files", "u:owner-1", filesRatePerMinute); err == nil {
		t.Fatal("one owner's file budget was not closed")
	}
	if err := s.limitAPI(httptest.NewRecorder(), r, "files", "u:owner-2", filesRatePerMinute); err != nil {
		t.Fatalf("a second owner inherited the first owner's budget: %v", err)
	}
}

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
