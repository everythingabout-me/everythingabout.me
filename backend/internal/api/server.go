// Package api 提供 HTTP 接口和前端静态文件服务。
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/emailcode"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/mail"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/ratelimit"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
)

type Server struct {
	cfg     *config.Config
	db      *store.DB
	auth    *auth.Service
	codes   *emailcode.Service
	mail    *mail.Sender
	storage storage.Storage
	nodes   *nodes.Service
	limiter *ratelimit.Limiter
	web     http.Handler
}

// New 组装依赖。web 是前端构建产物（SPA）的处理器，可以是 nil。
func New(cfg *config.Config, db *store.DB, objectStorage storage.Storage, web http.Handler) *Server {
	return &Server{
		cfg:     cfg,
		db:      db,
		auth:    auth.New(db, cfg),
		codes:   emailcode.New(db),
		mail:    mail.New(cfg),
		storage: objectStorage,
		nodes:   nodes.New(db, objectStorage),
		limiter: ratelimit.New(),
		web:     web,
	}
}

type handlerFunc func(http.ResponseWriter, *http.Request) error

// Handler 返回完整的 http.Handler（接口 + SPA）。
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

	mux.HandleFunc("GET /api/fs/list", s.wrap(s.handleList))
	mux.HandleFunc("GET /api/fs/search", s.wrap(s.handleSearch))
	mux.HandleFunc("POST /api/fs/upload/sign", s.wrap(s.handleUploadSign))
	mux.HandleFunc("POST /api/fs/upload/confirm", s.wrap(s.handleUploadConfirm))
	mux.HandleFunc("GET /api/fs/download", s.handleDownload)
	mux.HandleFunc("POST /api/fs/mkdir", s.wrap(s.handleMkdir))
	mux.HandleFunc("POST /api/fs/rename", s.wrap(s.handleRename))
	mux.HandleFunc("POST /api/fs/delete", s.wrap(s.handleDelete))

	if s.web != nil {
		mux.Handle("/", s.web)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.Error(w, "前端还没有构建：在 web/ 运行 npm run build，或用 npm run dev 启动 Vite。", http.StatusServiceUnavailable)
				return
			}
			http.NotFound(w, r)
		})
	}

	return s.recoverer(s.logger(mux))
}

func (s *Server) ok(w http.ResponseWriter, data any) error {
	httpx.WriteOK(w, data)
	return nil
}

// wrap 把返回 error 的 handler 变成标准 http.HandlerFunc。
func (s *Server) wrap(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration", time.Since(started).Round(time.Millisecond).String(),
		)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("panic", "path", r.URL.Path, "panic", recovered)
				httpx.WriteError(w, httpx.Internal("服务器内部错误"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
