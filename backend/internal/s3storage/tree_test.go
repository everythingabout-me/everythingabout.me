package s3storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"everythingabout.me/backend/internal/objecttree/treetest"
	"everythingabout.me/backend/internal/storage"
)

func TestNativeTreeConformance(t *testing.T) {
	treetest.Run(t, func(server *httptest.Server) (storage.Factory, error) {
		original := server.Config.Handler
		server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
				t.Error("S3 SDK request lacks SigV4 authentication")
			}
			if r.Header.Get("X-Amz-Security-Token") != "test-session-token" {
				t.Error("S3 SDK request lost explicit session token")
			}
			if r.URL.Path != "/test-bucket" && !strings.HasPrefix(r.URL.Path, "/test-bucket/") {
				t.Errorf("path-style bucket missing: %q", r.URL.Path)
			}
			original.ServeHTTP(w, r)
		})
		return NewFactory(Options{Endpoint: server.URL, Region: "test-region-1", Bucket: "test-bucket", PathStyle: true, AccessKeyID: "test-id", SecretAccessKey: "test-secret", SessionToken: "test-session-token"})
	})
}
func TestSignedRequestsDoNotFollowRedirects(t *testing.T) {
	var received atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Add(1); w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	factory, err := NewFactory(Options{Endpoint: server.URL, Region: "test-region-1", Bucket: "test-bucket", PathStyle: true, AccessKeyID: "test-id", SecretAccessKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory.backend.Head(context.Background(), "drive/alice/file")
	if err == nil || received.Load() != 0 {
		t.Fatalf("signed request followed redirect: calls=%d err=%v", received.Load(), err)
	}
}

func TestFactoryOptionsAreExplicit(t *testing.T) {
	valid := Options{Endpoint: "https://s3.example.test", Region: "test-region-1", Bucket: "test-bucket", AccessKeyID: "test-id", SecretAccessKey: "test-secret"}
	for _, test := range []struct {
		name   string
		change func(*Options)
	}{
		{"endpoint", func(o *Options) { o.Endpoint = "" }},
		{"credentials_in_url", func(o *Options) { o.Endpoint = "https://id:secret@s3.example.test" }},
		{"query", func(o *Options) { o.Endpoint += "?secret=value" }},
		{"path", func(o *Options) { o.Endpoint += "/path" }},
		{"region", func(o *Options) { o.Region = "" }},
		{"bucket", func(o *Options) { o.Bucket = "" }},
		{"key", func(o *Options) { o.AccessKeyID = "" }},
		{"secret", func(o *Options) { o.SecretAccessKey = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			opts := valid
			test.change(&opts)
			_, err := NewFactory(opts)
			if !storage.IsCode(err, "invalid_config") {
				t.Fatal(err)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration error leaked secret")
			}
		})
	}
	factory, err := NewFactory(valid)
	if err != nil {
		t.Fatal(err)
	}
	if factory.backend.client.Options().UsePathStyle {
		t.Fatal("virtual-host style was overwritten")
	}
}
