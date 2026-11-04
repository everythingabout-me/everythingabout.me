package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
)

func auditTestServer(principal *auth.Principal) *Server {
	return &Server{cfg: &config.Config{}, filesAuth: &boundaryAuth{principal: principal}, trees: &boundaryFactory{driver: &boundaryDriver{}}}
}

func auditTestPrincipal() *auth.Principal {
	return &auth.Principal{User: &auth.User{ID: "41fdd829-dbec-48ba-b029-25c67118670c"}, Token: &auth.Token{ID: "grant-id", Name: "Original app", Prefix: "Photos", CanRead: true, PolicyVersion: 7}}
}

func TestFilesAuditAdmissionFailureBlocksProtectedOutput(t *testing.T) {
	s := auditTestServer(auditTestPrincipal())
	called, attempts := false, 0
	persist := func(_ context.Context, a *filechanges.Audit) (bool, error) {
		attempts++
		if attempts == 1 && (a.Outcome != "admitted" || a.BytesWritten != nil) {
			t.Fatalf("invalid admission: %+v", a)
		}
		return false, errors.New("unavailable")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/files/content?path=%2FPhotos%2Fa", nil)
	r.Header.Set("Authorization", "Bearer valid")
	s.fileWrapWithAudit(func(w http.ResponseWriter, _ *http.Request, _ *fileRequest) error {
		called = true
		_, _ = io.WriteString(w, "protected-content")
		return nil
	}, persist)(w, r)
	if called || w.Code != 503 || strings.Contains(w.Body.String(), "protected-content") || attempts != 2 {
		t.Fatalf("admission failed open: called=%t status=%d attempts=%d", called, w.Code, attempts)
	}
}

func TestFilesAuditCompletionSnapshotsAndSanitizes(t *testing.T) {
	principal := auditTestPrincipal()
	s := auditTestServer(principal)
	var rows []filechanges.Audit
	persist := func(_ context.Context, a *filechanges.Audit) (bool, error) {
		rows = append(rows, *a)
		return true, nil
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/files/content?path=%2FPhotos%2Fliteral%252F.txt&signature=secret-signature&q=private-search", nil)
	r.Header.Set("Authorization", "Bearer valid")
	s.fileWrapWithAudit(func(w http.ResponseWriter, r *http.Request, _ *fileRequest) error {
		if len(rows) != 1 {
			t.Fatal("handler entered before admission")
		}
		actor, requestID, ok := filechanges.ActorFromContext(r.Context())
		if !ok || actor.Label != "Original app" || requestID != rows[0].RequestID {
			t.Fatal("settlement attribution missing from handler context")
		}
		principal.Token.Name, principal.Token.Prefix, principal.Token.CanRead = "Renamed", "Other", false
		w.WriteHeader(206)
		_, err := io.WriteString(w, "file-body")
		return err
	}, persist)(w, r)
	if len(rows) != 2 {
		t.Fatalf("records=%d", len(rows))
	}
	a := rows[1]
	if a.Action != "read" || a.Status != 206 || a.Outcome != "completed" || a.Allowed == nil || !*a.Allowed || a.BytesWritten == nil || *a.BytesWritten != 9 || a.Actor.Label != "Original app" || a.Actor.Scope != "/Photos" || a.Actor.PolicyGeneration != "7" || !a.Actor.CanRead || a.Path == nil || *a.Path != "/Photos/literal%2F.txt" {
		t.Fatalf("incorrect completion: %+v", a)
	}
	if a.RequestID == "" || a.RequestID != rows[0].RequestID || a.RequestID != w.Header().Get("X-Request-Id") {
		t.Fatal("request correlation lost")
	}
	encoded, _ := json.Marshal(rows)
	for _, secret := range []string{"secret-signature", "private-search", "file-body", "Bearer valid", "Renamed"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("audit contains forbidden data %q", secret)
		}
	}
}

func TestFilesAuditDeniedBeforeProblemAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		status  int
		outcome string
	}{
		{"forbidden", fc.NewProblem(403, "forbidden", "No read permission."), 403, "denied"},
		{"concealed", fc.NewProblem(404, "not_found", "The file was not found."), 404, "denied"},
		{"cancelled", context.Canceled, 503, "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := auditTestServer(auditTestPrincipal())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w := httptest.NewRecorder()
			var completed filechanges.Audit
			persist := func(ctx context.Context, a *filechanges.Audit) (bool, error) {
				if a.Outcome != "admitted" {
					if ctx.Err() != nil || w.Body.Len() != 0 {
						t.Fatal("completion cancelled or written after Problem Details")
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > filesAuditTimeout {
						t.Fatal("completion context is not bounded")
					}
					completed = *a
				}
				return true, nil
			}
			r := httptest.NewRequest("GET", "/api/files/content?path=%2FPhotos%2Fa", nil).WithContext(ctx)
			s.fileWrapWithAudit(func(http.ResponseWriter, *http.Request, *fileRequest) error {
				cancel()
				return tc.err
			}, persist)(w, r)
			if w.Code != tc.status || completed.Outcome != tc.outcome || completed.BytesWritten == nil || *completed.BytesWritten != 0 {
				t.Fatalf("incorrect failed outcome: %+v status=%d", completed, w.Code)
			}
		})
	}
}

