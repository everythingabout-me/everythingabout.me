package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/storage"
)

func contentRequest(driver storage.Driver) *fileRequest {
	return &fileRequest{Principal: &auth.Principal{User: &auth.User{}}, Driver: driver, Scope: "/", Namespace: "caller-scope", ContentNamespace: "owner-root-config"}
}

func runContent(t *testing.T, f *fileRequest, method, path string, headers http.Header) (*httptest.ResponseRecorder, error) {
	t.Helper()
	r := httptest.NewRequest(method, "/api/files/content?"+url.Values{"path": {path}}.Encode(), nil)
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	w := httptest.NewRecorder()
	err := (&Server{}).handleFilesContent(w, r, f)
	return w, err
}

func localContentFixture(t *testing.T) (*fileRequest, string) {
	t.Helper()
	root := t.TempDir()
	content, system := filepath.Join(root, "content"), filepath.Join(root, "system")
	for _, dir := range []string{content, system, filepath.Join(content, "folder")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := localstorage.NewFactory(localstorage.Options{CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: content, SystemRoot: system})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := driver.Close(); err != nil {
			t.Error(err)
		}
	})
	return contentRequest(driver), content
}

func TestFilesContentLocalBytesNamesAndHead(t *testing.T) {
	f, root := localContentFixture(t)
	names := []string{"literal +#?%.txt", "literal%2Fname.bin", "quote\" semi;.txt", "日本語.txt", "empty", "active.html", "active.svg", "image.png"}
	payload := []byte("0123456789\x00\xff")
	for _, name := range names {
		data := payload
		if name == "empty" {
			data = nil
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+"/"+name, func(t *testing.T) {
				w, err := runContent(t, f, method, "/"+name, nil)
				if err != nil {
					t.Fatal(err)
				}
				if w.Code != 200 || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) {
					t.Fatalf("status/length: %d %v", w.Code, w.Header())
				}
				if method == http.MethodGet && !bytes.Equal(w.Body.Bytes(), data) || method == http.MethodHead && w.Body.Len() != 0 {
					t.Fatalf("body %q", w.Body.Bytes())
				}
				if w.Header().Get("ETag") != "" {
					t.Fatal("Local must not invent a strong content validator")
				}
				if w.Header().Get("Cache-Control") != "private, no-cache" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Accept-Ranges") != "bytes" {
					t.Fatalf("headers: %v", w.Header())
				}
				kind, params, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
				if err != nil || kind != "attachment" || params["filename"] != name {
					t.Fatalf("disposition: %s %v", w.Header().Get("Content-Disposition"), err)
				}
				if w.Header().Get("Content-Type") == "" {
					t.Fatal("missing content type")
				}
			})
		}
	}
	for _, p := range []string{"/", "/folder"} {
		w, err := runContent(t, f, http.MethodGet, p, nil)
		assertContentProblem(t, err, 409, "not_a_file")
		if w.Body.Len() != 0 {
			t.Fatal("error wrote bytes")
		}
	}
}

