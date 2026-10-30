package storageprobe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	cos "github.com/tencentyun/cos-go-sdk-v5"
)

type probeRoundTrip func(*http.Request) (*http.Response, error)

func (f probeRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixtureResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func fixtureSDK(t *testing.T, transport http.RoundTripper) *cos.Client {
	t.Helper()
	u, err := url.Parse("https://probe.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{Timeout: time.Second, Transport: transport})
	// The fixture does not implement COS CRC response headers. Disabling CRC here
	// is fixture-only; live probes keep SDK integrity checks enabled.
	client.Conf.EnableCRC = false
	return client
}

func TestCOSSDKNativeConditionHeaders(t *testing.T) {
	// This proves SDK wire serialization/retry behavior only. The HTTP fixture
	// deliberately rejects everything and cannot establish provider support.
	for _, operation := range []string{"get", "put", "copy", "delete", "complete"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			headers := http.Header{"If-Match": {"\"captured-version\""}, "If-None-Match": {"*"}, "X-Cos-Forbid-Overwrite": {"true"}}
			// Headers are combined only to exercise SDK serialization, never to
			// claim that this contradictory combination has provider semantics.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				for key, values := range headers {
					if r.Header.Get(key) != values[0] {
						t.Errorf("%s lost native header %s", operation, key)
					}
				}
				if operation == "copy" && r.Header.Get("X-Cos-Copy-Source-If-Match") != "\"source-version\"" {
					t.Error("copy lost distinct source condition")
				}
				w.WriteHeader(http.StatusPreconditionFailed)
				io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			}))
			defer srv.Close()
			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{Timeout: time.Second})
			ctx := context.Background()
			switch operation {
			case "get":
				_, err = client.Object.Get(ctx, "target", &cos.ObjectGetOptions{XOptionHeader: &headers})
			case "put":
				_, err = client.Object.Put(ctx, "target", strings.NewReader("tiny"), &cos.ObjectPutOptions{ObjectPutHeaderOptions: &cos.ObjectPutHeaderOptions{XOptionHeader: &headers}})
			case "copy":
				_, _, err = client.Object.Copy(ctx, "target", "probe.invalid/source", &cos.ObjectCopyOptions{ObjectCopyHeaderOptions: &cos.ObjectCopyHeaderOptions{XCosCopySourceIfMatch: "\"source-version\"", XOptionHeader: &headers}})
			case "delete":
				_, err = client.Object.Delete(ctx, "target", &cos.ObjectDeleteOptions{XOptionHeader: &headers})
			case "complete":
				_, _, err = client.Object.CompleteMultipartUpload(ctx, "target", "generated-id", &cos.CompleteMultipartUploadOptions{XOptionHeader: &headers, Parts: []cos.Object{{PartNumber: 1, ETag: "part"}}})
			}
			if err == nil {
				t.Fatal("native 412 must surface as an error")
			}
			want := 1
			if operation == "complete" {
				want = 3 // This SDK's Complete loop retries even definite 412s.
			}
			if calls != want {
				t.Fatalf("native %s attempts = %d, want %d", operation, calls, want)
			}
		})
	}
}

func TestCOSSDKRetryLimitsAndDelayedReplay(t *testing.T) {
	for _, seekable := range []bool{false, true} {
		t.Run(fmt.Sprintf("seekable=%t", seekable), func(t *testing.T) {
			calls, visible := 0, "baseline"
			client := fixtureSDK(t, probeRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				visible = string(body) // Native effect occurred before the reply was lost.
				if calls == 1 {
					visible = "newer" // Another writer commits before the old replay arrives.
				}
				return nil, io.ErrUnexpectedEOF
			}))
			if client.Conf.RetryOpt.Count != 3 || client.Conf.RetryOpt.Interval != 0 || client.Conf.RetryOpt.AutoSwitchHost {
				t.Fatalf("review required: SDK retry defaults changed: %+v", client.Conf.RetryOpt)
			}
			var reader io.Reader = strings.NewReader("stale")
			if !seekable {
				reader = struct{ io.Reader }{reader} // Like runtime's sizedReader: no Seek method.
			}
			_, err := client.Object.Put(context.Background(), "target", reader, nil)
			if err == nil {
				t.Fatal("lost responses must not become success")
			}
			wantCalls, wantVisible := 1, "newer"
			if seekable {
				wantCalls, wantVisible = 3, "stale"
			}
			if calls != wantCalls || visible != wantVisible {
				t.Fatalf("calls=%d visible=%q; want calls=%d visible=%q", calls, visible, wantCalls, wantVisible)
			}
			t.Log("fixture/model negative evidence: error cannot prove non-application; unfenced SDK replay may overwrite a newer generation")
		})
	}
}

func TestCOSSDKCompleteLostReplyIsNotRollback(t *testing.T) {
	calls, visible := 0, "old"
	client := fixtureSDK(t, probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			visible = "completed"
			return nil, io.ErrUnexpectedEOF
		}
		return fixtureResponse(r, http.StatusNotFound, "<Error><Code>NoSuchUpload</Code></Error>"), nil
	}))
	_, _, err := client.Object.CompleteMultipartUpload(context.Background(), "target", "known-generated-id", &cos.CompleteMultipartUploadOptions{Parts: []cos.Object{{PartNumber: 1, ETag: "part"}}})
	if err == nil || visible != "completed" || calls != 3 {
		t.Fatalf("lost complete reply: err=%v visible=%q attempts=%d", err, visible, calls)
	}
}

func TestCOSSDKHTTPTimeoutDoesNotFenceLateEffect(t *testing.T) {
	accepted, release, applied := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	visible := "baseline"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		close(accepted)
		<-release // Models a backend effect surviving caller cancellation.
		mu.Lock()
		visible = "late-stale-effect"
		mu.Unlock()
		close(applied)
		w.WriteHeader(204)
	}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{Timeout: 100 * time.Millisecond})
	client.Conf.RetryOpt.Count = 1
	result := make(chan error, 1)
	go func() {
		_, err := client.Object.Delete(context.Background(), "generated-target")
		result <- err
	}()
	<-accepted
	err = <-result
	// Unblock the server even if a future SDK unexpectedly returns nil.
	mu.Lock()
	visible = "newer-generation"
	mu.Unlock()
	close(release)
	<-applied
	if err == nil {
		t.Fatal("timeout unexpectedly returned success")
	}
	mu.Lock()
	defer mu.Unlock()
	if visible != "late-stale-effect" {
		t.Fatalf("expected late unfenced server effect, got %q", visible)
	}
	t.Log("real HTTP timeout plus modeled backend delay: client timeout is no terminal-effect/quiescence proof")
}

func TestCOSSDKCompleteWithHeadCannotAttributeAnEffect(t *testing.T) {
	posts, heads := 0, 0
	client := fixtureSDK(t, probeRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodHead {
			heads++
			response := fixtureResponse(r, 200, "")
			response.Header.Set("ETag", "\"unrelated-writer\"")
			return response, nil
		}
		posts++
		return fixtureResponse(r, 404, "<Error><Code>NoSuchUpload</Code></Error>"), nil
	}))
	result, _, err := client.Object.CompleteMultipartUploadWithHead(context.Background(), "target", "never-applied-id", &cos.CompleteMultipartUploadOptions{})
	if err != nil || result.ETag != "\"unrelated-writer\"" || posts != 2 || heads != 1 {
		t.Fatalf("HEAD fallback model changed: result=%+v err=%v posts=%d heads=%d", result, err, posts, heads)
	}
	t.Log("negative SDK fixture evidence: HEAD existence fallback can report success for an unrelated existing object; checksum equality would still not fence older requests")
}
