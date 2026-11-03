package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestP6RetiredRoutesNeverReachLegacyServicesOrSPA(t *testing.T) {
	paths := []string{"/api/fs", "/api/fs/list", "/api/fs/search", "/api/fs/download?path=secret", "/api/fs/mkdir", "/api/fs/rename", "/api/fs/rename/job", "/api/fs/delete", "/api/fs/deletions", "/api/fs/upload/sign", "/api/fs/upload/confirm", "/capabilities", "/list", "/search", "/mkdir", "/move", "/move/job", "/move/job/retry", "/copy", "/copy/job/cancel", "/delete", "/upload", "/upload/id", "/upload/complete"}
	for _, backend := range []string{"cos", "local", "s3"} {
		for _, coordinated := range []bool{false, true} {
			// Nil auth/provider services make any accidental legacy handler execution
			// visible; the SPA must never mask retired endpoints with a 200 page.
			s := &Server{cfg: &config.Config{Storage: config.Storage{Backend: backend, CoordinatedWrites: coordinated}}, web: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("retired route reached SPA") })}
			handler := s.Handler()
			for _, path := range paths {
				for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS"} {
					w := httptest.NewRecorder()
					r := httptest.NewRequest(method, path, nil)
					r.Header.Set("Authorization", "Bearer old-client")
					handler.ServeHTTP(w, r)
					if w.Code != 410 || w.Header().Get("Location") != "" || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("backend=%s coordinated=%v %s %s: HTTP%d headers=%v body=%s", backend, coordinated, method, path, w.Code, w.Header(), w.Body.String())
					}
					if method != "HEAD" && !strings.Contains(w.Body.String(), `"code":"legacy_file_api_retired"`) {
						t.Fatal("retired route did not return an explicit Problem", w.Body.String())
					}
				}
			}
		}
	}
}

func TestP6GeneratedPromptsUseOnlyUnifiedTransfers(t *testing.T) {
	for _, prompt := range []string{saveLinkPrompt("https://files.example", "save-secret"), tokenPrompt("https://files.example", "token-secret", &auth.Token{CanRead: true, CanWrite: true, Prefix: "Apps/ +%2F?# "})} {
		for _, obsolete := range []string{"/api/fs/", "https://files.example/upload", "https://files.example/list", "/api/files/Inbox", "returned part URL", "upload to COS"} {
			if strings.Contains(prompt, obsolete) {
				t.Fatalf("generated instructions advertise obsolete transfer %q", obsolete)
			}
		}
		for _, required := range []string{"/api/files/content?path=", "If-None-Match: *", "Idempotency-Key:", "/api/files/uploads", "/parts/{number}", "/commit", "/api/files/operations/", "Content-Length", "state is succeeded", "explicitly admitted", "maxSinglePutBytes=0", "use the session flow below for every file", "transport=cos-signed", "/parts/{number}/authorize", "/parts/{number}/confirm", "Do not add application Authorization", "client-sha256-cos-md5"} {
			if !strings.Contains(prompt, required) {
				t.Fatalf("generated instructions omit %q", required)
			}
		}
	}
	readOnly := tokenPrompt("https://files.example", "secret", &auth.Token{CanRead: true})
	if strings.Contains(readOnly, "curl -X PUT") || strings.Contains(readOnly, "POST https://files.example/api/files/uploads") {
		t.Fatal("read-only connection advertised a write example")
	}
}

