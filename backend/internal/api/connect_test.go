package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
)

func TestConnectPostgresExplicitGrants(t *testing.T) {
	f := newFileWorkerFixture(t)
	root, _ := f.owner(t)
	other, _ := f.owner(t)
	cookie, _, err := f.auth.CreateSession(f.ctx, root.OwnerID, "connect test", "")
	if err != nil {
		t.Fatal(err)
	}
	otherCookie, _, err := f.auth.CreateSession(f.ctx, other.OwnerID, "other connect test", "")
	if err != nil {
		t.Fatal(err)
	}
	handler := f.server().Handler()
	request := func(method, target, body, session, bearer, origin string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "https://files.example"+target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if session != "" {
			req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	object := func(rec *httptest.ResponseRecorder, status int) map[string]any {
		t.Helper()
		if rec.Code != status {
			t.Fatalf("HTTP %d want %d: %s", rec.Code, status, rec.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	tokenFrom := func(rec *httptest.ResponseRecorder, status int) map[string]any {
		t.Helper()
		return object(rec, status)["data"].(map[string]any)["token"].(map[string]any)
	}
	for _, body := range []string{
		`{"kind":"app","write":true,"changes":true}`,
		`{"kind":"app","read":true,"subscribe":true}`,
		`{"kind":"app","write":true,"subscribe":true}`,
	} {
		result := object(request("POST", "/api/connect/tokens", body, cookie, "", "https://files.example"), 400)
		if !strings.Contains(result["error"].(string), "requires") {
			t.Fatal("missing dependency error", result)
		}
	}
	defaults := tokenFrom(request("POST", "/api/connect/tokens", `{"kind":"app","read":true,"write":true,"events":true}`, cookie, "", ""), 201)
	if defaults["changes"] != false || defaults["subscribe"] != false || defaults["policyVersion"] != float64(1) {
		t.Fatalf("old Connect create acquired rights: %v", defaults)
	}
	result := object(request("POST", "/api/connect/tokens", `{"kind":"app","prefix":"Apps/photos","read":true,"write":true,"events":true,"changes":true,"subscribe":true}`, cookie, "", ""), 201)["data"].(map[string]any)
	token := result["token"].(map[string]any)
	secret, id := result["secret"].(string), token["id"].(string)
	if token["changes"] != true || token["subscribe"] != true || token["policyVersion"] != float64(1) {
		t.Fatalf("explicit grants lost: %v", token)
	}
	var payload map[string]any
	if err = json.Unmarshal([]byte(result["payload"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 5 || payload["token"] != secret {
		t.Fatalf("QR no longer credential-only: %v", payload)
	}
	target := "/api/connect/tokens/" + id
	// Owner scope edits retain explicit rights sent by the client.
	policy := `{"prefix":"Apps","read":true,"write":true,"events":true,"changes":true,"subscribe":true}`
	updated := tokenFrom(request("PATCH", target, policy, cookie, "", ""), 200)
	if updated["changes"] != true || updated["subscribe"] != true || updated["policyVersion"] != float64(2) || updated["prefix"] != "Apps" {
		t.Fatalf("scope edit clobbered grants: %v", updated)
	}
	unchanged := tokenFrom(request("PATCH", target, policy, cookie, "", ""), 200)
	if unchanged["policyVersion"] != float64(2) {
		t.Fatalf("identical policy invalidated generation: %v", unchanged)
	}
	object(request("PATCH", target, policy, otherCookie, "", ""), 404)
	object(request("PATCH", target, policy, "", secret, ""), 401)
	object(request("PATCH", target, policy, cookie, "", "https://other.example"), 403)
	bad := object(request("PATCH", target, `{"write":true,"changes":true,"subscribe":true}`, cookie, "", ""), 400)
	if !strings.Contains(bad["error"].(string), "requires") {
		t.Fatal("missing update dependency error", bad)
	}
	named := object(request("PATCH", "/me", `{"name":"Photos app","changes":false,"subscribe":false,"read":false}`, "", secret, ""), 200)
	if named["changes"] != true || named["subscribe"] != true || named["policyVersion"] != float64(2) || named["read"] != true {
		t.Fatalf("self rename changed policy: %v", named)
	}
	me := object(request("GET", "/me", "", "", secret, ""), 200)["token"].(map[string]any)
	if me["changes"] != true || me["subscribe"] != true || me["policyVersion"] != float64(2) {
		t.Fatalf("GET /me omitted current policy: %v", me)
	}
	live := object(request("GET", "/s/"+secret+"?format=json", "", "", "", ""), 200)
	if live["changes"] != true || live["subscribe"] != true || live["policyVersion"] != float64(2) || !strings.Contains(live["prompt"].(string), "read + write + events + changes + subscribe") {
		t.Fatalf("live instruction policy omitted grants: %v", live)
	}
	listed := object(request("GET", "/api/connect/tokens", "", cookie, "", ""), 200)["data"].(map[string]any)["tokens"].([]any)
	found := false
	for _, item := range listed {
		row := item.(map[string]any)
		if row["id"] == id {
			found = row["changes"] == true && row["subscribe"] == true && row["policyVersion"] == float64(2)
		}
	}
	if !found {
		t.Fatal("Connect list lost grants")
	}
	// Older replacement requests leave omitted grants false rather than inheriting them.
	oldUpdate := tokenFrom(request("PATCH", target, `{"prefix":"Apps","read":true,"write":true,"events":true}`, cookie, "", ""), 200)
	if oldUpdate["changes"] != false || oldUpdate["subscribe"] != false || oldUpdate["policyVersion"] != float64(3) {
		t.Fatalf("omitted replacement grants inherited authority: %v", oldUpdate)
	}
	object(request("DELETE", target, "", cookie, "", ""), 200)
	missing := request("GET", "/s/"+secret+"?format=json", "", "", "", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("revoked instruction page returned %d: %s", missing.Code, missing.Body.String())
	}
}

func TestTokenPromptExplicitChangeRights(t *testing.T) {
	granted := tokenPrompt("https://files.example", "secret", &auth.Token{CanRead: true, CanChanges: true, CanSubscribe: true, Prefix: "Apps"})
	for _, text := range []string{"Access:  read + changes + subscribe", "File change history is explicitly granted", "Live file change subscriptions are explicitly granted", "Scope:   /Apps"} {
		if !strings.Contains(granted, text) {
			t.Fatalf("prompt missing %q: %s", text, granted)
		}
	}
	old := tokenPrompt("https://files.example", "secret", &auth.Token{CanRead: true, CanWrite: true, CanEvents: true})
	if strings.Contains(old, "explicitly granted") {
		t.Fatal("old token instructions acquired change rights", old)
	}
}
