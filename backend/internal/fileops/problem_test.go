package fileops

import (
	"errors"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// The failure detail is the only place a person learns why an operation stopped. It
// has to name the provider's own answer — status, code and request id — because
// "storage backend_error" alone is not something anyone can report, and it must never
// carry the provider's response body, which can echo the key back.
func TestProblemDetailNamesTheProviderAnswer(t *testing.T) {
	cause := E("storage_failure", storage.NewProviderError("backend_error", "", 503, "SlowDown", "req-42", errors.New("provider body text")))
	detail := problemDetail("storage_failure", cause, nil)
	for _, want := range []string{"storage backend_error", "HTTP 503", "SlowDown", "request req-42"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %q lacks %q", detail, want)
		}
	}
	if strings.Contains(detail, "provider body text") {
		t.Fatalf("provider response body reached the client: %q", detail)
	}
	// The chain keeps its ordering: the operation code first, then the storage answer.
	if !strings.HasPrefix(detail, "The file operation did not complete. Cause: storage backend_error (") {
		t.Fatalf("detail lost the cause chain: %q", detail)
	}
}

// A failure with no provider answer (a dropped connection) still says so as a storage
// code, and does not pretend a status arrived.
func TestProblemDetailWithoutProviderAnswer(t *testing.T) {
	detail := problemDetail("storage_failure", E("storage_failure", storage.NewError("backend_error", "", errors.New("connection reset by peer"))), nil)
	if !strings.Contains(detail, "storage backend_error") || strings.Contains(detail, "HTTP") {
		t.Fatalf("detail %q", detail)
	}
}