func TestFilesContentLocalRanges(t *testing.T) {
	f, root := localContentFixture(t)
	if err := os.WriteFile(filepath.Join(root, "data"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, method, rangeValue, ifRange, want, contentRange string
		status                                                int
	}{
		{name: "bounded", rangeValue: "bytes=2-5", want: "2345", contentRange: "bytes 2-5/10", status: 206},
		{name: "open", rangeValue: "bytes=7-", want: "789", contentRange: "bytes 7-9/10", status: 206},
		{name: "suffix", rangeValue: "bytes=-3", want: "789", contentRange: "bytes 7-9/10", status: 206},
		{name: "long suffix", rangeValue: "bytes=-100", want: "0123456789", contentRange: "bytes 0-9/10", status: 206},
		{name: "clamped", rangeValue: "bytes=8-100", want: "89", contentRange: "bytes 8-9/10", status: 206},
		{name: "multi", rangeValue: "bytes=0-1,8-9", want: "0123456789", status: 200},
		{name: "unknown unit", rangeValue: "records=1-2", want: "0123456789", status: 200},
		{name: "head ignores", method: http.MethodHead, rangeValue: "bytes=90-100", status: 200},
		{name: "if range unavailable", rangeValue: "bytes=0-1", ifRange: "\"unproven\"", want: "0123456789", status: 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			header := http.Header{"Range": {tt.rangeValue}}
			if tt.ifRange != "" {
				header.Set("If-Range", tt.ifRange)
			}
			w, err := runContent(t, f, method, "/data", header)
			if err != nil || w.Code != tt.status || w.Body.String() != tt.want || w.Header().Get("Content-Range") != tt.contentRange {
				t.Fatalf("status %d, headers %v, body %q, error %v", w.Code, w.Header(), w.Body.String(), err)
			}
			length := len(tt.want)
			if method == http.MethodHead {
				length = 10
			}
			if w.Header().Get("Content-Length") != strconv.Itoa(length) {
				t.Fatalf("length: %v", w.Header())
			}
		})
	}
	for _, value := range []string{"bytes=10-", "bytes=-0", "bytes=5-4", "bytes=wat", "bytes=+1-2", "bytes=0-9223372036854775808", "bytes=--1"} {
		t.Run(value, func(t *testing.T) {
			w, err := runContent(t, f, http.MethodGet, "/data", http.Header{"Range": {value}})
			assertContentProblem(t, err, 416, "range_not_satisfiable")
			if w.Header().Get("Content-Range") != "bytes */10" || w.Body.Len() != 0 {
				t.Fatalf("headers/body: %v %q", w.Header(), w.Body.String())
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	w, err := runContent(t, f, http.MethodGet, "/empty", http.Header{"Range": {"bytes=0-"}})
	assertContentProblem(t, err, 416, "range_not_satisfiable")
	if w.Header().Get("Content-Range") != "bytes */0" {
		t.Fatal(w.Header())
	}
}

func TestFilesContentInlineSafety(t *testing.T) {
	for _, tt := range []struct {
		media  string
		inline bool
	}{
		{"image/png", true}, {"text/plain; charset=utf-8", true}, {"video/mp4", true}, {"audio/mpeg", true},
		{"text/html", false}, {"image/svg+xml", false}, {"application/xhtml+xml", false}, {"text/javascript", false},
		{"application/javascript", false}, {"application/pdf", false}, {"application/xml", false}, {"image/unknown", false},
		{"text/html\r\nX-Injected: yes", false},
	} {
		t.Run(tt.media, func(t *testing.T) {
			driver := newContentDriver("payload", nil)
			driver.entry.MediaType = tt.media
			r := httptest.NewRequest(http.MethodGet, "/api/files/content?path=%2Ffile&disposition=inline", nil)
			w := httptest.NewRecorder()
			if err := (&Server{}).handleFilesContent(w, r, contentRequest(driver)); err != nil {
				t.Fatal(err)
			}
			kind, _, err := mime.ParseMediaType(w.Header().Get("Content-Disposition"))
			want := "attachment"
			if tt.inline {
				want = "inline"
			}
			if err != nil || kind != want || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("headers: %v", w.Header())
			}
			if strings.ContainsAny(w.Header().Get("Content-Type"), "\r\n") {
				t.Fatal("unsafe MIME header")
			}
		})
	}
}

func TestFilesContentVersionedConditions(t *testing.T) {
	driver := newContentDriver("0123456789", []byte("native-version:secret-locator"))
	f := contentRequest(driver)
	tag := strconv.Quote(*filesContentRevision(f.ContentNamespace, driver.entry))
	for _, tt := range []struct {
		name    string
		headers http.Header
		status  int
		want    string
	}{
		{"get", nil, 200, "0123456789"},
		{"match", http.Header{"If-Match": {tag}}, 200, "0123456789"},
		{"match list", http.Header{"If-Match": {"\"other\", " + tag}}, 200, "0123456789"},
		{"weak match fails", http.Header{"If-Match": {"W/" + tag}, "Range": {"bytes=99-"}}, 412, ""},
		{"match fails before range", http.Header{"If-Match": {"\"other\""}, "Range": {"bytes=99-"}}, 412, ""},
		{"none match", http.Header{"If-None-Match": {tag}, "Range": {"bytes=99-"}}, 304, ""},
		{"weak none match", http.Header{"If-None-Match": {"W/" + tag}}, 304, ""},
		{"none wildcard", http.Header{"If-None-Match": {"*"}}, 304, ""},
		{"match wildcard", http.Header{"If-Match": {"*"}}, 200, "0123456789"},
		{"if range matches", http.Header{"Range": {"bytes=2-3"}, "If-Range": {tag}}, 206, "23"},
		{"if range mismatch", http.Header{"Range": {"bytes=99-"}, "If-Range": {"\"other\""}}, 200, "0123456789"},
		{"if range weak", http.Header{"Range": {"bytes=2-3"}, "If-Range": {"W/" + tag}}, 200, "0123456789"},
		{"if range date", http.Header{"Range": {"bytes=2-3"}, "If-Range": {"Wed, 21 Oct 2015 07:28:00 GMT"}}, 200, "0123456789"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			driver.opens = nil
			w, err := runContent(t, f, http.MethodGet, "/file", tt.headers)
			if tt.status == 412 {
				assertContentProblem(t, err, 412, "precondition_failed")
			} else if err != nil || w.Code != tt.status {
				t.Fatalf("status %d error %v", w.Code, err)
			}
			if w.Body.String() != tt.want {
				t.Fatalf("body: %q", w.Body.String())
			}
			if tt.status != 412 && w.Header().Get("ETag") != tag {
				t.Fatalf("ETag %q != %q", w.Header().Get("ETag"), tag)
			}
			for _, options := range driver.opens {
				if !bytes.Equal(options.Version, driver.entry.Version) {
					t.Fatal("validator used without native pinned Open")
				}
			}
			if strings.Contains(w.Header().Get("ETag"), "secret-locator") {
				t.Fatal("native version exposed")
			}
		})
	}
	if driver.closes != driver.openCount {
		t.Fatalf("leaked bodies: %d closes/%d opens", driver.closes, driver.openCount)
	}
}

func TestFilesContentVersionedHeadAndEmptyFile(t *testing.T) {
	for _, data := range []string{"payload", ""} {
		driver := newContentDriver(data, []byte("native-version:immutable"))
		driver.bodyReader = contentErrorReader{}
		w, err := runContent(t, contentRequest(driver), http.MethodHead, "/file", http.Header{"Range": {"bytes=900-"}})
		if err != nil || w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(data)) || w.Header().Get("ETag") == "" {
			t.Fatalf("HEAD: %v %v %q", err, w.Header(), w.Body.String())
		}
		if driver.closes != 1 || len(driver.opens) != 1 || len(driver.opens[0].Version) == 0 {
			t.Fatal("HEAD did not prove and close a pinned native read")
		}
	}
	driver := newContentDriver("", []byte("native-version:empty"))
	w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", nil)
	if err != nil || w.Header().Get("Content-Length") != "0" || w.Body.Len() != 0 || driver.closes != 1 {
		t.Fatalf("empty file: %v %v", err, w.Header())
	}
}

func TestFilesContentLocalConditionsDoNotInventVersions(t *testing.T) {
	f, root := localContentFixture(t)
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		header http.Header
		status int
	}{
		{http.Header{"If-Match": {"\"guess\""}}, 412},
		{http.Header{"If-Match": {"*"}}, 200},
		{http.Header{"If-None-Match": {"\"guess\""}}, 200},
		{http.Header{"If-None-Match": {"*"}}, 304},
	} {
		w, err := runContent(t, f, http.MethodGet, "/file", tt.header)
		if tt.status == 412 {
			assertContentProblem(t, err, 412, "precondition_failed")
		} else if err != nil || w.Code != tt.status {
			t.Fatalf("status %d error %v", w.Code, err)
		}
		if w.Header().Get("ETag") != "" {
			t.Fatal("unversioned ETag")
		}
	}
}

