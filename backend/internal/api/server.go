// Package api 提供 HTTP 接口和前端静态文件服务。
package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/emailcode"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/mail"
	"everythingabout.me/backend/internal/mover"
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
	mover   *mover.Service
	limiter *ratelimit.Limiter
	web     http.Handler
}

// New 组装依赖。web 是前端构建产物（SPA）的处理器，可以是 nil。
func New(cfg *config.Config, db *store.DB, objectStorage storage.Storage, web http.Handler) *Server {
	nodeService := nodes.New(db, objectStorage)
	return &Server{
		cfg:     cfg,
		db:      db,
		auth:    auth.New(db, cfg),
		codes:   emailcode.New(db),
		mail:    mail.New(cfg),
		storage: objectStorage,
		nodes:   nodeService,
		mover:   mover.New(db, objectStorage, nodeService),
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

	// 对外 API：路径就是功能本身，不带 /v1 或 /api 前缀。
	mux.HandleFunc("GET /list", s.api(s.handleAPIList))
	mux.HandleFunc("GET /me", s.api(s.handleAPIMe))
	mux.HandleFunc("GET /search", s.api(s.handleAPISearch))
	mux.HandleFunc("POST /move", s.api(s.handleAPIMove))
	mux.HandleFunc("GET /move/{id}", s.api(s.handleAPIMoveJob))
	mux.HandleFunc("POST /move/{id}/retry", s.api(s.handleAPIMoveRetry))
	mux.HandleFunc("POST /upload", s.api(s.handleAPIUpload))
	mux.HandleFunc("POST /upload/complete", s.api(s.handleAPIComplete))
	mux.HandleFunc("GET /files/{path...}", s.api(s.handleAPIGetFile))
	mux.HandleFunc("HEAD /files/{path...}", s.api(s.handleAPIHeadFile))
	mux.HandleFunc("PUT /files/{path...}", s.api(s.handleAPIPutFile))
	mux.HandleFunc("DELETE /files/{path...}", s.api(s.handleAPIDeleteFile))

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

	return s.recoverer(s.logger(s.cors(mux)))
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

// api 是对外 API 的包装：限流 + {"error","message"} 的错误体（不是网页那套 {ok,...}）。
func (s *Server) api(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

// limitAPI 按令牌（没有令牌就按 IP）做粗粒度限流，顺带给出 Retry-After。
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
		return httpx.TooManyRequests(fmt.Sprintf("请求太频繁，请 %d 秒后再试", retryAfter))
	}
	return nil
}

// isAPIPath 判断是否是对外 API 的路径，用于 CORS 和预检。
func isAPIPath(path string) bool {
	switch path {
	case "/list", "/me", "/move", "/search", "/files", "/upload", "/upload/complete":
		return true
	}
	return strings.HasPrefix(path, "/files/") || strings.HasPrefix(path, "/move/")
}

func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, PUT, DELETE, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match, If-None-Match")
	w.Header().Set("Access-Control-Expose-Headers", "ETag, Content-Length, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
}

// cors 只作用于对外 API：允许浏览器跨域带 Bearer 调用，并直接回预检。
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
