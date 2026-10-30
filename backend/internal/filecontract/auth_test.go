package filecontract_test

import (
	"net/http"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
)

func TestAuthorizationPresenceSelectsBearerWithoutCookieFallback(t *testing.T) {
	tests := []struct {
		name   string
		header http.Header
		source fc.CredentialSource
		token  string
		status int
	}{
		{"absent", http.Header{"Cookie": {"session=valid"}}, fc.CookieCredential, "", 0},
		{"empty", http.Header{"Authorization": {""}, "Cookie": {"session=valid"}}, fc.BearerCredential, "", 401},
		{"present without values", http.Header{"Authorization": nil}, fc.BearerCredential, "", 401},
		{"whitespace", http.Header{"Authorization": {" \t "}}, fc.BearerCredential, "", 401},
		{"unsupported", http.Header{"Authorization": {"Basic abc"}}, fc.BearerCredential, "", 401},
		{"no token", http.Header{"Authorization": {"Bearer"}}, fc.BearerCredential, "", 401},
		{"multiple fields", http.Header{"Authorization": {"Bearer one two"}}, fc.BearerCredential, "", 401},
		{"combined", http.Header{"Authorization": {"Bearer one, Bearer two"}}, fc.BearerCredential, "", 401},
		{"repeated", http.Header{"Authorization": {"Bearer one", "Bearer two"}}, fc.BearerCredential, "", 401},
		{"noncanonical key", http.Header{"authorization": {""}}, fc.BearerCredential, "", 401},
		{"duplicate casing", http.Header{"Authorization": {"Bearer one"}, "authorization": {"Bearer two"}}, fc.BearerCredential, "", 401},
		{"newline separator", http.Header{"Authorization": {"Bearer\nvalid"}}, fc.BearerCredential, "", 401},
		{"Unicode separator", http.Header{"Authorization": {"Bearer\u00a0valid"}}, fc.BearerCredential, "", 401},
		{"Unicode outer whitespace", http.Header{"Authorization": {"\u00a0Bearer valid"}}, fc.BearerCredential, "", 401},
		{"bad token", http.Header{"Authorization": {"Bearer a=b"}}, fc.BearerCredential, "", 401},
		{"padding only", http.Header{"Authorization": {"Bearer ="}}, fc.BearerCredential, "", 401},
		{"valid", http.Header{"Authorization": {"Bearer abc-._~+/=="}, "Cookie": {"session=other"}}, fc.BearerCredential, "abc-._~+/==", 0},
		{"scheme casing", http.Header{"Authorization": {"bEaReR valid"}}, fc.BearerCredential, "valid", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := fc.SelectCredential(test.header)
			code := ""
			if test.status != 0 {
				code = "unauthenticated"
			}
			assertProblem(t, err, test.status, code)
			if selection.Source != test.source || selection.Token != test.token {
				t.Fatalf("selection = %+v; want source %q token %q", selection, test.source, test.token)
			}
		})
	}
}