func TestFilesContentAuthorizationAndQueryBeforeNativeIO(t *testing.T) {
	for _, tt := range []struct {
		query  string
		scope  fc.Path
		token  *auth.Token
		status int
		code   string
	}{
		{"path=%2Fprivate", "/allowed", nil, 404, "not_found"},
		{"path=%2Ffile", "/", &auth.Token{CanRead: false}, 403, "forbidden"},
		{"path=%2Ffile&path=%2Fother", "/", nil, 400, "invalid_selector"},
		{"path=%ZZ", "/", nil, 400, "invalid_request"},
		{"path=%2F..%2Ffile", "/", nil, 400, "invalid_path"},
	} {
		driver := newContentDriver("data", nil)
		f := contentRequest(driver)
		f.Scope = tt.scope
		f.Principal.Token = tt.token
		r := httptest.NewRequest(http.MethodGet, "/api/files/content?"+tt.query, nil)
		w := httptest.NewRecorder()
		err := (&Server{}).handleFilesContent(w, r, f)
		assertContentProblem(t, err, tt.status, tt.code)
		if driver.stats != 0 || driver.openCount != 0 || w.Body.Len() != 0 {
			t.Fatal("native I/O or output before request validation/authorization")
		}
	}
}

func TestFilesContentActualOpenMetadataAndChangingRanges(t *testing.T) {
	t.Run("actual full representation", func(t *testing.T) {
		driver := newContentDriver("new payload", nil)
		driver.statEntry = &storage.Entry{Kind: storage.File, Path: "file", Size: 1, MediaType: "text/html"}
		driver.entry.MediaType = "text/plain"
		w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", nil)
		if err != nil || w.Body.String() != "new payload" || w.Header().Get("Content-Length") != "11" || w.Header().Get("Content-Type") != "text/plain" {
			t.Fatalf("%v %v %q", err, w.Header(), w.Body.String())
		}
	})
	t.Run("version appears between stat and get", func(t *testing.T) {
		driver := newContentDriver("new", []byte("real-version"))
		driver.statEntry = &storage.Entry{Kind: storage.File, Path: "file", Size: 3}
		w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", nil)
		if err != nil || w.Header().Get("ETag") == "" || len(driver.opens) != 2 || len(driver.opens[0].Version) != 0 || string(driver.opens[1].Version) != "real-version" {
			t.Fatalf("%v %v %+v", err, w.Header(), driver.opens)
		}
	})
	t.Run("suffix retries new actual size", func(t *testing.T) {
		driver := newContentDriver("0123456789", nil)
		driver.beforeOpen = func(n int, opts storage.ReadOptions) {
			if n == 2 {
				driver.data = "abcdefghijkl"
				driver.entry.Size = 12
			}
		}
		w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", http.Header{"Range": {"bytes=-3"}})
		if err != nil || w.Body.String() != "jkl" || w.Header().Get("Content-Range") != "bytes 9-11/12" {
			t.Fatalf("%v %v %q", err, w.Header(), w.Body.String())
		}
		if driver.closes != driver.openCount {
			t.Fatal("retry leaked body")
		}
	})
	t.Run("mismatch never emits old validator", func(t *testing.T) {
		driver := newContentDriver("payload", []byte("current"))
		driver.statEntry = &storage.Entry{Kind: storage.File, Path: "file", Size: 7, Version: []byte("wrong")}
		driver.ignoreVersion = true
		w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", nil)
		if !storage.IsCode(err, "changed") || driver.openCount != 3 || w.Header().Get("ETag") != "" || w.Body.Len() != 0 || driver.closes != 3 {
			t.Fatalf("%v %v count=%d closes=%d", err, w.Header(), driver.openCount, driver.closes)
		}
	})
	t.Run("range actual path mismatch", func(t *testing.T) {
		driver := newContentDriver("payload", nil)
		driver.mutateRead = func(r *storage.Read) {
			if r.Offset > 0 {
				r.Entry.Path = "other/private"
			}
		}
		w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", http.Header{"Range": {"bytes=1-3"}})
		if !storage.IsCode(err, "invalid_backend_response") || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "" || driver.closes != 2 {
			t.Fatalf("cross-path range escaped validation: %v %v closes=%d", err, w.Header(), driver.closes)
		}
	})
	t.Run("range unsupported", func(t *testing.T) {
		driver := newContentDriver("payload", nil)
		driver.ranges = false
		w, err := runContent(t, contentRequest(driver), http.MethodGet, "/file", http.Header{"Range": {"bytes=99-"}})
		if err != nil || w.Code != 200 || w.Body.String() != "payload" || w.Header().Get("Accept-Ranges") != "" {
			t.Fatalf("%v %v %q", err, w.Header(), w.Body.String())
		}
	})
}

