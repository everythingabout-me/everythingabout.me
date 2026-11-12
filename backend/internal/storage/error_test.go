package storage

import "testing"

func TestProviderErrorDiagnostic(t *testing.T) {
	err, ok := NewProviderError("backend_error", "trip/a.txt", 503, "SlowDown", "req-42", nil).(*Error)
	if !ok {
		t.Fatal("provider error is not a storage error")
	}
	if got, want := err.Provider.Diagnostic(), "HTTP 503, SlowDown, request req-42"; got != want {
		t.Fatalf("diagnostic %q, want %q", got, want)
	}
	if got, want := err.Error(), `storage backend_error: trip/a.txt (HTTP 503, SlowDown, request req-42)`; got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
	// A partial answer still names the parts that arrived, never an invented one.
	partial := &ProviderError{Status: 429}
	if got, want := partial.Diagnostic(), "HTTP 429"; got != want {
		t.Fatalf("partial diagnostic %q, want %q", got, want)
	}
	// No answer at all is spelled by the absence of the facts, not by a placeholder.
	if got := NewError("backend_error", "", nil).(*Error).Provider.Diagnostic(); got != "" {
		t.Fatalf("plain error invented provider facts %q", got)
	}
	if got := (&Error{Code: "backend_error"}).Error(); got != "storage backend_error" {
		t.Fatalf("message without facts %q", got)
	}
}
