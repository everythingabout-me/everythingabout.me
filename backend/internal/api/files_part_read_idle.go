package api

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// uploadPartReadIdleTimeout is per network read, not per part: it fires only when a part
// body stops delivering bytes entirely. A phone on a bad link can stall for most of a
// minute in TCP retransmission and still finish, and the cost of waiting is one held spool
// slot, so the bound sits above a normal carrier stall rather than at LAN patience.
const uploadPartReadIdleTimeout = 120 * time.Second

// partReadIdle applies to each network read, not the entire upload. Clear the connection
// deadline after the handler so keep-alive control requests can reuse it.
func partReadIdle(w http.ResponseWriter, r *http.Request, idle time.Duration) func() {
	if r.Body == nil || idle <= 0 || r.Method != http.MethodPut {
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