func TestP6EventsHydrateAuthorizedContentAndKeepAuthoring(t *testing.T) {
	f := newFileWorkerFixture(t)
	f.cfg.FileCookieOrigins = []string{"https://files.example"}
	h := newUploadsHTTPFixture(t, f)
	// Use the same durable flow advertised by generated Save/Connect instructions.
	path := "/photo +%2F?#.png"
	id := h.newUpload(t, path, "p6-event-media", 4)
	uploadsHTTPJSON(t, h.part(id, 1, []byte("data"), nil), 201)
	h.commit(t, id)
	plain, _, err := f.auth.CreateToken(f.ctx, auth.NewTokenInput{UserID: h.root.OwnerID, Name: "events-only", Kind: auth.KindApp, CanEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"external_id": "p6-photo", "starts_at": "2026-11-01T00:00:00Z", "tz": "UTC", "title": "Photo", "type": "photo", "payload": map[string]any{"media": []string{strings.TrimPrefix(path, "/")}}})
	created := uploadsHTTPJSON(t, h.request("POST", "/events", plain, body, nil), 201)
	updated := uploadsHTTPJSON(t, h.request("POST", "/events", plain, body, nil), 200)
	if updated["id"] != created["id"] {
		t.Fatal("event idempotency changed identity")
	}
	listed := uploadsHTTPJSON(t, h.request("GET", "/events", plain, nil, nil), 200)
	if len(listed["events"].([]any)) != 1 {
		t.Fatalf("token event reconciliation lost authored event: %v", listed)
	}
	eventID := created["id"].(string)
	event := uploadsHTTPJSON(t, h.request("GET", "/events/"+eventID, plain, nil, nil), 200)
	media := event["payload"].(map[string]any)["media"].([]any)[0].(map[string]any)
	mediaURL := media["url"].(string)
	parsed, err := url.Parse(mediaURL)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.Path != "/api/files/content" || parsed.Query().Get("path") != path || parsed.Query().Get("disposition") != "inline" {
		t.Fatalf("event media escaped authorized content: %q %v", mediaURL, err)
	}
	for _, credential := range []string{h.bearer, "cookie"} {
		w := h.request("GET", mediaURL, credential, nil, nil)
		if w.Code != 200 || w.Body.String() != "data" || w.Header().Get("Location") != "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("same-origin media failed: HTTP%d headers%v body%s", w.Code, w.Header(), w.Body.String())
		}
	}
	uploadsHTTPProblem(t, h.request("GET", mediaURL, "", nil, nil), 401, "unauthenticated")
	uploadsHTTPProblem(t, h.request("GET", mediaURL, plain, nil, nil), 403, "forbidden")
	scoped, _ := f.token(t, h.root.OwnerID, "Other", true, false)
	uploadsHTTPProblem(t, h.request("GET", mediaURL, scoped, nil, nil), 404, "not_found")
	// Connect self-management and event detachment still work. Neither mutates bytes.
	uploadsHTTPJSON(t, h.request("GET", "/me", plain, nil, nil), 200)
	uploadsHTTPJSON(t, h.request("PATCH", "/me", plain, []byte(`{"name":"renamed app"}`), nil), 200)
	uploadsHTTPJSON(t, h.request("DELETE", "/events/"+eventID, plain, nil, nil), 200)
	if w := h.request("GET", mediaURL, h.bearer, nil, nil); w.Code != 200 || w.Body.String() != "data" {
		t.Fatal("event authoring changed native file bytes")
	}
}

func TestP6AdmissionConflictReturnsExactOperationWithoutGrantingRead(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	// A durable delete guard blocks descendants even before any byte ingress.
	op, _, err := f.journal.Begin(f.ctx, fileops.BeginRequest{Root: h.root, Actor: fileops.Actor{ID: "other-app", CanRead: true, CanWrite: true}, Kind: "delete", Intent: json.RawMessage(`{"target":"Blocked"}`), Guards: []fileops.Guard{{Path: storage.Path("Blocked"), Subtree: true}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []*httptest.ResponseRecorder{
		h.create("/Blocked", "p6-conflict", 4, h.bearer, nil),
		h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Blocked/child","parents":true}`), http.Header{"Idempotency-Key": {"p6-mkdir-conflict"}}),
	} {
		problem := uploadsHTTPJSON(t, w, 409)
		if problem["code"] != "operation_in_progress" || problem["operationId"] != op.ID {
			t.Fatalf("admission omitted exact blocking Operation: %v", problem)
		}
	}
	// Learning a blocker id must not allow another creator to inspect its manifest.
	uploadsHTTPProblem(t, h.request("GET", "/api/files/operations/"+op.ID, h.bearer, nil, nil), 403, "forbidden")
	owner := h.request("GET", "/api/files/operations/"+op.ID, "cookie", nil, nil)
	uploadsHTTPJSON(t, owner, 200)
	var count int
	if err = f.db.Pool().QueryRow(f.ctx, `select count(*) from file_uploads where owner_id=$1`, h.root.OwnerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("conflicting admission accepted bytes/session: %d %v", count, err)
	}
}
