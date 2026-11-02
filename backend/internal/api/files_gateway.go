package api

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
)

const uploadPartReadIdleTimeout = 60 * time.Second

var uploadGatewayPartPath = regexp.MustCompile(`^/api/files/uploads/[^/]+/parts/[0-9]+$`)

// partReadIdle applies to each network read, not the entire upload. Clear the
// connection deadline after the handler so keep-alive control requests can reuse it.
func partReadIdle(w http.ResponseWriter, r *http.Request, idle time.Duration) func() {
	if r.Body == nil || idle <= 0 || r.Method != http.MethodPut || !uploadGatewayPartPath.MatchString(r.URL.Path) {
		return func() {}
	}
	controller := http.NewResponseController(w)
	r.Body = &readIdleBody{ReadCloser: r.Body, controller: controller, idle: idle}
	return func() { _ = controller.SetReadDeadline(time.Time{}) }
}

type readIdleBody struct {
	io.ReadCloser
	controller *http.ResponseController
	idle       time.Duration
}

func (b *readIdleBody) Read(p []byte) (int, error) {
	if err := b.controller.SetReadDeadline(time.Now().Add(b.idle)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return b.ReadCloser.Read(p)
}

// UploadGatewayHandler exposes byte ingress only. The public reverse proxy routes
// these same-origin requests here; sessions, commit, status and Operations remain
// on the application. It reuses the complete Cookie/Bearer/CSRF and owner binding
// boundary rather than trusting forwarding headers or an unsigned upload handle.
func (s *Server) UploadGatewayHandler() http.Handler {
	parts := s.fileWrap(s.handleFilesUploadPart)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !uploadGatewayPartPath.MatchString(r.URL.Path) {
			writeFilesProblem(w, r, fc.NewProblem(404, "not_found", "The upload gateway serves numbered part requests only."))
			return
		}
		if r.Method != http.MethodPut {
			w.Header().Set("Allow", http.MethodPut)
			writeFilesProblem(w, r, fc.NewProblem(405, "method_not_allowed", "The upload gateway accepts PUT part requests only."))
			return
		}
		parts.ServeHTTP(w, r)
	})
	return s.recoverer(s.logger(s.cors(handler)))
}

// fileIngressHandler rejects misrouted bytes before authentication, storage or
// body consumption. External mode must never silently fall back to app bandwidth.
func (s *Server) fileIngressHandler(next http.Handler) http.Handler {
	if s.cfg == nil || !s.cfg.ExternalUploadIngress() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && (r.URL.Path == fc.Base+"/content" || uploadGatewayPartPath.MatchString(r.URL.Path)) {
			writeFilesProblem(w, r, fc.NewProblem(503, "upload_gateway_required", "Create an upload session and send numbered parts through the configured same-origin upload gateway."))
			return
		}
		next.ServeHTTP(w, r)
	})
}