func TestCookieMutationOriginAndOpaqueTokenPolicy(t *testing.T) {
	policy, err := fc.NewCookieMutationPolicy("https://files.example")
	assertProblem(t, err, 0, "")
	tests := []struct {
		name, method  string
		header        http.Header
		source        fc.CredentialSource
		validator     bool
		status, calls int
	}{
		{"valid raw put", "PUT", http.Header{"Origin": {"https://files.example"}, "X-Csrf-Token": {" opaque%2F+ token "}}, fc.CookieCredential, true, 0, 1},
		{"default port canonicalized", "POST", http.Header{"Origin": {"https://FILES.example:443"}, "X-Csrf-Token": {" opaque%2F+ token "}}, fc.CookieCredential, true, 0, 1},
		{"missing origin", "PUT", http.Header{"X-Csrf-Token": {" opaque%2F+ token "}}, fc.CookieCredential, true, 403, 0},
		{"null origin", "PUT", http.Header{"Origin": {"null"}}, fc.CookieCredential, true, 403, 0},
		{"foreign origin", "DELETE", http.Header{"Origin": {"https://evil.example"}}, fc.CookieCredential, true, 403, 0},
		{"wrong scheme", "POST", http.Header{"Origin": {"http://files.example"}}, fc.CookieCredential, true, 403, 0},
		{"wrong port", "POST", http.Header{"Origin": {"https://files.example:444"}}, fc.CookieCredential, true, 403, 0},
		{"path origin", "POST", http.Header{"Origin": {"https://files.example/"}}, fc.CookieCredential, true, 403, 0},
		{"multiple origins", "PUT", http.Header{"Origin": {"https://files.example", "https://files.example"}}, fc.CookieCredential, true, 403, 0},
		{"joined origins", "PUT", http.Header{"Origin": {"https://files.example https://evil.example"}}, fc.CookieCredential, true, 403, 0},
		{"missing token", "PUT", http.Header{"Origin": {"https://files.example"}}, fc.CookieCredential, true, 403, 0},
		{"empty token", "PUT", http.Header{"Origin": {"https://files.example"}, "X-Csrf-Token": {""}}, fc.CookieCredential, true, 403, 0},
		{"invalid token", "PUT", http.Header{"Origin": {"https://files.example"}, "X-Csrf-Token": {" opaque%2F+ token "}}, fc.CookieCredential, false, 403, 1},
		{"duplicate tokens", "PUT", http.Header{"Origin": {"https://files.example"}, "X-Csrf-Token": {"a", "b"}}, fc.CookieCredential, true, 403, 0},
		{"forwarding cannot approve", "PUT", http.Header{"Origin": {"https://evil.example"}, "Forwarded": {"host=files.example;proto=https"}, "X-Forwarded-Host": {"files.example"}}, fc.CookieCredential, true, 403, 0},
		{"referer cannot substitute", "PUT", http.Header{"Referer": {"https://files.example/page"}}, fc.CookieCredential, true, 403, 0},
		{"valid bearer ignores ambient origin", "PUT", http.Header{"Origin": {"https://evil.example"}}, fc.BearerCredential, false, 0, 0},
		{"read cookie", "GET", nil, fc.CookieCredential, false, 0, 0},
		{"head cookie", "HEAD", nil, fc.CookieCredential, false, 0, 0},
		{"preflight", "OPTIONS", nil, fc.CookieCredential, false, 0, 0},
		{"unknown method unsafe", "CUSTOM", nil, fc.CookieCredential, true, 403, 0},
		{"unknown source", "GET", nil, "", true, 403, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := policy.Check(test.method, test.header, test.source, func(token string) bool {
				calls++
				if token != " opaque%2F+ token " {
					t.Fatalf("opaque token changed to %q", token)
				}
				return test.validator
			})
			code := ""
			if test.status != 0 {
				code = "forbidden"
			}
			assertProblem(t, err, test.status, code)
			if calls != test.calls {
				t.Fatalf("validator called %d times; want %d", calls, test.calls)
			}
		})
	}
	header := http.Header{"Origin": {"https://files.example"}, "X-Csrf-Token": {"present"}}
	assertProblem(t, policy.Check("PUT", header, fc.CookieCredential, nil), 403, "forbidden")
	var unconfigured fc.CookieMutationPolicy
	assertProblem(t, unconfigured.Check("PUT", header, fc.CookieCredential, func(string) bool { return true }), 403, "forbidden")
}

func TestCanonicalOriginConfiguration(t *testing.T) {
	for _, origin := range []string{"https://files.example", "http://localhost:5173", "http://192.168.1.2:8080", "http://[::1]:8080"} {
		if _, err := fc.NewCookieMutationPolicy(origin); err != nil {
			t.Errorf("valid origin %q: %v", origin, err)
		}
	}
	for _, origin := range []string{"", "null", "*", "//files.example", "ftp://files.example", "https://user:secret@files.example", "https://files.example/", "https://files.example/path", "https://files.example?", "https://files.example#", "https://files.example:bad", "https://files.example:", "https://files.example:0", "https://files.example:65536", "https://files.example:999999999999999999999"} {
		if _, err := fc.NewCookieMutationPolicy(origin); err == nil {
			t.Errorf("accepted invalid canonical origin %q", origin)
		}
	}
}