func TestFilesAuditCompletionFailurePreservesResponse(t *testing.T) {
	s := auditTestServer(auditTestPrincipal())
	w := httptest.NewRecorder()
	persist := func(_ context.Context, a *filechanges.Audit) (bool, error) {
		if a.Outcome != "admitted" {
			return false, errors.New("completion unavailable")
		}
		return true, nil
	}
	r := httptest.NewRequest("PUT", "/api/files/content?path=%2FPhotos%2Fa", nil)
	r.Header.Set("Authorization", "Bearer valid")
	s.fileWrapWithAudit(func(w http.ResponseWriter, _ *http.Request, _ *fileRequest) error {
		w.WriteHeader(201)
		_, err := io.WriteString(w, "native-outcome")
		return err
	}, persist)(w, r)
	if w.Code != 201 || w.Body.String() != "native-outcome" {
		t.Fatalf("completion rewrote native outcome: %d %s", w.Code, w.Body.String())
	}
}

type auditShortWriter struct{ header http.Header }

func (w *auditShortWriter) Header() http.Header       { return w.header }
func (*auditShortWriter) WriteHeader(int)             {}
func (*auditShortWriter) Write(p []byte) (int, error) { return 2, io.ErrUnexpectedEOF }

func TestFilesAuditInterruptedStreamCountsAcceptedBytes(t *testing.T) {
	s := auditTestServer(auditTestPrincipal())
	var completion filechanges.Audit
	persist := func(_ context.Context, a *filechanges.Audit) (bool, error) {
		if a.Outcome != "admitted" {
			completion = *a
		}
		return true, nil
	}
	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("partial stream did not abort: %v", recovered)
		}
		if completion.Outcome != "interrupted" || completion.Status != 200 || completion.BytesWritten == nil || *completion.BytesWritten != 2 {
			t.Fatalf("incorrect partial completion: %+v", completion)
		}
	}()
	s.fileWrapWithAudit(func(w http.ResponseWriter, _ *http.Request, _ *fileRequest) error {
		_, err := w.Write([]byte("not-all-written"))
		return err
	}, persist)(&auditShortWriter{header: make(http.Header)}, httptest.NewRequest("GET", "/api/files/content?path=%2FPhotos%2Fa", nil))
}

