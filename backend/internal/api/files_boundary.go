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
	"time"

	"everythingabout.me/backend/internal/auth"
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
		}
		return fc.NewProblem(502, "storage_failure", "The storage read could not be completed.")
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

type filesResponseWriter struct {
	http.ResponseWriter
	started bool
}

func (w *filesResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *filesResponseWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *filesResponseWriter) Write(data []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(data)
}
func (w *filesResponseWriter) Flush() {
	if !w.started {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (s *Server) fileWrap(fn fileHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != fc.Base+"/content" && !(r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/uploads/") && strings.Contains(r.URL.Path, "/parts/")) {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			r = r.WithContext(ctx)
		}
		response := &filesResponseWriter{ResponseWriter: w}
		w = response
		defer func() {
			if recovered := recover(); recovered != nil {
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
				if err = s.limitAPI(w, r); err != nil {
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
			driver, err = s.filesDeleteOverlay(r.Context(), principal.User.ID, driver)
			if err != nil {
				return err
			}
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
			req := &fileRequest{Principal: principal, Source: source, Driver: driver, Scope: scope, ContentNamespace: rootIdentity, Namespace: rootIdentity + ":" + filePrincipalID(principal) + ":" + string(scope)}
			if principal.Token != nil {
				req.Namespace += fmt.Sprintf(":%t:%t:%t", principal.Token.CanRead, principal.Token.CanWrite, principal.Token.CanEvents)
			}
			if source == fc.CookieCredential {
				req.CSRFToken = verifier.FileCSRFToken(r)
			}
			return fn(w, r, req)
		}
		if err := run(); err != nil {
			if response.started {
				panic(http.ErrAbortHandler)
			}
			writeFilesProblem(w, r, err)
		}
	}
}

var pendingFileWorkflow = regexp.MustCompile(`^/api/files/(uploads/[^/]+(/parts(/[0-9]+)?|/commit)?|operations/[^/]+(/cancel)?)$`)

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
		case fc.Base + "/move":
			routes["POST"] = s.handleFilesMove
		case fc.Base + "/copy":
			routes["POST"] = s.handleFilesCopy
		case fc.Base + "/thumbnail":
			routes["GET"] = pending
		case fc.Base + "/uploads":
			routes["GET"] = s.handleFilesListUploads
			routes["POST"] = s.handleFilesCreateUpload
		default:
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
