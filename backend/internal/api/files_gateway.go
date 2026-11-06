package api

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
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

// fileIngressHandler rejects misrouted byte requests without reading their bodies.
// EdgeOne must route the reserved transfer prefix directly to COS; the app never
// falls back to receiving signed data or interpreting COS signatures as app auth.
func (s *Server) fileIngressHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		transfer := r.URL.Path == fc.Base+"/transfer" || strings.HasPrefix(r.URL.Path, fc.Base+"/transfer/")
		if transfer {
			writeFilesProblem(w, r, fc.NewProblem(503, "edgeone_route_required", "The signed transfer route must be forwarded by EdgeOne directly to COS."))
			return
		}
		next.ServeHTTP(w, r)
	})
}
