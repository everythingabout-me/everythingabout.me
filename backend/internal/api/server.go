// Package api serves the HTTP API and the frontend's static files.
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/browserupload"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/emailcode"
	"everythingabout.me/backend/internal/events"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/mail"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/ratelimit"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/timeline"
	"everythingabout.me/backend/internal/uploadindex"
	"everythingabout.me/backend/internal/uploadspool"
)

type OwnerStorageFactory interface {
	Open(context.Context, string) (storage.Driver, error)
}

type Server struct {
	browserUploads *browserupload.Service
	uploadIndex    UploadIndex
	trees          OwnerStorageFactory
	filesAuth      FilesAuthenticator
	filesIndex     FileReadIndex
	filesJournal   *fileops.Postgres
	uploadSpools   fileUploadSpoolFactory
	fileWorkerMu   sync.Mutex
	fileWorkers    *fileWorkers
	cfg            *config.Config
	db             *store.DB
	auth           *auth.Service
	codes          *emailcode.Service
	mail           *mail.Sender
	storage        storage.Storage
	nodes          *nodes.Service
	jobs           *jobs.Service
	eventsSvc      *events.Service
	timeline       *timeline.Service
	limiter        *ratelimit.Limiter
	web            http.Handler
}

// New wires dependencies. web is the handler for the frontend build (SPA) and may be nil.
func New(cfg *config.Config, db *store.DB, objectStorage storage.Storage, web http.Handler) *Server {
	nodeService := nodes.New(db, objectStorage)
	eventService := events.New(db, nodeService)
	var jobService *jobs.Service
	if cfg.BackendName() == "cos" && !cfg.Storage.CoordinatedWrites {
		jobService = jobs.New(db, objectStorage, nodeService)
		jobService.SetDeleteBinding(uploadIndexBinding(cfg))
	}
	server := &Server{
		cfg:        cfg,
		db:         db,
		auth:       auth.New(db, cfg),
		codes:      emailcode.New(db),
		mail:       mail.New(cfg),
		storage:    objectStorage,
		nodes:      nodeService,
		filesIndex: nodeService,
		jobs:       jobService,
		eventsSvc:  eventService,
		timeline:   timeline.New(db, objectStorage, cfg.Timeline, eventService),
		limiter:    ratelimit.New(),
		web:        web,
	}
	server.uploadSpools = func(binding storage.Config) (FileUploadSpool, error) {
		base := cfg.Storage.ObjectSpoolRoot
		if cfg.BackendName() == "local" {
			base = binding.SystemRoot
		}
		return uploadspool.New(base, binding)
	}
	if db != nil {
		server.filesJournal = fileops.NewPostgres(db.Pool())
	}
	if cfg.BackendName() == "cos" && !cfg.Storage.CoordinatedWrites {
		server.uploadIndex = uploadindex.New(uploadIndexBinding(cfg), uploadindex.NewJournal(db.Pool()), uploadindex.ObserverFunc(server.observeUploadedObject))
		if driver, ok := objectStorage.(browserupload.Driver); ok {
			server.browserUploads = browserupload.New(uploadIndexBinding(cfg), browserupload.NewJournal(db.Pool()), driver, server.indexBrowserUpload)
		}
	}
	return server
}

// SetStorageFactory wires the startup-owned factory; handlers never construct providers.
func (s *Server) SetStorageFactory(factory OwnerStorageFactory) { s.trees = factory }