func TestFilesAuditRouteCategoriesAndSelectors(t *testing.T) {
	for _, tc := range []struct{ method, target, action string }{
		{"GET", "/api/files?path=%2FPhotos", "metadata"},
		{"GET", "/api/files/list?path=%2FPhotos", "list"},
		{"GET", "/api/files/search?q=private", "search"},
		{"HEAD", "/api/files/content?path=%2FPhotos%2Fa", "head"},
		{"PUT", "/api/files/content?path=%2FPhotos%2Fa", "put"},
		{"POST", "/api/files/move", "move"},
		{"POST", "/api/files/copy", "copy"},
		{"DELETE", "/api/files?path=%2FPhotos%2Fa", "delete"},
		{"PUT", "/api/files/uploads/private-handle/parts/1?signature=secret", "upload_part"},
		{"POST", "/api/files/uploads/private-handle/commit", "upload_commit"},
	} {
		a := filesAuditAttempt(httptest.NewRequest(tc.method, tc.target, nil), auditTestPrincipal(), "req-test")
		if a.Action != tc.action {
			t.Fatalf("%s %s action=%q", tc.method, tc.target, a.Action)
		}
	}
	for _, query := range []string{"path=%2FPhotos%2F..%2Fprivate", "path=%2Fone&path=%2Ftwo", "path=%00", "id=secret-signature", "path=%ZZ"} {
		r := httptest.NewRequest("GET", "/api/files", nil)
		r.URL.RawQuery = query
		a := filesAuditAttempt(r, auditTestPrincipal(), "req-test")
		if a.Path != nil || a.EntryID != nil {
			t.Fatalf("malformed selector admitted into audit: %s %+v", query, a)
		}
	}
	principal := auditTestPrincipal()
	principal.Token.ID = auth.SaveLinkTokenPrefix + "retired-link"
	if actor := filesAuditActor(principal); actor.Kind != "save_link" || actor.GrantID != principal.Token.ID || actor.PrincipalID != filePrincipalID(principal) {
		t.Fatalf("save-link attribution lost: %+v", actor)
	}
}

func TestFilesAuditOperationCorrelation(t *testing.T) {
	const operationID = "41fdd829-dbec-48ba-b029-25c67118670c"
	attempt := filesAuditAttempt(httptest.NewRequest("POST", "/api/files/move", nil), auditTestPrincipal(), "req-operation")
	writer := &filesResponseWriter{ResponseWriter: httptest.NewRecorder()}
	writer.Header().Set("Location", fc.Base+"/operations/"+operationID)
	writer.WriteHeader(http.StatusAccepted)
	filesAuditCompletion(context.Background(), func(_ context.Context, a *filechanges.Audit) (bool, error) {
		if a.OperationID != operationID || a.RequestID != "req-operation" || a.Status != 202 {
			t.Fatalf("operation correlation lost: %+v", a)
		}
		return true, nil
	}, attempt, writer, nil)
	attempt = filesAuditAttempt(httptest.NewRequest("GET", fc.Base+"/operations/"+operationID, nil), auditTestPrincipal(), "req-status")
	if attempt.OperationID != operationID {
		t.Fatal("operation request correlation lost")
	}
}

func TestFilesAuditUnknownOwnerNeverWritesRow(t *testing.T) {
	s := auditTestServer(nil)
	recorded := false
	w := httptest.NewRecorder()
	s.fileWrapWithAudit(func(http.ResponseWriter, *http.Request, *fileRequest) error {
		t.Fatal("unauthenticated handler")
		return nil
	}, func(context.Context, *filechanges.Audit) (bool, error) {
		recorded = true
		return true, nil
	})(w, httptest.NewRequest("GET", "/api/files/list?path=%2F", nil))
	if recorded || w.Code != 401 {
		t.Fatalf("unowned request audited: %t status=%d", recorded, w.Code)
	}
}

