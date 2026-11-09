package cos

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTraceTransportKeepsTheResponseAndTheError(t *testing.T) {
	calls := 0
	traced := &traceTransport{next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/fail" {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	request, _ := http.NewRequest("PUT", "https://bucket.example/dir/file", nil)
	response, err := traced.RoundTrip(request)
	if err != nil || response.StatusCode != 204 {
		t.Fatalf("traced response %v %v", response, err)
	}
	if _, err = traced.RoundTrip(request.Clone(request.Context())); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("trace dropped a request: %d", calls)
	}
	bad, _ := http.NewRequest("PUT", "https://bucket.example/fail", nil)
	if _, err = traced.RoundTrip(bad); err == nil {
		t.Fatal("trace swallowed the transport error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