func TestFilesContentFailuresAndClosing(t *testing.T) {
	for _, stage := range []string{"stat", "open", "head close", "nil body", "invalid path", "invalid offset", "invalid length", "oversized"} {
		t.Run(stage, func(t *testing.T) {
			driver := newContentDriver("payload", nil)
			failure := errors.New("private native failure")
			method := http.MethodGet
			switch stage {
			case "stat":
				driver.statErr = failure
			case "open":
				driver.openErr = failure
			case "head close":
				driver.closeErr = failure
				method = http.MethodHead
			case "nil body":
				driver.mutateRead = func(r *storage.Read) { r.Body = nil }
			case "invalid path":
				driver.mutateRead = func(r *storage.Read) { r.Entry.Path = "other/private" }
			case "oversized":
				driver.mutateRead = func(r *storage.Read) { r.Entry.Size = 1 << 53; r.Length = 1 << 53 }
			case "invalid offset":
				driver.mutateRead = func(r *storage.Read) { r.Offset = 1 }
			case "invalid length":
				driver.mutateRead = func(r *storage.Read) { r.Length-- }
			}
			w, err := runContent(t, contentRequest(driver), method, "/file", nil)
			if err == nil || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "" || w.Header().Get("ETag") != "" {
				t.Fatalf("premature headers/body: %v %v %q", err, w.Header(), w.Body.String())
			}
			if stage != "nil body" && driver.openCount != driver.closes {
				t.Fatalf("leak %d/%d", driver.openCount, driver.closes)
			}
		})
	}
	for _, stage := range []string{"short read", "long read", "read failure", "final byte failure", "close failure", "writer failure"} {
		t.Run(stage, func(t *testing.T) {
			driver := newContentDriver("payload", nil)
			switch stage {
			case "short read":
				driver.bodyReader = strings.NewReader("pay")
			case "long read":
				driver.bodyReader = strings.NewReader("payloadextra")
			case "read failure":
				driver.bodyReader = contentErrorReader{}
			case "final byte failure":
				driver.bodyReader = &contentFinalErrorReader{data: "payload"}
			case "close failure":
				driver.closeErr = errors.New("close failed")
			}
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			if stage == "writer failure" {
				writer = contentFailWriter{recorder}
			}
			r := httptest.NewRequest(http.MethodGet, "/api/files/content?path=%2Ffile", nil)
			assertContentAbort(t, func() { _ = (&Server{}).handleFilesContent(writer, r, contentRequest(driver)) })
			if strings.Contains(recorder.Body.String(), "{\"ok\"") || driver.closes != 1 {
				t.Fatalf("stream appended error or leaked body: %q closes=%d", recorder.Body.String(), driver.closes)
			}
		})
	}
}

