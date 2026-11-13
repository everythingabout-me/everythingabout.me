package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// These tests exercise the account routes themselves, not just the auth service, because
// the security property lives at the join: the handler must name the session that made the
// change, and the service must delete the rest in the same transaction as the credential
// change. A stolen cookie is the credential an attacker keeps when the owner changes a
// password or moves the account to a new address; if those sessions survive, the recovery
// step is theater. They need a disposable Postgres and skip without one, like the rest of
// the Postgres-backed suite.
func newAccountSessionTestServer(t *testing.T) (*Server, *store.DB, context.Context) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_account_session_")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("repository migrations unavailable", err)
	}
	for _, migration := range migrations {
		data, readErr := os.ReadFile(migration)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if _, execErr := db.Pool().Exec(ctx, string(data)); execErr != nil {
			t.Fatalf("migration %s: %v", filepath.Base(migration), execErr)
		}
	}
	cfg := &config.Config{
		AppEnv:           "development",
		AppSecret:        "account-session-revocation-test",
		SessionTTLDays:   1,
		RegistrationOpen: true,
		Storage: config.Storage{
			Backend:           "local",
			ConnectionID:      "account-session-test",
			Generation:        1,
			Local:             config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir()},
			CoordinatedWrites: true,
		},
	}
	// The account routes never touch file bytes, so no storage driver is opened.
	return New(cfg, db, nil, nil), db, ctx
}

func createAccount(t *testing.T, ctx context.Context, server *Server, email string, passwordHash *string) string {
	t.Helper()
	row, err := server.auth.CreateUser(ctx, email, passwordHash)
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}

func hashedPassword(t *testing.T, password string) *string {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	return &hash
}

// openSession mints a real session row and returns its cookie token.
func openSession(t *testing.T, ctx context.Context, server *Server, userID string) string {
	t.Helper()
	token, _, err := server.auth.CreateSession(ctx, userID, "account-session-test", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func sessionAlive(t *testing.T, ctx context.Context, server *Server, token string) bool {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	user, err := server.auth.SessionUser(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	return user != nil
}

func sessionRows(t *testing.T, ctx context.Context, db *store.DB, userID string) int {
	t.Helper()
	var count int
	if err := db.Pool().QueryRow(ctx, `select count(*) from sessions where user_id = $1::uuid`, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func postAccountJSON(t *testing.T, server *Server, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestPasswordChangeRevokesOtherSessionsPostgres(t *testing.T) {
	server, db, ctx := newAccountSessionTestServer(t)
	userID := createAccount(t, ctx, server, "password@example.test", hashedPassword(t, "old-password-123"))
	current := openSession(t, ctx, server, userID)
	others := []string{openSession(t, ctx, server, userID), openSession(t, ctx, server, userID)}

	// A rejected attempt is not a credential change: it must not revoke anything, or an
	// attacker who briefly holds the cookie could sign the owner out of their own devices.
	refused := postAccountJSON(t, server, "/api/auth/password", current, map[string]any{
		"password": "attacker-chosen-123", "currentPassword": "wrong-password",
	})
	if refused.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong current password was not rejected: %d %s", refused.Code, refused.Body.String())
	}
	if got := sessionRows(t, ctx, db, userID); got != 3 {
		t.Fatalf("a rejected password change revoked sessions: %d rows left", got)
	}

	changed := postAccountJSON(t, server, "/api/auth/password", current, map[string]any{
		"password": "new-password-123", "currentPassword": "old-password-123",
	})
	if changed.Code != http.StatusOK {
		t.Fatalf("password change failed: %d %s", changed.Code, changed.Body.String())
	}
	if !sessionAlive(t, ctx, server, current) {
		t.Fatal("the session that changed the password was revoked along with the others")
	}
	for _, token := range others {
		if sessionAlive(t, ctx, server, token) {
			t.Fatal("another session survived the password change")
		}
	}
	if got := sessionRows(t, ctx, db, userID); got != 1 {
		t.Fatalf("password change left %d session rows, want only the current one", got)
	}
}

func TestEmailChangeRevokesOtherSessionsPostgres(t *testing.T) {
	server, db, ctx := newAccountSessionTestServer(t)
	const oldEmail, newEmail = "old@example.test", "new@example.test"
	userID := createAccount(t, ctx, server, oldEmail, nil)
	current := openSession(t, ctx, server, userID)
	other := openSession(t, ctx, server, userID)

	// The two codes are what the browser obtains from the real endpoints; issuing them
	// directly keeps this test about session revocation, not about mail delivery.
	oldCode, err := server.codes.Issue(ctx, oldEmail, "change_email_old", "")
	if err != nil {
		t.Fatal(err)
	}
	newCode, err := server.codes.Issue(ctx, newEmail, "change_email", "")
	if err != nil {
		t.Fatal(err)
	}
	changed := postAccountJSON(t, server, "/api/auth/email", current, map[string]any{
		"email": newEmail, "oldCode": oldCode, "code": newCode,
	})
	if changed.Code != http.StatusOK {
		t.Fatalf("email change failed: %d %s", changed.Code, changed.Body.String())
	}

	if !sessionAlive(t, ctx, server, current) {
		t.Fatal("the session that changed the email was revoked along with the others")
	}
	if sessionAlive(t, ctx, server, other) {
		t.Fatal("a session opened under the old address survived the email change")
	}
	if got := sessionRows(t, ctx, db, userID); got != 1 {
		t.Fatalf("email change left %d session rows, want only the current one", got)
	}
	var email string
	if err := db.Pool().QueryRow(ctx, `select email from users where id = $1::uuid`, userID).Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != newEmail {
		t.Fatalf("email change stored %q, want %q", email, newEmail)
	}
}

// The development login shortcut is the behavior `make dev` relies on: with no SES
// credentials the code comes back in the response (and is logged) instead of being mailed.
// APP_ENV now has no default, so this is the test that the development value still reaches
// this branch after the variable became mandatory.
func TestVerificationCodeReachesDevelopmentClientPostgres(t *testing.T) {
	server, _, _ := newAccountSessionTestServer(t)
	sent := postAccountJSON(t, server, "/api/auth/code", "", map[string]any{"email": "dev-code@example.test"})
	if sent.Code != http.StatusOK {
		t.Fatalf("development code request failed: %d %s", sent.Code, sent.Body.String())
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			ExpiresIn int    `json:"expiresIn"`
			DevCode   string `json:"devCode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(sent.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Data.ExpiresIn <= 0 {
		t.Fatalf("unexpected code response: %s", sent.Body.String())
	}
	if len(envelope.Data.DevCode) != 6 {
		t.Fatalf("development mode did not return a verification code for the login page: %s", sent.Body.String())
	}
}