func TestOwnerAuditAuthorizationAndPaginationValidation(t *testing.T) {
	for _, tc := range []struct {
		name, target, bearer string
		principal            *auth.Principal
		status               int
	}{
		{"bearer owner", "/api/audit", "Bearer valid", &auth.Principal{User: &auth.User{ID: "owner"}}, 403},
		{"cookie token", "/api/audit", "", auditTestPrincipal(), 403},
		{"missing owner", "/api/audit", "", nil, 401},
		{"oversized page", "/api/audit?limit=101", "", &auth.Principal{User: &auth.User{ID: "owner"}}, 400},
		{"duplicate page", "/api/audit?limit=1&limit=2", "", &auth.Principal{User: &auth.User{ID: "owner"}}, 400},
		{"invalid cursor", "/api/audit?cursor=invalid", "", &auth.Principal{User: &auth.User{ID: "owner"}}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := auditTestServer(tc.principal)
			r := httptest.NewRequest("GET", tc.target, nil)
			if tc.bearer != "" {
				r.Header.Set("Authorization", tc.bearer)
			}
			w := httptest.NewRecorder()
			s.ownerAuditHandler()(w, r)
			if w.Code != tc.status || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestFilesAuditPostgresAdmissionAndOwnerPages(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	root, _ := fixture.owner(t)
	principal := auditTestPrincipal()
	principal.User.ID = root.OwnerID
	s := auditTestServer(principal)
	s.db = fixture.db
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		s.fileWrap(func(w http.ResponseWriter, _ *http.Request, _ *fileRequest) error {
			return writeFilesJSON(w, 200, map[string]bool{"allowed": true})
		})(w, httptest.NewRequest("GET", "/api/files/list?path=%2FPhotos", nil))
		if w.Code != 200 {
			t.Fatalf("request failed: %d %s", w.Code, w.Body.String())
		}
	}
	denied := httptest.NewRecorder()
	s.fileWrap(func(http.ResponseWriter, *http.Request, *fileRequest) error {
		return fc.NewProblem(403, "forbidden", "This credential does not permit the read.")
	})(denied, httptest.NewRequest("GET", "/api/files/content?path=%2FPhotos%2Fa", nil))
	audits, err := filechanges.ReadAudit(fixture.ctx, fixture.db.Pool(), root.OwnerID, 1)
	if err != nil || len(audits) != 1 || audits[0].Outcome != "denied" || audits[0].Status != 403 || audits[0].BytesWritten == nil || *audits[0].BytesWritten != 0 || audits[0].Actor.Label != "Original app" {
		t.Fatalf("denied audit was not persisted: %+v %v", audits, err)
	}
	s.filesAuth = &boundaryAuth{principal: &auth.Principal{User: principal.User}}
	var page fc.Collection[filechanges.Audit]
	w := httptest.NewRecorder()
	s.ownerAuditHandler()(w, httptest.NewRequest("GET", "/api/audit?limit=2", nil))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("bad audit page: %d %s", w.Code, w.Body.String())
	}
	first, firstCursor := page.Items[0].ID, *page.NextCursor
	otherRoot, _ := fixture.owner(t)
	s.filesAuth = &boundaryAuth{principal: &auth.Principal{User: &auth.User{ID: otherRoot.OwnerID}}}
	w = httptest.NewRecorder()
	s.ownerAuditHandler()(w, httptest.NewRequest("GET", "/api/audit", nil))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 0 {
		t.Fatalf("audit crossed owner boundary: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	s.ownerAuditHandler()(w, httptest.NewRequest("GET", "/api/audit?cursor="+firstCursor, nil))
	if w.Code != 400 {
		t.Fatalf("copied owner cursor accepted: %d %s", w.Code, w.Body.String())
	}
	s.filesAuth = &boundaryAuth{principal: &auth.Principal{User: principal.User}}
	w = httptest.NewRecorder()
	s.ownerAuditHandler()(w, httptest.NewRequest("GET", "/api/audit?limit=2&cursor="+firstCursor, nil))
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].ID == first {
		t.Fatalf("bad next page: %d %s", w.Code, w.Body.String())
	}
	// Use the isolated disposable database to prove DB admission failure cannot
	// enter a protected handler. Other tests and production data are untouched.
	if _, err := fixture.db.Pool().Exec(fixture.ctx, `alter table file_audit rename to file_audit_unavailable`); err != nil {
		t.Fatal(err)
	}
	s.filesAuth = &boundaryAuth{principal: principal}
	called := false
	w = httptest.NewRecorder()
	s.fileWrap(func(w http.ResponseWriter, _ *http.Request, _ *fileRequest) error {
		called = true
		_, err := io.WriteString(w, "protected-bytes")
		return err
	})(w, httptest.NewRequest("GET", "/api/files/content?path=%2FPhotos%2Fa", nil))
	if called || w.Code != 503 || strings.Contains(w.Body.String(), "protected-bytes") {
		t.Fatalf("DB failure admitted content: called=%t status=%d", called, w.Code)
	}
}
