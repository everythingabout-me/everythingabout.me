package cos

import (
	"context"
	"errors"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/tencent"
	sdk "github.com/tencentyun/cos-go-sdk-v5"
)

func mutationFixture(t *testing.T, handler http.Handler) (*Client, objectmutation.Backend, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	native := sdk.NewClient(&sdk.BaseURL{BucketURL: u}, &http.Client{Transport: &sdk.AuthorizationTransport{SecretID: "fixture-id", SecretKey: "fixture-secret", SessionToken: "fixture-token", Transport: transport}})
	c := &Client{bucket: "fixture-123456", region: "ap-test", client: native, transport: transport, cred: tencent.Credentials{SecretID: "fixture-id", SecretKey: "fixture-secret"}}
	b, err := c.MutationBackend()
	if err != nil {
		t.Fatal(err)
	}
	return c, b, server
}
func TestMutationNativeWireAndVersionSelection(t *testing.T) {
	key := "drive/alice/ a+%2F?#中文.txt "
	var puts, deletes, gets atomic.Int32
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Authorization"), "q-sign-algorithm=sha1") || r.Header.Get("X-Cos-Security-Token") != "fixture-token" {
			t.Errorf("COS authentication/session missing: %v", r.Header)
		}
		if r.URL.Path != "/"+key {
			t.Errorf("native key rewritten: %q", r.URL.Path)
		}
		w.Header().Set("X-Cos-Version-Id", "version+%?old")
		w.Header().Set("ETag", `"opaque-native-tag"`)
		switch r.Method {
		case "HEAD":
			w.Header().Set("Content-Length", "4")
			w.Header().Set("Content-Type", "application/custom")
			w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 01:00:00 GMT")
			w.Header().Set("X-Cos-Meta-Eam-intent", "nonce")
			w.WriteHeader(200)
		case "GET":
			gets.Add(1)
			if r.URL.Query().Has("versionId") || r.Header.Get("If-Match") != `"opaque-native-tag"` {
				t.Errorf("current read lost ETag condition: %s %q", r.URL.RawQuery, r.Header.Get("If-Match"))
			}
			w.Header().Set("Content-Length", "4")
			io.WriteString(w, "data")
		case "PUT":
			w.Header().Set("X-Cos-Hash-Crc64ecma", strconv.FormatUint(crc64.Checksum([]byte("data"), crc64.MakeTable(crc64.ECMA)), 10))
			puts.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "data" || r.ContentLength != 4 || len(r.TransferEncoding) != 0 {
				t.Errorf("native bytes %q len%d transfer%v err%v", body, r.ContentLength, r.TransferEncoding, err)
			}
			if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Cos-Forbid-Overwrite") != "true" || r.Header.Get("X-Cos-Meta-Eam-intent") != "nonce" || r.Header.Get("Content-Type") != "application/custom" {
				t.Errorf("native PUT headers lost: %v", r.Header)
			}
			w.WriteHeader(200)
		case "DELETE":
			deletes.Add(1)
			if r.Header.Get("If-Match") != `"opaque-native-tag"` || r.URL.Query().Has("versionId") {
				t.Error("delete changed conditional current-key semantics")
			}
			w.Header().Set("X-Cos-Version-Id", "delete-marker")
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
		t.Fatalf("bad HEAD %+v", head)
	}
	body, err := b.Open(ctx, key, head.ETag)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil || string(data) != "data" {
		t.Fatal("wrong retained bytes", string(data), err)
	}
	receipt, err := b.Put(ctx, objectmutation.Put{Key: key, Size: 4, Body: strings.NewReader("data"), MediaType: "application/custom", Absent: true, Metadata: map[string]string{"eam-intent": "nonce"}})
	if err != nil {
		t.Fatalf("%v cause=%v", err, errors.Unwrap(err))
	}
	if receipt.VersionID != head.VersionID || receipt.Size != 4 || receipt.ETag != head.ETag || receipt.Metadata["eam-intent"] != "nonce" {
		t.Fatalf("bad PUT receipt %+v", receipt)
	}
	marker, err := b.Delete(ctx, key, head.ETag)
	if err != nil || marker.VersionID != "delete-marker" {
		t.Fatalf("bad DELETE %+v %v", marker, err)
	}
	if puts.Load() != 1 || deletes.Load() != 1 || gets.Load() != 1 {
		t.Fatal("native operation repeated")
	}
}
func TestMutationPutConditionalNotModifiedIsPreconditionFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		absent bool
		etag   string
		want   string
	}{
		{"create_not_modified", http.StatusNotModified, true, "", "precondition_failed"},
		{"create_conflict", http.StatusConflict, true, "", "outcome_unknown"},
		{"replace_not_modified", http.StatusNotModified, false, `"old"`, "outcome_unknown"},
		{"unconditional_not_modified", http.StatusNotModified, false, "", "outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			_, backend, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPut || r.Header.Get("If-Match") != tc.etag {
					t.Error("unexpected PUT method or replacement condition")
				}
				if tc.absent {
					if r.Header.Get("If-None-Match") != "*" || r.Header.Get("X-Cos-Forbid-Overwrite") != "true" {
						t.Error("create-only native headers missing")
					}
				} else if r.Header.Get("If-None-Match") != "" || r.Header.Get("X-Cos-Forbid-Overwrite") != "" {
					t.Error("create-only native headers added to another PUT")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(tc.status)
			}))
			_, err := backend.Put(context.Background(), objectmutation.Put{Key: "occupied", Size: 4, Body: strings.NewReader("data"), Absent: tc.absent, ExpectedETag: tc.etag})
			if !storage.IsCode(err, tc.want) || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v; want one attempt and %s", calls.Load(), err, tc.want)
			}
		})
	}
}