// StartTimeline starts the background organizer. Run it after listen, so startup is not delayed.
func (s *Server) StartTimeline(ctx context.Context) {
	if s.cfg.BackendName() != "cos" || s.cfg.Storage.CoordinatedWrites {
		return
	}
	s.timeline.Start(ctx)
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

// Handler returns the full http.Handler (API + SPA).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.wrap(func(w http.ResponseWriter, r *http.Request) error {
		return s.ok(w, map[string]any{"status": "ok"})
	}))

	mux.HandleFunc("POST /api/auth/code", s.wrap(s.handleSendCode))
	mux.HandleFunc("POST /api/auth/verify", s.wrap(s.handleVerifyCode))
	mux.HandleFunc("POST /api/auth/login", s.wrap(s.handlePasswordLogin))
	mux.HandleFunc("POST /api/auth/logout", s.wrap(s.handleLogout))
	mux.HandleFunc("GET /api/auth/me", s.wrap(s.handleMe))
	mux.HandleFunc("POST /api/auth/password", s.wrap(s.handleSetPassword))
	mux.HandleFunc("DELETE /api/auth/password", s.wrap(s.handleRemovePassword))
	mux.HandleFunc("POST /api/auth/email/current-code", s.wrap(s.handleCurrentEmailCode))
	mux.HandleFunc("POST /api/auth/email/code", s.wrap(s.handleNewEmailCode))
	mux.HandleFunc("POST /api/auth/email", s.wrap(s.handleChangeEmail))
	mux.HandleFunc("GET /api/account", s.wrap(s.handleAccount))
	mux.HandleFunc("GET /api/audit", s.ownerAuditHandler())

	mux.HandleFunc("GET /api/save-links", s.wrap(s.handleListSaveLinks))
	mux.HandleFunc("POST /api/save-links", s.wrap(s.handleCreateSaveLink))
	mux.HandleFunc("DELETE /api/save-links/{id}", s.wrap(s.handleRevokeSaveLink))

	mux.HandleFunc("GET /api/connect/tokens", s.wrap(s.handleConnectList))
	mux.HandleFunc("POST /api/connect/tokens", s.wrap(s.handleConnectCreate))
	mux.HandleFunc("PATCH /api/connect/tokens/{id}", s.wrap(s.handleConnectUpdate))
	mux.HandleFunc("DELETE /api/connect/tokens/{id}", s.wrap(s.handleConnectRevoke))

	mux.HandleFunc("GET /api/timeline", s.wrap(s.handleTimeline))
	mux.HandleFunc("GET /api/timeline/status", s.wrap(s.handleTimelineStatus))
	mux.HandleFunc("POST /api/timeline/rebuild", s.wrap(s.handleTimelineRebuild))
	mux.HandleFunc("DELETE /api/events/{id}", s.wrap(s.handleDeleteEvent))

	// Retired routes are explicit so an SPA fallback cannot disguise a stale client.
	// No legacy file handler or signer is reachable through this router.
	for _, route := range []string{"/api/fs", "/api/fs/", "/capabilities", "/list", "/search", "/mkdir", "/move", "/move/", "/copy", "/copy/", "/delete", "/delete/", "/upload", "/upload/"} {
		mux.HandleFunc(route, retiredFileRoute)
	}

	// Account/Connect management and event authoring remain separate APIs.
	// The shared file resource family below owns /api/files exclusively.
	mux.HandleFunc("POST /events", s.api(s.handleAPICreateEvent))
	mux.HandleFunc("GET /events", s.api(s.handleAPIListEvents))
	mux.HandleFunc("GET /events/{id}", s.api(s.handleAPIGetEvent))
	mux.HandleFunc("DELETE /events/{id}", s.api(s.handleAPIDeleteEvent))

	mux.HandleFunc("GET /me", s.api(s.handleAPIMe))
	mux.HandleFunc("PATCH /me", s.api(s.handleAPIPatchMe))
	files := s.fileIngressHandler(s.filesHandler())
	mux.Handle("/api/files", files)
	mux.Handle("/api/files/", files)

	// A save link is an instruction page using the unified file API.
	// The sub-router registers GET only; ServeMux answers 405 for POST, with no business handler.
	saveMux := http.NewServeMux()
	saveMux.HandleFunc("GET /{code}", s.wrap(s.handleSaveLinkPage))
	mux.Handle("/s/", http.StripPrefix("/s", saveMux))

	if s.web != nil {
		mux.Handle("/", s.web)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.Error(w, "frontend is not built: run npm run build in web/, or npm run dev for Vite.", http.StatusServiceUnavailable)
				return
			}
			http.NotFound(w, r)
		})
	}

	return s.recoverer(s.logger(s.cors(mux)))
}

