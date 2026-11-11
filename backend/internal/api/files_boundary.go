package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/storage"
)

type FilesAuthenticator interface {
	AuthenticateFiles(context.Context, *http.Request) (*auth.Principal, fc.CredentialSource, error)
	FileCSRFToken(*http.Request) string
	ValidateFileCSRF(*http.Request, string) bool
}

func writeFilesJSON(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(value)
}
func writeFilesProblem(w http.ResponseWriter, r *http.Request, err error) {
	if w.Header().Get("X-Request-Id") == "" {
		var id [12]byte
		_, _ = rand.Read(id[:])
		w.Header().Set("X-Request-Id", "req_"+hex.EncodeToString(id[:]))
	}
	p := filesProblem(err)
	copy := *p
	copy.RequestID = w.Header().Get("X-Request-Id")
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	if copy.Status == 401 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="files"`)
	}
	w.WriteHeader(copy.Status)
	if r.Method != http.MethodHead {
		_ = json.NewEncoder(w).Encode(copy)
	}
}
func filesProblem(err error) *fc.Problem {
	var p *fc.Problem
	if errors.As(err, &p) {
		return p
	}
	var native *storage.Error
	if errors.As(err, &native) {
		switch native.Code {
		case "not_found":
			return fc.NewProblem(404, "not_found", "The path was not found.")
		case "not_a_file", "not_file":
			return fc.NewProblem(409, "not_a_file", "The path is not a file.")
		case "not_a_folder", "not_folder":
			return fc.NewProblem(409, "not_a_folder", "The path is not a folder.")
		case "ambiguous_path", "name_conflict":
			return fc.NewProblem(409, "name_conflict", "The native path is ambiguous.")
		case "invalid_name":
			return fc.NewProblem(400, "invalid_path", "The native path name is not supported.")
		case "changed":
			return fc.NewProblem(412, "precondition_failed", "The file changed before the read was selected.")
		case "invalid_range":
			return fc.NewProblem(416, "range_not_satisfiable", "The byte range is not satisfiable.")
		case "unsupported":
			return fc.NewProblem(422, "unsupported_option", "The requested read guarantee is not supported.")
		case "invalid_cursor":
			return fc.NewProblem(409, "cursor_stale", "The cursor is invalid for this request.")
		case "resource_limit":
			return fc.NewProblem(503, "temporarily_unavailable", "The directory exceeds the configured read budget.")
		case "upload_aborted", "upload_expired":
			return fc.NewProblem(410, native.Code, "The upload session is no longer open.")
		case "upload_incomplete":
			return fc.NewProblem(409, "upload_incomplete", "The upload session is missing provider parts.")
		case "intent_conflict":
			return fc.NewProblem(409, "idempotency_conflict", "The session intent does not match its stored identity.")
		case "outcome_unknown":
			// The native effect may or may not have happened; the session identity is
			// still the only way to settle it, so a blind retry is not offered.
			return fc.NewProblem(503, "storage_outcome_unknown", "The storage outcome is unknown. Resume through the same session.")
		case "closed":
			return fc.NewProblem(503, "temporarily_unavailable", "The storage connection is closed.")
		// These are failures of this service's own intent or configuration, not of the
		// provider and not of the request the client sent.
		case "invalid_intent", "invalid_config":
			return fc.NewProblem(500, "internal_error", "The storage request was rejected as invalid.")
		}
		return fc.NewProblem(502, "storage_failure", "The storage request could not be completed.")
	}
	var old *httpx.Error
	if errors.As(err, &old) {
		code := "invalid_request"
		switch old.Status {
		case 401:
			code = "unauthenticated"
		case 403:
			code = "forbidden"
		case 404:
			code = "not_found"
		case 429:
			code = "rate_limited"
		default:
			if old.Status >= 500 {
				code = "temporarily_unavailable"
			}
		}
		return fc.NewProblem(old.Status, code, http.StatusText(old.Status))
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fc.NewProblem(504, "storage_timeout", "The storage request timed out.")
	}
	if errors.Is(err, context.Canceled) {
		return fc.NewProblem(503, "temporarily_unavailable", "The request was cancelled.")
	}
	slog.Error("file API failure", "error", err)
	return fc.NewProblem(500, "internal_error", "The request could not be completed.")
}

// rootCause returns the deepest wrapped error, which is where a driver or provider
// message survives. Logs may name it; public problem bodies never do.
func rootCause(err error) string {
	deepest := err
	for {
		next := errors.Unwrap(deepest)
		if next == nil {
			if deepest == err {
				return ""
			}
			return deepest.Error()
		}
		deepest = next
	}
}

// logFilesFailure records one line per failed file request. The request id a client
// sees, the public problem code and the private cause are logged together so a
// reported failure can be traced back to the exact driver error.
func (s *Server) logFilesFailure(w http.ResponseWriter, r *http.Request, audit *filechanges.Audit, err error) {
	problem := filesProblem(err)
	attrs := []any{
		"request_id", w.Header().Get("X-Request-Id"),
		"method", r.Method,
		"path", r.URL.Path,
		"status", problem.Status,
		"code", problem.Code,
		"error", err.Error(),
	}
	if cause := rootCause(err); cause != "" {
		attrs = append(attrs, "cause", cause)
	}
	if audit != nil {
		attrs = append(attrs, "owner_id", audit.OwnerID, "action", audit.Action)
	}
	switch {
	case problem.Status >= 500:
		slog.Error("file request failed", attrs...)
	// Routine refusals are already visible in the request log; anything else is
	// worth a warning because it usually means a client sent something unexpected.
	case problem.Status >= 400 && problem.Status != 401 && problem.Status != 403 && problem.Status != 404:
		slog.Warn("file request rejected", attrs...)
	}
}

type filesResponseWriter struct {
	http.ResponseWriter
	started      bool
	status       int
	bytesWritten int64
	writeErr     error
}

func (w *filesResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *filesResponseWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	// Informational responses do not commit the final response status.
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.started = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *filesResponseWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytesWritten += int64(n)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}
func (w *filesResponseWriter) Flush() {
	if !w.started {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (s *Server) fileWrap(fn fileHandler) http.HandlerFunc {
	return s.fileWrapWithAudit(fn, s.recordFilesAudit)
}

func (s *Server) fileWrapWithAudit(fn fileHandler, persist filesAuditRecorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clearReadDeadline := partReadIdle(w, r, uploadPartReadIdleTimeout)
		defer clearReadDeadline()
		// No request here gets a wall clock, read or mutation. A mutation's work is
		// proportional to the tree it captures, so a timer fires exactly on the folders
		// large enough to need the time: a blanket 30s cutoff aborted a 776-folder
		// recursive delete mid-capture and reported "backend_error" with no cause. A read's
		// work is proportional to its page, and the indexed routes never touch storage at
		// all, so the same clock could only ever fire on a slow query and then blame the
		// storage. The request context is the only deadline: the client's disconnect
		// cancels it, a native call carries its own per-request provider budget, and a
		// single unbounded statement is bounded where it runs (for example the snapshot's
		// statement_timeout).
		response := &filesResponseWriter{ResponseWriter: w}
		w = response
		var audit *filechanges.Audit
		finishedAudit := false
		defer func() {
			if recovered := recover(); recovered != nil {
				if !finishedAudit {
					filesAuditCompletion(r.Context(), persist, audit, response, fc.NewProblem(500, "handler_interrupted", "The request was interrupted."))
				}
				if response.started {
					panic(http.ErrAbortHandler)
				}
				panic(recovered)
			}
		}()
		var id [12]byte
		_, _ = rand.Read(id[:])
		w.Header().Set("X-Request-Id", "req_"+hex.EncodeToString(id[:]))
		run := func() error {
			verifier := s.filesAuth
			if verifier == nil {
				verifier = s.auth
			}
			// Selection precedes the verifier even for injected implementations: an empty
			// explicit header cannot regain cookie authority.
			if _, err := fc.SelectCredential(r.Header); err != nil {
				return err
			}
			if verifier == nil {
				return fc.NewProblem(503, "temporarily_unavailable", "Authentication is not configured.")
			}
			principal, source, err := verifier.AuthenticateFiles(r.Context(), r)
			if err != nil {
				return err
			}
			if principal == nil || principal.User == nil {
				return fc.NewProblem(401, "unauthenticated", "A valid credential is required.")
			}
			audit = filesAuditAttempt(r, principal, w.Header().Get("X-Request-Id"))
			r = r.WithContext(filechanges.WithActor(r.Context(), audit.Actor, audit.RequestID))
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && source == fc.CookieCredential {
				accepted := false
				for _, origin := range s.cfg.FileCookieOrigins {
					policy, e := fc.NewCookieMutationPolicy(origin)
					if e == nil && policy.Check(r.Method, r.Header, source, func(token string) bool { return verifier.ValidateFileCSRF(r, token) }) == nil {
						accepted = true
						break
					}
				}
				if !accepted {
					return fc.NewProblem(403, "forbidden", "An approved Origin and valid session-bound CSRF token are required.")
				}
			}
			if s.limiter != nil {
				if err = s.limitAPI(w, r, "files", "u:"+principal.User.ID, filesRatePerMinute); err != nil {
					return err
				}
			}
			if s.trees == nil {
				return fc.NewProblem(503, "temporarily_unavailable", "File storage is not configured.")
			}
			driver, err := s.trees.Open(r.Context(), principal.User.ID)
			if err != nil {
				return err
			}
			defer driver.Close()
			scope := fc.Path("/")
			if principal.Token != nil {
				scope, err = fc.FromRelative(principal.Token.Prefix)
				if err != nil {
					return fc.NewProblem(401, "unauthenticated", "The credential scope is invalid.")
				}
			}
			rootIdentity := principal.User.ID + ":" + s.cfg.BackendName() + ":" + s.cfg.Storage.ConnectionID + fmt.Sprint(s.cfg.Storage.Generation)
			if provider, ok := s.trees.(interface {
				Binding(string) (storage.Config, error)
			}); ok {
				binding, e := provider.Binding(principal.User.ID)
				if e != nil {
					return e
				}
				rootIdentity = fmt.Sprintf("%s:%s:%d:%s:%s", binding.OwnerID, binding.ConnectionID, binding.Generation, binding.ContentRoot, binding.SystemRoot)
			}
			// Start concealment before any read: the accepted delete is already final
			// for the user, only its native purge remains.
			driver, err = s.filesDeleteOverlay(r.Context(), principal.User.ID, rootIdentity, driver)
			if err != nil {
				return err
			}
			req := &fileRequest{Principal: principal, Source: source, Driver: driver, Scope: scope, ContentNamespace: rootIdentity, Namespace: rootIdentity + ":" + filePrincipalID(principal) + ":" + string(scope)}
			if principal.Token != nil {
				req.Namespace += fmt.Sprintf(":%t:%t:%t", principal.Token.CanRead, principal.Token.CanWrite, principal.Token.CanEvents)
			}
			if source == fc.CookieCredential {
				req.CSRFToken = verifier.FileCSRFToken(r)
			}
			if _, err := persist(r.Context(), audit); err != nil {
				filesAuditFailure(audit, "admission", err)
				if principal.Token != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
					return fc.NewProblem(503, "audit_unavailable", "The access audit could not be recorded.")
				}
			}
			return fn(w, r, req)
		}
		err := run()
		if err == nil {
			err = response.writeErr
		}
		if audit == nil && err != nil {
			slog.Warn("file authentication rejected", "request_id", w.Header().Get("X-Request-Id"), "status", filesProblem(err).Status)
		}
		filesAuditCompletion(r.Context(), persist, audit, response, err)
		finishedAudit = true
		if err != nil {
			if response.started {
				panic(http.ErrAbortHandler)
			}
			s.logFilesFailure(w, r, audit, err)
			writeFilesProblem(w, r, err)
		}
	}
}

var pendingFileWorkflow = regexp.MustCompile(`^/api/files/(uploads/[^/]+(/parts(/[0-9]+|/sign)?|/commit)?|operations/[^/]+(/cancel)?)$`)

func (s *Server) filesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes := map[string]fileHandler{}
		pending := func(http.ResponseWriter, *http.Request, *fileRequest) error {
			return fc.NewProblem(501, "not_implemented", "This file operation is not available yet.")
		}
		switch r.URL.Path {
		case fc.Base:
			routes["GET"] = s.handleFilesMetadata
			routes["POST"] = s.handleFilesMkdir
			routes["DELETE"] = s.handleFilesDelete
		case fc.Base + "/list":
			routes["GET"] = s.handleFilesList
		case fc.Base + "/search":
			routes["GET"] = s.handleFilesSearch
		case fc.Base + "/me":
			routes["GET"] = s.handleFilesMe
		case fc.Base + "/capabilities":
			routes["GET"] = s.handleFilesCapabilities
		case fc.Base + "/content":
			routes["GET"] = s.handleFilesContent
			routes["HEAD"] = s.handleFilesContent
			routes["PUT"] = s.handleFilesPut
		case fc.Base + "/downloads":
			routes["POST"] = s.handleFilesDownloads
		case fc.Base + "/move":
			routes["POST"] = s.handleFilesMove
		case fc.Base + "/copy":
			routes["POST"] = s.handleFilesCopy
		case fc.Base + "/thumbnail":
			routes["GET"] = pending
		case fc.Base + "/sync":
			routes["POST"] = s.handleFilesSync
		case fc.Base + "/changes":
			routes["GET"] = s.handleFilesChanges
		case fc.Base + "/subscriptions":
			routes["POST"] = s.handleFilesCreateSubscription
			routes["GET"] = s.handleFilesListSubscriptions
		case fc.Base + "/change-snapshots":
			routes["POST"] = s.handleFilesCreateChangeSnapshot
		case fc.Base + "/uploads":
			routes["GET"] = s.handleFilesListUploads
			routes["POST"] = s.handleFilesCreateUpload
		case fc.Base + "/uploads/batch":
			routes["POST"] = s.handleFilesCreateUploadBatch
		case fc.Base + "/uploads/commit/batch":
			routes["POST"] = s.handleFilesCommitUploadBatch
		default:
			if strings.HasPrefix(r.URL.Path, fc.Base+"/subscriptions/") {
				suffix := strings.TrimPrefix(r.URL.Path, fc.Base+"/subscriptions/")
				parts := strings.Split(suffix, "/")
				if len(parts) > 0 {
					r.SetPathValue("id", parts[0])
				}
				if len(parts) == 1 {
					routes["GET"] = s.handleFilesGetSubscription
					routes["DELETE"] = s.handleFilesDeleteSubscription
				}
				if len(parts) == 2 && parts[1] == "renew" {
					routes["POST"] = s.handleFilesRenewSubscription
				}
				if len(parts) == 2 && parts[1] == "events" {
					routes["GET"] = s.handleFilesSubscriptionEvents
				}
			} else if strings.HasPrefix(r.URL.Path, fc.Base+"/change-snapshots/") && !strings.Contains(strings.TrimPrefix(r.URL.Path, fc.Base+"/change-snapshots/"), "/") {
				r.SetPathValue("id", strings.TrimPrefix(r.URL.Path, fc.Base+"/change-snapshots/"))
				routes["GET"] = s.handleFilesGetChangeSnapshot
				routes["DELETE"] = s.handleFilesDeleteChangeSnapshot
			}
			if pendingFileWorkflow.MatchString(r.URL.Path) {
				switch {
				case strings.HasPrefix(r.URL.Path, fc.Base+"/operations/"):
					if strings.HasSuffix(r.URL.Path, "/cancel") {
						routes["POST"] = s.handleFilesCancelOperation
					} else {
						routes["GET"] = s.handleFilesGetOperation
					}
				case strings.HasSuffix(r.URL.Path, "/commit"):
					routes["POST"] = s.handleFilesCommitUpload
				case strings.HasSuffix(r.URL.Path, "/parts/sign"):
					routes["POST"] = s.handleFilesSignUploadParts
				case strings.Contains(r.URL.Path, "/parts/"):
					routes["PUT"] = s.handleFilesUploadPart
				case strings.HasSuffix(r.URL.Path, "/parts"):
					routes["GET"] = s.handleFilesUploadParts
				case strings.Contains(r.URL.Path, "/uploads/"):
					routes["GET"] = s.handleFilesGetUpload
					routes["DELETE"] = s.handleFilesAbortUpload
				default:
					routes["GET"] = pending
				}
			}
		}
		fn := routes[r.Method]
		if fn == nil {
			if len(routes) == 0 {
				writeFilesProblem(w, r, fc.NewProblem(404, "not_found", "The file API resource was not found."))
				return
			}
			allow := make([]string, 0, len(routes))
			for method := range routes {
				allow = append(allow, method)
			}
			sort.Strings(allow)
			w.Header().Set("Allow", strings.Join(allow, ", "))
			writeFilesProblem(w, r, fc.NewProblem(405, "method_not_allowed", "The method is not supported for this resource."))
			return
		}
		s.fileWrap(fn)(w, r)
	})
}