func TestMutationSingleAttemptsAndUnchangedReadClient(t *testing.T) {
	for _, method := range []string{"PUT", "DELETE"} {
		for _, status := range []int{304, 412, 409, 500, 503} {
			t.Run(method+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				c, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.WriteHeader(status)
					io.WriteString(w, `<Error><Code>Failure</Code><Message>private provider secret</Message></Error>`)
				}))
				var err error
				if method == "PUT" {
					_, err = b.Put(context.Background(), objectmutation.Put{Key: "a", Size: 4, Body: strings.NewReader("data"), ExpectedETag: `"old"`})
				} else {
					_, err = b.Delete(context.Background(), "a", `"old"`)
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
				if c.client.Conf.RetryOpt.Count != 3 {
					t.Fatal("read client retry policy changed")
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
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); w.WriteHeader(200) }))
	defer target.Close()
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	_, err := b.Put(context.Background(), objectmutation.Put{Key: "a", Size: 4, Body: strings.NewReader("data")})
	if !storage.IsCode(err, "outcome_unknown") || targetCalls.Load() != 0 {
		t.Fatal("native redirect followed", err)
	}
}
func TestMutationPinnedTruncationConditionsAndCancellation(t *testing.T) {
	var calls atomic.Int32
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.Method {
		case "GET":
			if r.Header.Get("If-Match") != `"retained"` {
				t.Error("read condition lost")
			}
			w.Header().Set("ETag", `"retained"`)
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
	body, err := b.Open(context.Background(), "a", `"retained"`)
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

func TestMutationReadsAndZeroLengthAreExact(t *testing.T) {
	_, b, _ := mutationFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			w.Header().Set("Content-Length", "-1")
			w.WriteHeader(200)
		case "GET":
			w.Header().Set("X-Cos-Version-Id", "wrong")
			w.Header().Set("Content-Length", "4")
			io.WriteString(w, "data")
		case "PUT":
			if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
				t.Errorf("zero request len%d encoding%v", r.ContentLength, r.TransferEncoding)
			}
			w.Header().Set("X-Cos-Version-Id", "zero")
			w.WriteHeader(200)
		}
	}))
	if _, err := b.Head(context.Background(), "a"); err == nil {
		t.Fatal("invalid HEAD size accepted")
	}
	if _, err := b.Open(context.Background(), "a", "required"); !storage.IsCode(err, "changed") {
		t.Fatal("ignored version accepted", err)
	}
	if _, err := b.Open(context.Background(), "a", ""); !storage.IsCode(err, "unsupported") {
		t.Fatal("unversioned read accepted", err)
	}
	if _, err := b.Put(context.Background(), objectmutation.Put{Key: "a", Body: strings.NewReader(""), Size: 0}); err != nil {
		t.Fatal(err)
	}
}