func retiredFileRoute(w http.ResponseWriter, r *http.Request) {
	writeFilesProblem(w, r, fc.NewProblem(http.StatusGone, "legacy_file_api_retired", "Use the unified /api/files API."))
}

func (s *Server) ok(w http.ResponseWriter, data any) error {
	httpx.WriteOK(w, data)
	return nil
}

// wrap turns an error-returning handler into a standard http.HandlerFunc.
func (s *Server) wrap(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.assertBackendRoute(r); err != nil {
			httpx.WriteError(w, err)
			return
		}
		if err := fn(w, r); err != nil {
			var apiErr *httpx.Error
			var pathErr *paths.Error
			if !errors.As(err, &apiErr) && !errors.As(err, &pathErr) {
				slog.Error("api", "method", r.Method, "path", r.URL.Path, "error", err)
			}
			httpx.WriteError(w, err)
		}
	}
}

// api wraps the public API: rate limit + {"error","message"} bodies (not the web {ok,...} shape).
func (s *Server) api(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.assertBackendRoute(r); err != nil {
			writeAPIError(w, err)
			return
		}
		if err := s.limitAPI(w, r); err != nil {
			writeAPIError(w, err)
			return
		}
		if err := fn(w, r); err != nil {
			writeAPIError(w, err)
		}
	}
}

const apiRatePerMinute = 600

// limitAPI does coarse rate limiting per token (per IP without one), plus a Retry-After.
func (s *Server) limitAPI(w http.ResponseWriter, r *http.Request) error {
	key := "ip:" + httpx.ClientIP(r)
	if header := strings.TrimSpace(r.Header.Get("Authorization")); strings.HasPrefix(header, "Bearer ") {
		if id, _, ok := auth.ParseToken(strings.TrimPrefix(header, "Bearer ")); ok {
			key = "tok:" + id
		}
	}
	allowed, retryAfter := s.limiter.Allow("api:"+key, apiRatePerMinute, time.Minute)
	w.Header().Set("X-RateLimit-Limit", fmt.Sprint(apiRatePerMinute))
	if !allowed {
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
		return httpx.TooManyRequests(fmt.Sprintf("too many requests; retry in %d seconds", retryAfter))
	}
	return nil
}

// isAPIPath reports whether a path belongs to the public API, for CORS and preflight.
func isAPIPath(path string) bool {
	switch path {
	case "/me", "/api/files", "/events":
		return true
	}
	for _, prefix := range []string{"/api/files/", "/events/", "/s/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, PATCH, DELETE, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers",
		"Authorization, Content-Type, If-Match, If-None-Match, Prefer")
	w.Header().Set("Access-Control-Expose-Headers",
		"ETag, Content-Length, Content-Type, Location, Retry-After, X-RateLimit-Limit")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

// cors applies to the public API only: browsers may call cross-origin with a Bearer token,
// and preflight is answered right here.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isFileAPIPath(r.URL.Path) {
			s.filesCORS(next).ServeHTTP(w, r)
			return
		}
		if isAPIPath(r.URL.Path) {
			setCORSHeaders(w)
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireUser(r *http.Request) (*auth.User, error) {
	return s.auth.RequireUser(r.Context(), r)
}

func (s *Server) currentUser(r *http.Request) (*auth.User, error) {
	return s.auth.SessionUser(r.Context(), r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(data)
}
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
func (r *statusRecorder) Flush() {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		// /s/<code> is a credential in the path: never write it to the log.
		path := r.URL.Path
		if strings.HasPrefix(path, "/s/") {
			path = "/s/<redacted>"
		}
		slog.Info("request",
			"method", r.Method,
			"path", path,
			"status", recorder.status,
			"duration", time.Since(started).Round(time.Millisecond).String(),
		)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				slog.Error("panic", "path", r.URL.Path, "panic", recovered)
				if isFileAPIPath(r.URL.Path) {
					writeFilesProblem(w, r, errors.New("file handler panic"))
					return
				}
				httpx.WriteError(w, httpx.Internal("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