func TestFilesContentCancellationUnblocksAndCloses(t *testing.T) {
	driver := newContentDriver("payload", nil)
	body := &contentBlockingBody{started: make(chan struct{}), closed: make(chan struct{})}
	driver.customBody = body
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodGet, "/api/files/content?path=%2Ffile", nil).WithContext(ctx)
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		_ = (&Server{}).handleFilesContent(httptest.NewRecorder(), r, contentRequest(driver))
	}()
	select {
	case <-body.started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not start")
	}
	cancel()
	select {
	case value := <-done:
		if value != http.ErrAbortHandler {
			t.Fatalf("panic: %v", value)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not unblock native read")
	}
	if body.closes.Load() != 1 {
		t.Fatalf("cancelled native stream close count: %d", body.closes.Load())
	}
}

func TestFilesContentHTTPFailureTruncatesConnection(t *testing.T) {
	data := strings.Repeat("x", 1<<20)
	driver := newContentDriver(data, nil)
	driver.bodyReader = io.MultiReader(strings.NewReader(data[:128<<10]), contentErrorReader{})
	done := make(chan struct{})
	s := &Server{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if err := s.handleFilesContent(w, r, contentRequest(driver)); err != nil {
			http.Error(w, "unexpected pre-stream failure", 500)
		}
	})
	server := httptest.NewServer(s.recoverer(s.logger(handler)))
	defer server.Close()
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Get(server.URL + "/api/files/content?path=%2Ffile")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil || response.StatusCode != 200 || response.ContentLength != int64(len(data)) {
		t.Fatalf("expected a truncated content stream: status=%d length=%d error=%v", response.StatusCode, response.ContentLength, err)
	}
	if len(body) == 0 || !bytes.Equal(body, []byte(data[:len(body)])) || len(body) >= len(data) {
		t.Fatalf("unexpected partial bytes: %d", len(body))
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("aborted handler did not finish")
	}
	if driver.closes != 1 {
		t.Fatalf("native body close count: %d", driver.closes)
	}
}

