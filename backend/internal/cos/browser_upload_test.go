package cos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"everythingabout.me/backend/internal/browserupload"
	"everythingabout.me/backend/internal/paths"
	cos "github.com/tencentyun/cos-go-sdk-v5"
)

type browserCOSRequest struct {
	Method, Path, UploadID, Part, Marker, ContentType string
	Bytes                                             int
	Body                                              []byte
}

type browserCOSFixture struct {
	mu              sync.Mutex
	requests        []browserCOSRequest
	completeFailure bool
	headStatus      int
	headMarker      string
}

func newBrowserCOSFixture(t *testing.T) (*Client, *browserCOSFixture) {
	t.Helper()
	fixture := &browserCOSFixture{headStatus: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read fake provider request: %v", err)
			w.WriteHeader(500)
			return
		}
		request := browserCOSRequest{Method: r.Method, Path: r.URL.Path, UploadID: r.URL.Query().Get("uploadId"), Part: r.URL.Query().Get("partNumber"), Marker: r.Header.Get(browserMarker), ContentType: r.Header.Get("Content-Type"), Bytes: len(body)}
		if len(body) < 1<<20 {
			request.Body = body
		}
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, request)
		failure, headStatus, headMarker := fixture.completeFailure, fixture.headStatus, fixture.headMarker
		fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		w.Header().Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(body, crc64.MakeTable(crc64.ECMA)), 10))
		switch {
		case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
			_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>synthetic-123</Bucket><Key>target</Key><UploadId>native-session</UploadId></InitiateMultipartUploadResult>`)
		case r.Method == http.MethodPut && request.UploadID != "":
			w.Header().Set("ETag", `"part-`+request.Part+`"`)
			w.WriteHeader(200)
		case r.Method == http.MethodPost && request.UploadID != "":
			if failure {
				w.WriteHeader(500)
				_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>ambiguous completion</Message></Error>`)
				return
			}
			_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><Location>native</Location><Bucket>synthetic-123</Bucket><Key>target</Key><ETag>"final"</ETag></CompleteMultipartUploadResult>`)
		case r.Method == http.MethodHead:
			w.Header().Set(browserMarker, headMarker)
			w.WriteHeader(headStatus)
		case r.Method == http.MethodDelete && request.UploadID != "":
			w.WriteHeader(204)
		case r.Method == http.MethodPut:
			w.Header().Set("ETag", `"empty"`)
			w.WriteHeader(200)
		default:
			t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: base}, server.Client())
	publish := cos.NewClient(&cos.BaseURL{BucketURL: base}, server.Client())
	publish.Conf.RetryOpt.Count = 1
	return &Client{client: client, publishClient: publish}, fixture
}
func (fixture *browserCOSFixture) snapshot() []browserCOSRequest {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	return append([]browserCOSRequest(nil), fixture.requests...)
}
func browserTestSession(size int64) browserupload.Session {
	return browserupload.Session{ID: "bu_synthetic", OwnerID: "11111111-1111-1111-1111-111111111111", Path: "Health/ +%?#/steps.json", Size: size, Mime: "application/json"}
}
func stageBrowserFile(t *testing.T, client *Client, session browserupload.Session, body io.Reader) (json.RawMessage, []browserStage) {
	t.Helper()
	var checkpoints []browserStage
	data, err := client.Stage(context.Background(), session, body, func(_ context.Context, data json.RawMessage) error {
		var checkpoint browserStage
		if err := json.Unmarshal(data, &checkpoint); err != nil {
			return err
		}
		checkpoints = append(checkpoints, checkpoint)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return data, checkpoints
}

func TestBrowserCOSStageMultipartThenPublish(t *testing.T) {
	client, fixture := newBrowserCOSFixture(t)
	body := bytes.Repeat([]byte("a"), browserPartBytes+3)
	session := browserTestSession(int64(len(body)))
	data, checkpoints := stageBrowserFile(t, client, session, bytes.NewReader(body))
	if len(checkpoints) != 4 || checkpoints[0].UploadID != "native-session" || checkpoints[0].Ready || len(checkpoints[0].Parts) != 0 {
		t.Fatalf("allocation must be checkpointed before parts: %+v", checkpoints)
	}
	last := checkpoints[len(checkpoints)-1]
	digest := sha256.Sum256(body)
	if !last.Ready || last.SHA256 != hex.EncodeToString(digest[:]) || len(last.Parts) != 2 {
		t.Fatalf("wrong ready receipt: %+v", last)
	}
	requests := fixture.snapshot()
	if len(requests) != 3 {
		t.Fatalf("staging published or made unexpected requests: %+v", requests)
	}
	if requests[0].Method != "POST" || requests[0].Marker != session.ID || requests[0].ContentType != session.Mime {
		t.Fatalf("native init must carry attempt marker and MIME: %+v", requests[0])
	}
	for index, request := range requests[1:] {
		if request.Method != "PUT" || request.UploadID != "native-session" || request.Part != strconv.Itoa(index+1) {
			t.Fatalf("wrong part request: %+v", request)
		}
		want := browserPartBytes
		if index == 1 {
			want = 3
		}
		if request.Bytes != want {
			t.Fatalf("part bytes=%d, want %d", request.Bytes, want)
		}
		if request.Path != "/"+paths.KeyFor(session.Path, session.OwnerID) {
			t.Fatalf("native filename not preserved: %q", request.Path)
		}
	}
	published, err := client.Publish(context.Background(), session, data)
	if err != nil || !published {
		t.Fatalf("publish=%v err=%v", published, err)
	}
	requests = fixture.snapshot()
	if len(requests) != 4 || requests[3].Method != "POST" || requests[3].UploadID != "native-session" {
		t.Fatalf("expected exactly one native complete: %+v", requests)
	}
	var completion cos.CompleteMultipartUploadOptions
	if err := xml.Unmarshal(requests[3].Body, &completion); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(completion.Parts, last.Parts) {
		t.Fatalf("completion parts=%+v staged=%+v", completion.Parts, last.Parts)
	}
	// The fixture accepts only native object methods. No STS/token endpoint was
	// called: browser receipt creation never needs provider credentials here.
}

func TestBrowserCOSStageRejectsWrongBodyLengthWithoutComplete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		declared int64
		body     string
	}{{"short", 4, "abc"}, {"overlong", 2, "abc"}, {"zero-overlong", 0, "a"}} {
		t.Run(tc.name, func(t *testing.T) {
			client, fixture := newBrowserCOSFixture(t)
			var last browserStage
			data, err := client.Stage(context.Background(), browserTestSession(tc.declared), strings.NewReader(tc.body), func(_ context.Context, data json.RawMessage) error { return json.Unmarshal(data, &last) })
			if err == nil || len(data) != 0 || last.Ready {
				t.Fatalf("wrong body accepted: ready=%v data=%s err=%v", last.Ready, data, err)
			}
			for _, request := range fixture.snapshot() {
				if request.Method == "POST" && request.UploadID != "" {
					t.Fatal("mismatched staging completed native object")
				}
				if request.Method == "PUT" && request.UploadID == "" {
					t.Fatal("mismatched staging wrote final object")
				}
			}
		})
	}
}

func TestBrowserCOSZeroBytePublishesOnlyAfterReady(t *testing.T) {
	client, fixture := newBrowserCOSFixture(t)
	session := browserTestSession(0)
	data, checkpoints := stageBrowserFile(t, client, session, bytes.NewReader(nil))
	if len(fixture.snapshot()) != 0 || len(checkpoints) != 1 || !checkpoints[0].Ready || checkpoints[0].UploadID != "" {
		t.Fatal("empty file was published or initialized during staging")
	}
	ok, err := client.Publish(context.Background(), session, data)
	if !ok || err != nil {
		t.Fatal("zero-byte publish failed", err)
	}
	requests := fixture.snapshot()
	if len(requests) != 1 || requests[0].Method != "PUT" || requests[0].Bytes != 0 || requests[0].Marker != session.ID || requests[0].ContentType != session.Mime {
		t.Fatalf("wrong empty native publication: %+v", requests)
	}
}

func TestBrowserCOSAmbiguousCompleteIsNotRetried(t *testing.T) {
	client, fixture := newBrowserCOSFixture(t)
	session := browserTestSession(3)
	data, _ := stageBrowserFile(t, client, session, strings.NewReader("abc"))
	fixture.mu.Lock()
	fixture.completeFailure = true
	fixture.mu.Unlock()
	ok, err := client.Publish(context.Background(), session, data)
	if err == nil || ok {
		t.Fatal("ambiguous completion reported terminal success")
	}
	completions := 0
	for _, request := range fixture.snapshot() {
		if request.Method == "POST" && request.UploadID != "" {
			completions++
		}
	}
	if completions != 1 {
		t.Fatalf("native complete automatically repeated %d times", completions)
	}
}

func TestBrowserCOSInspectAttributesAttemptWithoutClaimingTerminal(t *testing.T) {
	client, fixture := newBrowserCOSFixture(t)
	session := browserTestSession(3)
	for _, tc := range []struct {
		status  int
		marker  string
		applied bool
	}{{200, "another-attempt", false}, {200, session.ID, true}, {404, "", false}} {
		fixture.mu.Lock()
		fixture.headStatus = tc.status
		fixture.headMarker = tc.marker
		fixture.mu.Unlock()
		result, err := client.Inspect(context.Background(), session)
		if err != nil || result.Applied != tc.applied || result.Terminal {
			t.Fatalf("inspect status=%d marker=%q: %+v err=%v", tc.status, tc.marker, result, err)
		}
		if tc.status == 200 {
			err = client.BrowserUploadMarker(context.Background(), session.OwnerID, session.Path, session.ID)
			if (err == nil) != tc.applied {
				t.Fatalf("marker verification mismatch: %v", err)
			}
		}
	}
}

func TestBrowserCOSDiscardAbortsExactSessionNeverDeletesFinalFile(t *testing.T) {
	client, fixture := newBrowserCOSFixture(t)
	session := browserTestSession(3)
	data, _ := stageBrowserFile(t, client, session, strings.NewReader("abc"))
	session.Checkpoint = data
	if err := client.Discard(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	requests := fixture.snapshot()
	last := requests[len(requests)-1]
	if last.Method != "DELETE" || last.UploadID != "native-session" || last.Path != "/"+paths.KeyFor(session.Path, session.OwnerID) {
		t.Fatalf("discard did not abort exact native session: %+v", last)
	}
	for _, request := range requests {
		if request.Method == "DELETE" && request.UploadID == "" {
			t.Fatal("discard deleted final native path")
		}
	}
	session.Checkpoint = json.RawMessage(`{}`)
	before := len(requests)
	if err := client.Discard(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	if len(fixture.snapshot()) != before {
		t.Fatal("empty checkpoint attempted native deletion")
	}
}

func TestBrowserCOSAllocationCheckpointFailureSendsNoParts(t *testing.T) {
	client, fixture := newBrowserCOSFixture(t)
	_, err := client.Stage(context.Background(), browserTestSession(3), strings.NewReader("abc"), func(context.Context, json.RawMessage) error { return fmt.Errorf("journal unavailable") })
	if err == nil {
		t.Fatal("failed allocation checkpoint was ignored")
	}
	requests := fixture.snapshot()
	if len(requests) != 1 || requests[0].Method != "POST" || requests[0].UploadID != "" {
		t.Fatalf("parts sent before allocation checkpoint: %+v", requests)
	}
}
