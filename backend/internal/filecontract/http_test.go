package filecontract_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
)

// newBoundaryFixture is a deliberately test-only adapter. Credential validity,
// current revisions and operation state are fixture inputs, not storage/runtime
// implementations. It demonstrates the composition future HTTP handlers need;
// none of these tests assert that the current application exposes these routes.
func newBoundaryFixture(t *testing.T, currentContentRevision *string) http.Handler {
	t.Helper()
	policy, err := fc.NewCookieMutationPolicy("https://files.example")
	if err != nil {
		t.Fatal(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(err error) {
			var p *fc.Problem
			if !errors.As(err, &p) {
				t.Fatalf("fixture got unexpected error: %v", err)
			}
			p.RequestID = "req_fixture"
			w.Header().Set("Content-Type", "application/problem+json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(p.Status)
			if err := json.NewEncoder(w).Encode(p); err != nil {
				t.Error(err)
			}
		}
		selection, err := fc.SelectCredential(r.Header)
		if err != nil {
			fail(err)
			return
		}
		scope := fc.Path("/")
		if selection.Source == fc.BearerCredential {
			// This fake verifier makes invalid/expired Bearer failures observable even
			// when a valid cookie is present. Production needs the real auth service.
			if selection.Token != "fixture-token" {
				fail(fc.NewProblem(401, "unauthenticated", "The Bearer token is invalid or expired."))
				return
			}
			scope = "/Notes"
		} else {
			cookie, err := r.Cookie("session")
			if err != nil || cookie.Value != "owner-session" {
				fail(fc.NewProblem(401, "unauthenticated", "A valid session is required."))
				return
			}
		}
		if err := policy.Check(r.Method, r.Header, selection.Source, func(token string) bool { return token == "opaque%2F+session-bound" }); err != nil {
			fail(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		respond := func(status int, value any) {
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(value); err != nil {
				t.Error(err)
			}
		}
		switch r.URL.Path {
		case fc.Base, fc.Base + "/content":
			path, err := fc.ParseQueryPath(r.URL.RawQuery)
			if err != nil {
				fail(err)
				return
			}
			if !fc.WithinScope(scope, path, nil) {
				fail(fc.NewProblem(404, "not_found", "The path was not found."))
				return
			}
			status := 200
			if r.Method == http.MethodPut {
				condition, err := fc.ParsePutCondition(r.Header)
				if err != nil {
					fail(err)
					return
				}
				if err := fc.CheckPutCondition(condition, currentContentRevision); err != nil {
					fail(err)
					return
				}
				if condition.Mode == fc.PutCreate {
					status = 201
				}
			}
			respond(status, fixtureMetadata(path))
		case fc.Base + "/move", fc.Base + "/copy":
			var from, to fc.Path
			var expected string
			status := 200
			if r.URL.Path == fc.Base+"/move" {
				var request fc.MoveRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					fail(fc.NewProblem(400, "invalid_request", "Invalid move JSON."))
					return
				}
				if err := fc.ValidateMove(request); err != nil {
					fail(err)
					return
				}
				from, to, expected = request.From, request.To, request.IfRevision
			} else {
				var request fc.CopyRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					fail(fc.NewProblem(400, "invalid_request", "Invalid copy JSON."))
					return
				}
				if err := fc.ValidateCopy(request); err != nil {
					fail(err)
					return
				}
				from, to, expected, status = request.From, request.To, request.IfSourceRevision, 201
			}
			if !fc.WithinScope(scope, from, nil) || !fc.WithinScope(scope, to, nil) {
				fail(fc.NewProblem(404, "not_found", "The path was not found."))
				return
			}
			if err := fc.CheckRevision(expected, "nr_observed"); err != nil {
				fail(err)
				return
			}
			respond(status, fixtureMetadata(to))
		case fc.Base + "/operations/o_fixture", fc.Base + "/operations/o_fixture/cancel":
			operation := fc.Operation{ID: "o_fixture", Kind: fc.OperationMove, State: fc.OperationOutcomeUnknown, Progress: fc.Progress{Completed: 1, Unit: "entries"}, CancelRequested: r.Method == http.MethodPost, CreatedAt: fixtureTime}
			status := 200
			if r.Method == http.MethodPost {
				status = 202
				w.Header().Set("Location", fc.Base+"/operations/o_fixture")
				w.Header().Set("Retry-After", "2")
			}
			respond(status, operation)
		case fc.Base + "/uploads/u_fixture/parts/1":
			respond(201, map[string]any{"number": 1, "size": 0, "sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"})
		default:
			fail(fc.NewProblem(404, "not_found", "The fixture resource was not found."))
		}
	})
}

func requestFixture(method, target, body string, header http.Header) *http.Request {
	request := httptest.NewRequest(method, "https://untrusted-request-host.example"+target, strings.NewReader(body))
	request.Header = header.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	return request
}

func cookieHeaders() http.Header {
	return http.Header{"Cookie": {"session=owner-session"}, "Origin": {"https://files.example"}, "X-Csrf-Token": {"opaque%2F+session-bound"}}
}

func TestHTTPFixturesCredentialAndRawMutationBoundary(t *testing.T) {
	path := "/Notes/cafe\u0301%2F+.md"
	query := url.Values{"path": {path}}.Encode()
	for _, endpoint := range []struct {
		method, path string
		success      int
	}{
		{"PUT", "/api/files/content?" + query, 201},
		{"PUT", "/api/files/uploads/u_fixture/parts/1", 201},
		{"POST", "/api/files/operations/o_fixture/cancel", 202},
	} {
		tests := []struct {
			name   string
			edit   func(http.Header)
			status int
		}{
			{"valid cookie raw request", func(http.Header) {}, endpoint.success},
			{"missing origin", func(h http.Header) { h.Del("Origin") }, 403},
			{"missing csrf", func(h http.Header) { h.Del("X-CSRF-Token") }, 403},
			{"invalid csrf", func(h http.Header) { h.Set("X-CSRF-Token", "present-but-invalid") }, 403},
			{"forged forwarding", func(h http.Header) {
				h.Set("Origin", "https://evil.example")
				h.Set("X-Forwarded-Host", "files.example")
				h.Set("X-Forwarded-Proto", "https")
			}, 403},
			{"empty authorization ignores valid cookie", func(h http.Header) { h.Set("Authorization", "") }, 401},
			{"whitespace authorization ignores valid cookie", func(h http.Header) { h.Set("Authorization", " \t") }, 401},
			{"expired bearer ignores valid cookie", func(h http.Header) { h.Set("Authorization", "Bearer expired") }, 401},
			{"valid bearer ignores invalid cookie and csrf", func(h http.Header) {
				h.Set("Authorization", "Bearer fixture-token")
				h.Set("Cookie", "session=invalid")
				h.Del("Origin")
				h.Del("X-CSRF-Token")
			}, endpoint.success},
		}
		for _, test := range tests {
			t.Run(endpoint.path+"/"+test.name, func(t *testing.T) {
				header := cookieHeaders()
				test.edit(header)
				recorder := httptest.NewRecorder()
				newBoundaryFixture(t, nil).ServeHTTP(recorder, requestFixture(endpoint.method, endpoint.path, "", header))
				if recorder.Code != test.status {
					t.Fatalf("status = %d; want %d; body = %s", recorder.Code, test.status, recorder.Body.String())
				}
				if recorder.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("metadata/lifecycle fixture is cacheable")
				}
				var object map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
					t.Fatal(err)
				}
				if _, exists := object["ok"]; exists {
					t.Fatal("legacy envelope in boundary fixture")
				}
				if test.status >= 400 {
					if recorder.Header().Get("Content-Type") != "application/problem+json" || object["status"] != float64(test.status) || object["requestId"] != "req_fixture" {
						t.Fatalf("invalid Problem response: %#v", object)
					}
				} else if endpoint.method == "POST" {
					location, err := url.Parse(recorder.Header().Get("Location"))
					if err != nil || location.IsAbs() || location.Host != "" || location.Path != "/api/files/operations/o_fixture" {
						t.Fatalf("invalid operation Location: %v", location)
					}
					if object["state"] != "outcome_unknown" || object["finishedAt"] != nil || object["cancelRequested"] != true {
						t.Fatalf("cancellation falsely terminates uncertain effects: %#v", object)
					}
				} else if strings.Contains(endpoint.path, "/content?") && object["path"] != path {
					t.Fatalf("path spelling changed across HTTP boundary: %#v", object)
				}
			})
		}
	}
}