func TestFilesContentRevisionBindingAndNativeRename(t *testing.T) {
	entry := storage.Entry{Kind: storage.File, Path: "old", NativeRef: []byte("private/old"), Version: []byte("native-version:opaque")}
	a := filesContentRevision("owner-root", entry)
	if a == nil {
		t.Fatal("versioned file has no revision")
	}
	entry.NativeRef = []byte("another-private-locator")
	if other := filesContentRevision("owner-root", entry); *other == *a {
		t.Fatal("different native objects reused a content revision")
	}
	entry.NativeRef = []byte("private/old")
	if other := filesContentRevision("other-owner-root", entry); *other == *a {
		t.Fatal("revision is not root-bound")
	}
	entry.Path = "new"
	b := filesContentRevision("owner-root", entry)
	if b == nil || *a == *b {
		t.Fatal("different native paths reused a content revision")
	}
	entry.Version = nil
	if filesContentRevision("owner-root", entry) != nil {
		t.Fatal("unversioned content has revision")
	}
	entry.Kind = storage.Folder
	entry.Version = []byte("native-version:folder")
	if filesContentRevision("owner-root", entry) != nil {
		t.Fatal("folder has content revision")
	}
}

func TestFilesContentPerPathNativeVersionDoesNotAliasValidators(t *testing.T) {
	// Drivers may use a revision counter whose value repeats across distinct files.
	// These equal-length contents must not share HTTP cache or resume validators.
	driver := newContentDriver("AAA", []byte("1"))
	driver.entry.Path = "a"
	f := contentRequest(driver)
	a, err := runContent(t, f, http.MethodGet, "/a", nil)
	if err != nil || a.Body.String() != "AAA" {
		t.Fatalf("first file: %v %q", err, a.Body.String())
	}
	tagA := a.Header().Get("ETag")
	if tagA == "" {
		t.Fatal("missing pinned content validator")
	}
	driver.entry.Path = "b"
	driver.data = "BBB"
	for _, tt := range []struct {
		name    string
		headers http.Header
		status  int
		body    string
	}{
		{"none match", http.Header{"If-None-Match": {tagA}}, 200, "BBB"},
		{"if range", http.Header{"Range": {"bytes=1-"}, "If-Range": {tagA}}, 200, "BBB"},
		{"if match", http.Header{"If-Match": {tagA}}, 412, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := runContent(t, f, http.MethodGet, "/b", tt.headers)
			if tt.status == 412 {
				assertContentProblem(t, err, 412, "precondition_failed")
			} else if err != nil || b.Code != tt.status {
				t.Fatalf("second file status=%d error=%v", b.Code, err)
			}
			if b.Body.String() != tt.body {
				t.Fatalf("second file body: %q", b.Body.String())
			}
			if tt.status != 412 && (b.Header().Get("ETag") == "" || b.Header().Get("ETag") == tagA) {
				t.Fatal("distinct file reused another file's validator")
			}
		})
	}
	if driver.closes != driver.openCount {
		t.Fatalf("leaked native body: %d/%d", driver.closes, driver.openCount)
	}
}

func TestFilesContentNativeReplacementDoesNotAliasValidators(t *testing.T) {
	driver := newContentDriver("AAA", []byte("1"))
	driver.entry.NativeRef = []byte("object-A")
	f := contentRequest(driver)
	a, err := runContent(t, f, http.MethodGet, "/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	tagA := a.Header().Get("ETag")
	driver.entry.NativeRef = []byte("object-B")
	driver.data = "BBB"
	for _, headers := range []http.Header{
		{"If-None-Match": {tagA}},
		{"Range": {"bytes=1-"}, "If-Range": {tagA}},
	} {
		b, err := runContent(t, f, http.MethodGet, "/file", headers)
		if err != nil || b.Code != 200 || b.Body.String() != "BBB" || b.Header().Get("ETag") == tagA {
			t.Fatalf("replacement reused validator: status=%d error=%v headers=%v body=%q", b.Code, err, b.Header(), b.Body.String())
		}
	}
}

func TestFilesContentRangeNativeReplacementReevaluatesConditions(t *testing.T) {
	driver := newContentDriver("AAA", []byte("1"))
	driver.entry.NativeRef = []byte("object-A")
	f := contentRequest(driver)
	tagA := strconv.Quote(*filesContentRevision(f.ContentNamespace, driver.entry))
	driver.beforeOpen = func(n int, options storage.ReadOptions) {
		if n == 2 {
			driver.entry.NativeRef = []byte("object-B")
			driver.data = "BBB"
		}
	}
	w, err := runContent(t, f, http.MethodGet, "/file", http.Header{"Range": {"bytes=1-"}, "If-Range": {tagA}})
	if err != nil || w.Code != 200 || w.Body.String() != "BBB" || w.Header().Get("ETag") == tagA || driver.openCount != 3 {
		t.Fatalf("mixed native identities: status=%d error=%v headers=%v body=%q opens=%d", w.Code, err, w.Header(), w.Body.String(), driver.openCount)
	}
	if driver.closes != 3 {
		t.Fatalf("replacement retry leaked body: %d", driver.closes)
	}
}

func assertContentProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var problem *fc.Problem
	if !errors.As(err, &problem) || problem.Status != status || problem.Code != code {
		t.Fatalf("wanted %d %s; got %v", status, code, err)
	}
}
func assertContentAbort(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if value := recover(); value != http.ErrAbortHandler {
			t.Fatalf("wanted ErrAbortHandler panic; got %v", value)
		}
	}()
	fn()
}

