package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
)

func getAccountJSON(t *testing.T, server *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

// The handle is the address a person types, so these tests are about the route contract, not
// only the service: the claim has to come back in the user payload every page already reads,
// a second account must not be able to take it, and the old name must keep resolving.
func TestUsernameClaimRenameAndConflictPostgres(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	owner := createAccount(t, ctx, server, "address@example.test", nil)
	other := createAccount(t, ctx, server, "other-address@example.test", nil)
	ownerSession := openSession(t, ctx, server, owner)
	otherSession := openSession(t, ctx, server, other)

	claimed := postAccountJSON(t, server, "/api/auth/username", ownerSession, map[string]any{
		"username": "Address",
	})
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim failed: %d %s", claimed.Code, claimed.Body.String())
	}
	if body := claimed.Body.String(); !strings.Contains(body, `"username":"address"`) {
		t.Fatalf("claim did not answer with the folded handle: %s", body)
	}

	// The handle is part of every account payload, not a second request the pages forgot.
	if me := getAccountJSON(t, server, "/api/auth/me", ownerSession); !strings.Contains(me.Body.String(), `"username":"address"`) {
		t.Fatalf("/api/auth/me lost the handle: %s", me.Body.String())
	}

	if taken := postAccountJSON(t, server, "/api/auth/username", otherSession, map[string]any{
		"username": "address",
	}); taken.Code != http.StatusConflict {
		t.Fatalf("a taken handle answered %d: %s", taken.Code, taken.Body.String())
	}
	if reserved := postAccountJSON(t, server, "/api/auth/username", otherSession, map[string]any{
		"username": "settings",
	}); reserved.Code != http.StatusBadRequest {
		t.Fatalf("a reserved handle answered %d: %s", reserved.Code, reserved.Body.String())
	}

	renamed := postAccountJSON(t, server, "/api/auth/username", ownerSession, map[string]any{
		"username": "address-drive",
	})
	if renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), `"username":"address-drive"`) {
		t.Fatalf("rename failed: %d %s", renamed.Code, renamed.Body.String())
	}
	if alias := getAccountJSON(t, server, "/api/auth/username/address", ownerSession); alias.Code != http.StatusOK ||
		!strings.Contains(alias.Body.String(), `"username":"address-drive"`) {
		t.Fatalf("old handle did not resolve: %d %s", alias.Code, alias.Body.String())
	}
	// Aliases are not a directory: another account cannot resolve this one's old name.
	if foreign := getAccountJSON(t, server, "/api/auth/username/address", otherSession); foreign.Code != http.StatusNotFound {
		t.Fatalf("a foreign handle resolved: %d %s", foreign.Code, foreign.Body.String())
	}
	// And the superseded name stays out of reach even after the rename.
	if retaken := postAccountJSON(t, server, "/api/auth/username", otherSession, map[string]any{
		"username": "address",
	}); retaken.Code != http.StatusConflict {
		t.Fatalf("a superseded alias was claimable: %d %s", retaken.Code, retaken.Body.String())
	}
}

func TestUsernameRequiresASession(t *testing.T) {
	server, _, ctx := newAccountSessionTestServer(t)
	createAccount(t, ctx, server, "anonymous@example.test", nil)
	if claimed := postAccountJSON(t, server, "/api/auth/username", "", map[string]any{
		"username": "nobody",
	}); claimed.Code != http.StatusUnauthorized {
		t.Fatalf("an anonymous claim answered %d: %s", claimed.Code, claimed.Body.String())
	}
}