func TestHTTPFixturesPutPreconditionResponses(t *testing.T) {
	tests := []struct {
		name      string
		current   *string
		condition http.Header
		status    int
		code      string
	}{
		{"default create", nil, nil, 201, ""},
		{"default collision", ptr("cr_observed"), nil, 409, "name_conflict"},
		{"explicit collision", ptr("cr_observed"), http.Header{"If-None-Match": {"*"}}, 412, "precondition_failed"},
		{"replace", ptr("cr_observed"), http.Header{"If-Match": {`"cr_observed"`}}, 200, ""},
		{"stale", ptr("cr_new"), http.Header{"If-Match": {`"cr_observed"`}}, 412, "precondition_failed"},
		{"wildcard", ptr("cr_observed"), http.Header{"If-Match": {"*"}}, 400, "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{"Authorization": {"Bearer fixture-token"}}
			for name, values := range test.condition {
				header[name] = values
			}
			recorder := httptest.NewRecorder()
			newBoundaryFixture(t, test.current).ServeHTTP(recorder, requestFixture("PUT", "/api/files/content?path=%2FNotes%2Fa", "", header))
			if recorder.Code != test.status {
				t.Fatalf("status = %d; want %d", recorder.Code, test.status)
			}
			var object map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &object); err != nil {
				t.Fatal(err)
			}
			if test.code != "" && object["code"] != test.code {
				t.Fatalf("Problem = %#v", object)
			}
		})
	}
}