type contentDriver struct {
	entry                      storage.Entry
	statEntry                  *storage.Entry
	data                       string
	ranges, ignoreVersion      bool
	stats, openCount, closes   int
	opens                      []storage.ReadOptions
	statErr, openErr, closeErr error
	bodyReader                 io.Reader
	customBody                 io.ReadCloser
	beforeOpen                 func(int, storage.ReadOptions)
	mutateRead                 func(*storage.Read)
}

func newContentDriver(data string, version []byte) *contentDriver {
	return &contentDriver{entry: storage.Entry{Path: "file", NativeRef: []byte("native-object:file"), Kind: storage.File, Size: int64(len(data)), Version: version}, data: data, ranges: true}
}
func (d *contentDriver) Describe() storage.Descriptor {
	return storage.Descriptor{RangeRead: d.ranges, Names: storage.NameRules{CaseSensitive: true}}
}
func (d *contentDriver) Close() error { return nil }
func (d *contentDriver) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	d.stats++
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	if d.statErr != nil {
		return storage.Entry{}, d.statErr
	}
	if d.statEntry != nil {
		return *d.statEntry, nil
	}
	return d.entry, nil
}
func (d *contentDriver) List(context.Context, storage.Path, storage.PageRequest) (storage.Page, error) {
	panic("unexpected List")
}
func (d *contentDriver) Open(ctx context.Context, p storage.Path, options storage.ReadOptions) (storage.Read, error) {
	if err := ctx.Err(); err != nil {
		return storage.Read{}, err
	}
	if d.openErr != nil {
		return storage.Read{}, d.openErr
	}
	d.openCount++
	d.opens = append(d.opens, options)
	if d.beforeOpen != nil {
		d.beforeOpen(d.openCount, options)
	}
	if len(options.Version) > 0 && !d.ignoreVersion && !bytes.Equal(options.Version, d.entry.Version) {
		return storage.Read{}, storage.NewError("changed", p, nil)
	}
	length := int64(len(d.data)) - options.Offset
	if options.Length != nil {
		length = *options.Length
	}
	if options.Offset < 0 || length < 0 || options.Offset+length > int64(len(d.data)) {
		return storage.Read{}, storage.NewError("invalid_range", p, nil)
	}
	var reader io.Reader = strings.NewReader(d.data[options.Offset : options.Offset+length])
	if d.bodyReader != nil {
		reader = d.bodyReader
	}
	var body io.ReadCloser = &contentCountBody{Reader: reader, onClose: func() error { d.closes++; return d.closeErr }}
	if d.customBody != nil {
		body = d.customBody
	}
	read := storage.Read{Body: body, Entry: d.entry, Offset: options.Offset, Length: length}
	if d.mutateRead != nil {
		d.mutateRead(&read)
	}
	return read, nil
}

type contentCountBody struct {
	io.Reader
	once    sync.Once
	onClose func() error
	err     error
}

func (b *contentCountBody) Close() error { b.once.Do(func() { b.err = b.onClose() }); return b.err }

type contentErrorReader struct{}

func (contentErrorReader) Read([]byte) (int, error) { return 0, errors.New("native stream failed") }

type contentFinalErrorReader struct{ data string }

func (r *contentFinalErrorReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, errors.New("failure with final native bytes")
}

type contentFailWriter struct{ *httptest.ResponseRecorder }

func (contentFailWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }

type contentBlockingBody struct {
	started, closed     chan struct{}
	readOnce, closeOnce sync.Once
	closes              atomic.Int32
}

func (b *contentBlockingBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, context.Canceled
}
func (b *contentBlockingBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}
