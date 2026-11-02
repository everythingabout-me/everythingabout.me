package s3storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

func mutationFixture(t *testing.T, handler http.Handler) (*Factory, objectmutation.Backend, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f, err := NewFactory(Options{Endpoint: server.URL, Region: "test-region", Bucket: "test-bucket", PathStyle: true, AccessKeyID: "fixture-id", SecretAccessKey: "fixture-secret", SessionToken: "fixture-token"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.MutationBackend()
	if err != nil {
		t.Fatal(err)
	}
	return f, b, server
}
func TestMutationNativeWireAndVersionSelection(t *testing.T) {
	key := "drive/alice/ a+%2F?#中文.txt "
	var puts, deletes, gets atomic.Int32
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.Header.Get("X-Amz-Security-Token") != "fixture-token" {
			t.Error("explicit server authentication missing")
		}
		if r.URL.Path != "/test-bucket/"+key {
			t.Errorf("native key changed: %q", r.URL.Path)
		}
		w.Header().Set("X-Amz-Version-Id", "version+%?old")
		w.Header().Set("ETag", `"opaque-native-tag"`)
		switch r.Method {
		case "HEAD":
			w.Header().Set("Content-Length", "4")
			w.Header().Set("Content-Type", "application/custom")
			w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 01:00:00 GMT")
			w.Header().Set("X-Amz-Meta-Eam-intent", "nonce")
			w.WriteHeader(200)
		case "GET":
			gets.Add(1)
			if r.URL.Query().Get("versionId") != "version+%?old" {
				t.Errorf("native version changed: %q", r.URL.RawQuery)
			}
			w.Header().Set("Content-Length", "4")
			io.WriteString(w, "data")
		case "PUT":
			puts.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "data" || r.ContentLength != 4 {
				t.Errorf("body %q length%d err%v", body, r.ContentLength, err)
			}
			if len(r.TransferEncoding) != 0 {
				t.Error("known-length PUT became chunked")
			}
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Amz-Meta-Eam-intent") != "nonce" || r.Header.Get("Content-Type") != "application/custom" {
				t.Errorf("PUT headers lost: %v", r.Header)
			}
			w.WriteHeader(200)
		case "DELETE":
			deletes.Add(1)
			if r.Header.Get("If-Match") != `"opaque-native-tag"` || r.URL.Query().Has("versionId") {
				t.Error("delete changed condition/current-key semantics")
			}
			w.Header().Set("X-Amz-Version-Id", "delete-marker")
			w.Header().Set("X-Amz-Delete-Marker", "true")
			w.WriteHeader(204)
		default:
			t.Error("unexpected request", r.Method)
		}
	}))
	ctx := context.Background()
	head, err := b.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if head.Key != key || head.Size != 4 || head.ETag != `"opaque-native-tag"` || head.Metadata["eam-intent"] != "nonce" || head.ModifiedAt.IsZero() {
		t.Fatalf("bad native metadata %+v", head)
	}
	body, err := b.Open(ctx, key, head.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(data) != "data" {
		t.Fatal("version bytes", string(data), err)
	}
	input := objectmutation.Put{Key: key, Body: strings.NewReader("data"), Size: 4, MediaType: "application/custom", Metadata: map[string]string{"eam-intent": "nonce"}, Absent: true}
	result, err := b.Put(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != 4 || result.VersionID != head.VersionID || result.ETag != head.ETag || result.Metadata["eam-intent"] != "nonce" {
		t.Fatalf("bad PUT receipt %+v", result)
	}
	marker, err := b.Delete(ctx, key, head.ETag)
	if err != nil || marker.VersionID != "delete-marker" {
		t.Fatalf("bad delete receipt %+v %v", marker, err)
	}
	if puts.Load() != 1 || deletes.Load() != 1 || gets.Load() != 1 {
		t.Fatal("unexpected native mutation replay")
	}
}
func TestMutationRequestsAreSingleAttemptAndKeepReadPolicy(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE"} {
		for _, status := range []int{412, 409, 500, 503} {
			t.Run(method+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				f, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(status)
					io.WriteString(w, `<Error><Code>Failure</Code><Message>private provider secret</Message></Error>`)
				}))
				var err error
				if method == "PUT" {
					_, err = b.Put(context.Background(), objectmutation.Put{Key: "drive/alice/a", Body: strings.NewReader("data"), Size: 4, ExpectedETag: `"old"`})
				} else {
					_, err = b.Delete(context.Background(), "drive/alice/a", `"old"`)
				}
				want := "outcome_unknown"
				if status == 412 {
					want = "precondition_failed"
				}
				if !storage.IsCode(err, want) || calls.Load() != 1 {
					t.Fatalf("calls%d err%v want%s", calls.Load(), err, want)
				}
				if strings.Contains(err.Error(), "provider secret") {
					t.Fatal("provider message exposed")
				}
				if f.backend.client.Options().RetryMaxAttempts != 2 {
					t.Fatal("read retry configuration changed")
				}
			})
		}
	}
}
func TestMutationLostReplyAndRedirectRemainUnknown(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Body != nil {
					io.Copy(io.Discard, r.Body)
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				conn.Close()
			}))
			var err error
			if method == "PUT" {
				_, err = b.Put(context.Background(), objectmutation.Put{Key: "a", Size: 4, Body: strings.NewReader("data")})
			} else {
				_, err = b.Delete(context.Background(), "a", "")
			}
			if !storage.IsCode(err, "outcome_unknown") || calls.Load() != 1 {
				t.Fatalf("lost reply replayed calls%d err%v", calls.Load(), err)
			}
		})
	}
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	_, err := b.Put(context.Background(), objectmutation.Put{Key: "a", Size: 4, Body: strings.NewReader("data")})
	if !storage.IsCode(err, "outcome_unknown") || redirected.Load() != 0 {
		t.Fatal("mutation followed provider redirect", err)
	}
}
func TestMutationPinnedTruncationConditionsAndCancellation(t *testing.T) {
	var calls atomic.Int32
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.Method {
		case "GET":
			w.Header().Set("X-Amz-Version-Id", "retained")
			w.Header().Set("Content-Length", "8")
			io.WriteString(w, "tiny")
		case "PUT":
			if r.Header.Get("If-Match") != `"captured"` || r.Header.Get("If-None-Match") != "" {
				t.Error("replace condition lost")
			}
			w.WriteHeader(412)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
		case "HEAD":
			w.WriteHeader(404)
		}
	}))
	body, err := b.Open(context.Background(), "a", "retained")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(body)
	body.Close()
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("short version accepted: %v", err)
	}
	if _, err = b.Put(context.Background(), objectmutation.Put{Key: "a", Body: strings.NewReader("data"), Size: 4, ExpectedETag: `"captured"`}); !storage.IsCode(err, "precondition_failed") {
		t.Fatal(err)
	}
	if _, err = b.Head(context.Background(), "a"); !storage.IsCode(err, "not_found") {
		t.Fatal("missing HEAD not typed", err)
	}
	before := calls.Load()
	if _, err = b.Put(context.Background(), objectmutation.Put{Key: "a", Body: strings.NewReader("data"), Size: 4, Absent: true, ExpectedETag: `"conflict"`}); !storage.IsCode(err, "invalid_request") {
		t.Fatal("contradictory condition accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = b.Delete(ctx, "a", ""); !storage.IsCode(err, "outcome_unknown") {
		t.Fatal("cancelled mutation implied rollback", err)
	}
	if calls.Load() != before {
		t.Fatal("invalid or cancelled mutation reached provider")
	}
}

func TestMutationInvalidNativeReadsAndZeroBody(t *testing.T) {
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			w.Header().Set("Content-Length", "-1")
			w.WriteHeader(200)
		case "GET":
			w.Header().Set("X-Amz-Version-Id", "different")
			w.Header().Set("Content-Length", "4")
			io.WriteString(w, "data")
		case "PUT":
			if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
				t.Errorf("zero PUT not exact length: %d %v", r.ContentLength, r.TransferEncoding)
			}
			w.Header().Set("X-Amz-Version-Id", "zero")
			w.WriteHeader(200)
		}
	}))
	if _, err := b.Head(context.Background(), "a"); err == nil {
		t.Fatal("invalid native length accepted")
	}
	if _, err := b.Open(context.Background(), "a", "requested"); !storage.IsCode(err, "changed") {
		t.Fatal("ignored version accepted", err)
	}
	if _, err := b.Open(context.Background(), "a", ""); !storage.IsCode(err, "unsupported") {
		t.Fatal("unpinned read accepted", err)
	}
	if _, err := b.Put(context.Background(), objectmutation.Put{Key: "a", Body: strings.NewReader(""), Size: 0}); err != nil {
		t.Fatal(err)
	}
}