func TestHTTPFixturesNamespaceRevisionsAndBothPathScopes(t *testing.T) {
	tests := []struct {
		name, route, body string
		status            int
		code              string
	}{
		{"move accepted", "move", `{"from":"/Notes/a","to":"/Notes/b","ifRevision":"nr_observed"}`, 200, ""},
		{"copy accepted", "copy", `{"from":"/Notes/a","to":"/Notes/b","ifSourceRevision":"nr_observed"}`, 201, ""},
		{"move missing", "move", `{"from":"/Notes/a","to":"/Notes/b"}`, 428, "precondition_required"},
		{"copy missing", "copy", `{"from":"/Notes/a","to":"/Notes/b"}`, 428, "precondition_required"},
		{"move stale", "move", `{"from":"/Notes/a","to":"/Notes/b","ifRevision":"nr_old"}`, 412, "precondition_failed"},
		{"source outside scope", "move", `{"from":"/Secret/a","to":"/Notes/b","ifRevision":"nr_observed"}`, 404, "not_found"},
		{"destination outside scope", "copy", `{"from":"/Notes/a","to":"/Secret/b","ifSourceRevision":"nr_observed"}`, 404, "not_found"},
		{"prefix sibling outside scope", "copy", `{"from":"/Notes/a","to":"/Notes-old/b","ifSourceRevision":"nr_observed"}`, 404, "not_found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			newBoundaryFixture(t, nil).ServeHTTP(recorder, requestFixture("POST", "/api/files/"+test.route, test.body, http.Header{"Authorization": {"Bearer fixture-token"}}))
			if recorder.Code != test.status {
				t.Fatalf("status = %d; want %d; body = %s", recorder.Code, test.status, recorder.Body.String())
			}
			if test.code != "" {
				var p fc.Problem
				if err := json.Unmarshal(recorder.Body.Bytes(), &p); err != nil {
					t.Fatal(err)
				}
				if p.Code != test.code {
					t.Fatalf("code = %q; want %q", p.Code, test.code)
				}
			}
		})
	}
}
