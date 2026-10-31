package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
)

func TestFilesMalformedAuthorizationNeverVerifiesAmbientCookie(t *testing.T) {
	s := New(nil, &config.Config{AppSecret: "fixture"})
	for _, value := range []string{"", " \t", "Basic value", "Bearer", "Bearer bad token"} {
		r := httptest.NewRequest("GET", "/api/files/me", nil)
		r.Header.Set("Authorization", value)
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "would-hit-database"})
		_, source, err := s.AuthenticateFiles(context.Background(), r)
		if err == nil || source != fc.BearerCredential {
			t.Fatalf("header %q fell back: %s %v", value, source, err)
		}
	}
}
func TestFileCSRFBoundToSessionAndAppSecret(t *testing.T) {
	s := New(nil, &config.Config{AppSecret: "fixture"})
	r := httptest.NewRequest("PUT", "/api/files/content?path=%2Fa", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "random-session-a"})
	token := s.FileCSRFToken(r)
	if token == "" || !s.ValidateFileCSRF(r, token) || s.ValidateFileCSRF(r, "wrong") {
		t.Fatal("invalid CSRF identity")
	}
	other := httptest.NewRequest("PUT", "/api/files/content?path=%2Fa", nil)
	other.AddCookie(&http.Cookie{Name: SessionCookie, Value: "random-session-b"})
	if s.ValidateFileCSRF(other, token) {
		t.Fatal("token copied across sessions")
	}
	rotated := New(nil, &config.Config{AppSecret: "different"})
	if rotated.ValidateFileCSRF(r, token) {
		t.Fatal("token survived application secret rotation")
	}
}
