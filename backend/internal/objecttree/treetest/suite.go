// Package treetest exercises both object-store SDK adapters against native HTTP
// fixtures. It is imported only by adapter tests and never contacts a provider.
package treetest

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

const special = " space +%?#é .txt"
const versionID = "v+1/=?&%"

var fixedTime = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type nativeFixture struct {
	mu           sync.Mutex
	objects      map[string][]byte
	mode         string
	requests     int
	listRequests int
	version      string
	started      chan struct{}
	cancelled    chan struct{}
}

func fixture() *nativeFixture {
	return &nativeFixture{objects: map[string][]byte{
		"drive/alice/": {}, "drive/alice/" + special: []byte("0123456789"), "drive/alice/empty/": {}, "drive/alice/notes/a%2Fb.txt": []byte("raw %2F name"), "drive/alice/trailing ": []byte("trailing space"), "drive/alice/zero": {}, "drive/alice/NFD-e\u0301": []byte("decomposed"), "drive/bob/private": []byte("bob"), "system/alice/private": []byte("system"),
	}, version: versionID}
}
func binding(owner string) storage.Config {
	return storage.Config{ConnectionID: "test", Generation: 1, OwnerID: owner, ContentRoot: "drive/" + owner, SystemRoot: "system/" + owner}
}

// Run verifies exact native requests and byte readback through the actual SDK.
// The constructor must use test server only and explicit disposable credentials.
func Run(t *testing.T, construct func(*httptest.Server) (storage.Factory, error)) {
	t.Helper()
	setup := func(t *testing.T) (*nativeFixture, storage.Factory, storage.Driver) {
		t.Helper()
		f := fixture()
		server := httptest.NewServer(f)
		t.Cleanup(server.Close)
		factory, err := construct(server)
		if err != nil {
			t.Fatal(err)
		}
		driver, err := factory.Open(context.Background(), binding("alice"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { driver.Close() })
		return f, factory, driver
	}
	t.Run("native_tree_exit_and_exact_names", func(t *testing.T) {
		native, _, driver := setup(t)
		ctx := context.Background()
		descriptor := driver.Describe()
		if !descriptor.RangeRead || descriptor.PinnedRead || descriptor.AtomicCreate || descriptor.AtomicReplace || descriptor.NativeConditionalMutation || descriptor.NativeTreeMove {
			t.Fatalf("unproven capability: %+v", descriptor)
		}
		root, err := driver.Stat(ctx, "")
		if err != nil || root.Kind != storage.Folder {
			t.Fatalf("root: %+v %v", root, err)
		}
		var entries []storage.Entry
		cursor := ""
		for {
			page, err := driver.List(ctx, "", storage.PageRequest{Limit: 2, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, page.Entries...)
			if page.NextCursor == "" {
				break
			}
			if page.NextCursor == cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = page.NextCursor
		}
		var names []string
		for _, entry := range entries {
			names = append(names, string(entry.Path))
		}
		expected := []string{special, "NFD-e\u0301", "empty", "notes", "trailing ", "zero"}
		if !reflect.DeepEqual(names, expected) {
			t.Fatalf("exact names: %#v want %#v", names, expected)
		}
		for _, path := range []storage.Path{storage.Path(special), "notes/a%2Fb.txt", "zero", "NFD-e\u0301", "trailing "} {
			info, err := driver.Stat(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			read, err := driver.Open(ctx, path, storage.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(read.Body)
			read.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			native.mu.Lock()
			want := append([]byte(nil), native.objects["drive/alice/"+string(path)]...)
			native.mu.Unlock()
			if string(data) != string(want) || info.Size != int64(len(want)) || read.Entry.Size != int64(len(want)) {
				t.Fatalf("native readback %q: %q %+v %+v", path, data, info, read)
			}
			if info.MediaType != "text/plain" || !info.ModifiedAt.Equal(fixedTime) || len(info.Version) == 0 {
				t.Fatalf("HEAD evidence: %+v", info)
			}
		}
		folder, err := driver.Stat(ctx, "notes")
		if err != nil || folder.Kind != storage.Folder {
			t.Fatalf("implicit folder: %+v %v", folder, err)
		}
		empty, err := driver.List(ctx, "empty", storage.PageRequest{})
		if err != nil || len(empty.Entries) != 0 {
			t.Fatalf("empty folder: %+v %v", empty, err)
		}
		native.mu.Lock()
		requests := native.listRequests
		native.mu.Unlock()
		if requests < 3 {
			t.Fatal("native pages were not followed")
		}
	})
	t.Run("ranges_and_actual_get_metadata", func(t *testing.T) {
		native, _, driver := setup(t)
		ctx := context.Background()
		for _, test := range []struct {
			offset int64
			length *int64
			want   string
		}{{0, ptr(1), "0"}, {2, ptr(4), "2345"}, {7, nil, "789"}} {
			read, err := driver.Open(ctx, storage.Path(special), storage.ReadOptions{Offset: test.offset, Length: test.length})
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(read.Body)
			read.Body.Close()
			if err != nil || string(data) != test.want || read.Offset != test.offset || read.Length != int64(len(test.want)) || read.Entry.Size != 10 {
				t.Fatalf("range: %q %+v %v", data, read, err)
			}
		}
		native.mu.Lock()
		native.mode = "get_changed"
		native.mu.Unlock()
		read, err := driver.Open(ctx, storage.Path(special), storage.ReadOptions{Offset: 2, Length: ptr(4)})
		if err != nil {
			t.Fatal(err)
		}
		read.Body.Close()
		if read.Entry.Size != 12 || string(read.Entry.Version) != "native-version:new-version" || !read.Entry.ModifiedAt.Equal(fixedTime.Add(time.Hour)) || read.Entry.MediaType != "application/changed" {
			t.Fatalf("GET representation was replaced by preflight metadata: %+v", read.Entry)
		}
	})
	t.Run("indexed_references_skip_ancestor_listing", func(t *testing.T) {
		native, _, driver := setup(t)
		ctx := context.Background()
		reader, ok := driver.(storage.ReferenceReader)
		if !ok {
			t.Fatal("tree does not serve reference-addressed reads")
		}
		key := []byte("drive/alice/" + special)
		native.mu.Lock()
		before := native.listRequests
		native.mu.Unlock()
		entry, err := reader.StatReference(ctx, storage.Path(special), key)
		if err != nil || entry.Kind != storage.File || entry.Size != 10 || len(entry.Version) == 0 {
			t.Fatalf("reference stat: %+v %v", entry, err)
		}
		read, err := reader.OpenReference(ctx, storage.Path(special), key, storage.ReadOptions{Offset: 2, Length: ptr(4)})
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(read.Body)
		read.Body.Close()
		if readErr != nil || string(data) != "2345" || read.Length != 4 || read.Entry.Size != 10 {
			t.Fatalf("reference range: %q %+v %v", data, read, readErr)
		}
		native.mu.Lock()
		after := native.listRequests
		native.mu.Unlock()
		if after != before {
			t.Fatalf("reference read listed ancestors: %d pages", after-before)
		}
		// A reference is never authority to read a different object.
		for _, ref := range [][]byte{[]byte("drive/bob/private"), []byte("drive/alice"), []byte("drive/alice/../bob/private"), nil} {
			if _, err := reader.StatReference(ctx, storage.Path(special), ref); !storage.IsCode(err, "stale_reference") {
				t.Fatalf("foreign reference accepted %q: %v", ref, err)
			}
			if _, err := reader.OpenReference(ctx, storage.Path(special), ref, storage.ReadOptions{}); !storage.IsCode(err, "stale_reference") {
				t.Fatalf("foreign reference opened %q: %v", ref, err)
			}
		}
	})
	t.Run("versions_conditions_and_missing", func(t *testing.T) {
		native, _, driver := setup(t)
		ctx := context.Background()
		entry, err := driver.Stat(ctx, storage.Path(special))
		if err != nil {
			t.Fatal(err)
		}
		read, err := driver.Open(ctx, storage.Path(special), storage.ReadOptions{Version: entry.Version})
		if err != nil {
			t.Fatal(err)
		}
		read.Body.Close()
		_, err = driver.Open(ctx, storage.Path(special), storage.ReadOptions{Version: []byte("native-version:missing")})
		code(t, err, "changed")
		_, err = driver.Open(ctx, storage.Path(special), storage.ReadOptions{Version: []byte("etag:guess")})
		code(t, err, "unsupported")
		native.mu.Lock()
		native.mode = "ignore_version"
		native.mu.Unlock()
		_, err = driver.Open(ctx, storage.Path(special), storage.ReadOptions{Version: []byte("native-version:other")})
		code(t, err, "changed")
		native.mu.Lock()
		native.mode = "precondition"
		native.mu.Unlock()
		_, err = driver.Open(ctx, storage.Path(special), storage.ReadOptions{Version: entry.Version})
		code(t, err, "changed")
		native.mu.Lock()
		native.mode = ""
		native.version = ""
		native.mu.Unlock()
		entry, err = driver.Stat(ctx, storage.Path(special))
		if err != nil || len(entry.Version) != 0 {
			t.Fatalf("ETag/mtime incorrectly pinned: %+v %v", entry, err)
		}
		_, err = driver.Stat(ctx, "absent")
		code(t, err, "not_found")
		_, err = driver.List(ctx, "absent", storage.PageRequest{})
		code(t, err, "not_found")
		_, err = driver.Open(ctx, "absent", storage.ReadOptions{})
		code(t, err, "not_found")
		_, err = driver.Open(ctx, "notes", storage.ReadOptions{})
		code(t, err, "not_file")
		_, err = driver.List(ctx, "zero", storage.PageRequest{})
		code(t, err, "not_folder")
	})
	t.Run("invalid_ranges_and_provider_range_rejections", func(t *testing.T) {
		native, _, driver := setup(t)
		ctx := context.Background()
		for _, opts := range []storage.ReadOptions{{Offset: -1}, {Length: ptr(0)}, {Length: ptr(-1)}, {Offset: math.MaxInt64, Length: ptr(1)}, {Offset: 10}, {Offset: 11, Length: ptr(1)}} {
			_, err := driver.Open(ctx, storage.Path(special), opts)
			code(t, err, "invalid_range")
		}
		for _, mode := range []string{"ignore_range", "bad_range_start", "bad_range_length", "bad_range_total", "short_suffix"} {
			native.mu.Lock()
			native.mode = mode
			native.mu.Unlock()
			opts := storage.ReadOptions{Offset: 2, Length: ptr(4)}
			if mode == "short_suffix" {
				opts.Length = nil
			}
			_, err := driver.Open(ctx, storage.Path(special), opts)
			code(t, err, "invalid_backend_response")
		}
		native.mu.Lock()
		native.mode = ""
		native.mu.Unlock()
		_, err := driver.Open(ctx, "zero", storage.ReadOptions{Length: ptr(1)})
		code(t, err, "invalid_range")
	})
	t.Run("cursor_binding_paths_and_roots", func(t *testing.T) {
		native, factory, driver := setup(t)
		ctx := context.Background()
		page, err := driver.List(ctx, "", storage.PageRequest{Limit: 1})
		if err != nil || page.NextCursor == "" {
			t.Fatalf("first page: %+v %v", page, err)
		}
		bob, err := factory.Open(ctx, binding("bob"))
		if err != nil {
			t.Fatal(err)
		}
		defer bob.Close()
		_, err = bob.List(ctx, "", storage.PageRequest{Cursor: page.NextCursor})
		code(t, err, "invalid_cursor")
		_, err = driver.List(ctx, "notes", storage.PageRequest{Cursor: page.NextCursor})
		code(t, err, "invalid_cursor")
		_, err = driver.List(ctx, "", storage.PageRequest{Cursor: "not-base64"})
		code(t, err, "invalid_cursor")
		changed := binding("alice")
		changed.Generation++
		other, err := factory.Open(ctx, changed)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		_, err = other.List(ctx, "", storage.PageRequest{Cursor: page.NextCursor})
		code(t, err, "invalid_cursor")
		for _, root := range []string{"drive/alice", "drive/alice/staging", "drive", "drive/alice/../system", "/system"} {
			cfg := binding("alice")
			cfg.SystemRoot = root
			_, err = factory.Open(ctx, cfg)
			code(t, err, "invalid_config")
		}
		invalid := binding("alice")
		invalid.OwnerID = "../bob"
		_, err = factory.Open(ctx, invalid)
		code(t, err, "invalid_config")
		for _, path := range []storage.Path{"../bob/private", "/private", "notes//a", "notes/./a", "notes/../a", "notes\\a"} {
			_, err = driver.Stat(ctx, path)
			code(t, err, "invalid_name")
		}
		native.mu.Lock()
		before := native.requests
		native.mu.Unlock()
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = driver.List(cancelled, "", storage.PageRequest{})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		_, err = driver.Open(cancelled, storage.Path(special), storage.ReadOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		native.mu.Lock()
		after := native.requests
		native.mu.Unlock()
		if before != after {
			t.Fatal("cancelled calls reached native service")
		}
		got, err := bob.List(ctx, "", storage.PageRequest{})
		if err != nil || len(got.Entries) != 1 || got.Entries[0].Path != "private" {
			t.Fatalf("owner binding: %+v %v", got, err)
		}
	})
	t.Run("truncated_body_and_inflight_cancellation", func(t *testing.T) {
		native, _, driver := setup(t)
		native.mu.Lock()
		native.mode = "short_body"
		native.mu.Unlock()
		read, err := driver.Open(context.Background(), storage.Path(special), storage.ReadOptions{Offset: 2, Length: ptr(4)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(read.Body)
		read.Body.Close()
		if err == nil {
			t.Fatal("truncated native body was accepted")
		}
		started, cancelled := make(chan struct{}), make(chan struct{})
		native.mu.Lock()
		native.mode, native.started, native.cancelled = "cancel_get", started, cancelled
		native.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		result := make(chan error, 1)
		go func() {
			read, err := driver.Open(ctx, storage.Path(special), storage.ReadOptions{})
			if err == nil {
				read.Body.Close()
			}
			result <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("native GET did not start")
		}
		cancel()
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled GET: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("native GET ignored cancellation")
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("native server did not observe cancellation")
		}
	})
	t.Run("driver_close_isolates_lifetimes", func(t *testing.T) {
		native, factory, driver := setup(t)
		bob, err := factory.Open(context.Background(), binding("bob"))
		if err != nil {
			t.Fatal(err)
		}
		defer bob.Close()
		started, cancelled := make(chan struct{}), make(chan struct{})
		native.mu.Lock()
		native.mode, native.started, native.cancelled = "cancel_get", started, cancelled
		native.mu.Unlock()
		result := make(chan error, 1)
		go func() {
			read, err := driver.Open(context.Background(), storage.Path(special), storage.ReadOptions{})
			if err == nil {
				read.Body.Close()
			}
			result <- err
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("native GET did not start")
		}
		if err := driver.Close(); err != nil {
			t.Fatal(err)
		}
		if err := driver.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("close did not cancel request")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("close did not stop native request")
		}
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("native server did not observe close")
		}
		_, err = driver.Stat(context.Background(), "")
		code(t, err, "closed")
		_, err = driver.List(context.Background(), "", storage.PageRequest{})
		code(t, err, "closed")
		_, err = driver.Open(context.Background(), storage.Path(special), storage.ReadOptions{})
		code(t, err, "closed")
		native.mu.Lock()
		native.mode = ""
		native.mu.Unlock()
		read, err := bob.Open(context.Background(), "private", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(read.Body)
		read.Body.Close()
		if err != nil || string(data) != "bob" {
			t.Fatalf("other root affected by close: %q %v", data, err)
		}
	})
	t.Run("malformed_native_metadata", func(t *testing.T) {
		native, _, driver := setup(t)
		native.mu.Lock()
		native.mode = "bad_modified"
		native.mu.Unlock()
		_, err := driver.List(context.Background(), "", storage.PageRequest{})
		code(t, err, "invalid_backend_response")
	})
	t.Run("empty_native_segment", func(t *testing.T) {
		native, _, driver := setup(t)
		native.mu.Lock()
		native.objects = map[string][]byte{"drive/alice//child": []byte("hidden")}
		native.mu.Unlock()
		_, err := driver.List(context.Background(), "", storage.PageRequest{})
		code(t, err, "invalid_name")
	})
	t.Run("collisions_and_incomplete_native_pages", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			objects map[string][]byte
		}{{"file_folder", map[string][]byte{"drive/alice/same": []byte("file"), "drive/alice/same/child": []byte("child")}}, {"unicode", map[string][]byte{"drive/alice/é": {}, "drive/alice/e\u0301": {}}}} {
			t.Run(test.name, func(t *testing.T) {
				native, _, driver := setup(t)
				native.mu.Lock()
				native.objects = test.objects
				native.mu.Unlock()
				_, err := driver.List(context.Background(), "", storage.PageRequest{Limit: 1})
				code(t, err, "ambiguous_path")
				_, err = driver.Stat(context.Background(), "same")
				code(t, err, "ambiguous_path")
			})
		}
		for _, mode := range []string{"missing_cursor", "repeat_cursor", "empty_truncated", "empty_advancing"} {
			t.Run(mode, func(t *testing.T) {
				native, _, driver := setup(t)
				native.mu.Lock()
				native.mode = mode
				native.mu.Unlock()
				_, err := driver.List(context.Background(), "", storage.PageRequest{})
				code(t, err, "incomplete_listing")
			})
		}
	})
}
func ptr(value int64) *int64 { return &value }
func code(t *testing.T, err error, want string) {
	t.Helper()
	if !storage.IsCode(err, want) {
		t.Fatalf("error = %v, want storage %s", err, want)
	}
}

func (f *nativeFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	if f.mode == "cancel_get" && r.Method == http.MethodGet && !r.URL.Query().Has("prefix") {
		started, cancelled := f.started, f.cancelled
		f.mu.Unlock()
		close(started)
		<-r.Context().Done()
		close(cancelled)
		return
	}
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") == "" {
		http.Error(w, "SDK authentication missing", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	key = strings.TrimPrefix(key, "test-bucket/")
	if r.URL.Query().Has("prefix") {
		f.listRequests++
		f.list(w, r)
		return
	}
	data, exists := f.objects[key]
	if !exists {
		nativeError(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	version := f.version
	if selected := r.URL.Query().Get("versionId"); selected != "" && selected != version && f.mode != "ignore_version" {
		nativeError(w, http.StatusNotFound, "NoSuchVersion")
		return
	}
	if f.mode == "precondition" && r.Method == http.MethodGet {
		nativeError(w, http.StatusPreconditionFailed, "PreconditionFailed")
		return
	}
	modified := fixedTime
	mediaType := "text/plain"
	if f.mode == "get_changed" && r.Method == http.MethodGet {
		data = []byte("abcdefghijkl")
		version = "new-version"
		modified = fixedTime.Add(time.Hour)
		mediaType = "application/changed"
	}
	if version != "" {
		w.Header().Set("x-amz-version-id", version)
		w.Header().Set("x-cos-version-id", version)
	}
	w.Header().Set("ETag", `"same-etag-not-proof"`)
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
	status := http.StatusOK
	if byteRange := r.Header.Get("Range"); byteRange != "" && f.mode != "ignore_range" {
		bounds := strings.Split(strings.TrimPrefix(byteRange, "bytes="), "-")
		if len(bounds) != 2 {
			nativeError(w, 416, "InvalidRange")
			return
		}
		start, _ := strconv.ParseInt(bounds[0], 10, 64)
		end := int64(len(data)) - 1
		if bounds[1] != "" {
			end, _ = strconv.ParseInt(bounds[1], 10, 64)
		}
		if start >= int64(len(data)) || start < 0 || end < start {
			nativeError(w, 416, "InvalidRange")
			return
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		total := len(data)
		data = data[start : end+1]
		status = 206
		switch f.mode {
		case "bad_range_start":
			start++
			end++
		case "bad_range_length":
			end++
		case "bad_range_total":
			total = int(end)
		case "short_suffix":
			end--
			data = data[:len(data)-1]
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		if f.mode == "short_body" && len(data) > 1 {
			data = data[:1]
		}
		w.Write(data)
	}
}
func nativeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>fixture</Message></Error>", code)
}

type listObject struct {
	Key          string
	Size         int64
	LastModified string
	ETag         string
}
type prefixXML struct{ Prefix string }
type listXML struct {
	XMLName               xml.Name `xml:"ListBucketResult"`
	Name                  string
	Prefix                string
	Delimiter             string
	MaxKeys               int
	KeyCount              int
	IsTruncated           bool
	NextMarker            string       `xml:",omitempty"`
	NextContinuationToken string       `xml:",omitempty"`
	EncodingType          string       `xml:",omitempty"`
	Contents              []listObject `xml:"Contents"`
	CommonPrefixes        []prefixXML  `xml:"CommonPrefixes"`
}

func (f *nativeFixture) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	if r.URL.Query().Get("delimiter") != "/" {
		nativeError(w, 400, "MissingDelimiter")
		return
	}
	keys := map[string]bool{}
	for key := range f.objects {
		if strings.HasPrefix(key, prefix) {
			rest := strings.TrimPrefix(key, prefix)
			if slash := strings.IndexByte(rest, '/'); slash >= 0 {
				keys[prefix+rest[:slash+1]] = true
			} else {
				keys[key] = false
			}
		}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	cursor := r.URL.Query().Get("marker")
	v2 := r.URL.Query().Get("list-type") == "2"
	if v2 {
		if token := r.URL.Query().Get("continuation-token"); token != "" {
			raw, err := base64.RawURLEncoding.DecodeString(token)
			if err != nil {
				nativeError(w, 400, "InvalidToken")
				return
			}
			cursor = string(raw)
		}
	}
	start := sort.SearchStrings(sorted, cursor)
	if cursor != "" && start < len(sorted) && sorted[start] == cursor {
		start++
	}
	end := start + 2
	if end > len(sorted) {
		end = len(sorted)
	}
	result := listXML{Name: "test-bucket", Prefix: prefix, Delimiter: "/", MaxKeys: 2, KeyCount: end - start, IsTruncated: end < len(sorted)}
	encode := func(key string) string { return key }
	if r.URL.Query().Get("encoding-type") == "url" {
		result.EncodingType = "url"
		encode = func(key string) string { return strings.ReplaceAll(url.QueryEscape(key), "+", "%20") }
	}
	for _, key := range sorted[start:end] {
		if keys[key] {
			result.CommonPrefixes = append(result.CommonPrefixes, prefixXML{encode(key)})
		} else {
			result.Contents = append(result.Contents, listObject{Key: encode(key), Size: int64(len(f.objects[key])), LastModified: fixedTime.Format(time.RFC3339), ETag: `"etag"`})
		}
	}
	if result.IsTruncated {
		next := sorted[end-1]
		if f.mode == "repeat_cursor" && cursor != "" {
			next = cursor
		}
		if v2 {
			result.NextContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(next))
		} else {
			result.NextMarker = next
		}
	}
	if f.mode == "missing_cursor" {
		result.IsTruncated = true
		result.NextMarker = ""
		result.NextContinuationToken = ""
	}
	if f.mode == "bad_modified" && len(result.Contents) > 0 {
		result.Contents[0].LastModified = "not-a-time"
	}
	if f.mode == "empty_truncated" || f.mode == "empty_advancing" {
		result.IsTruncated = true
		result.NextMarker = ""
		result.NextContinuationToken = ""
		if f.mode == "empty_advancing" {
			result.NextMarker = "next"
			result.NextContinuationToken = "next"
		}
		result.Contents = nil
		result.CommonPrefixes = nil
	}
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(result)
}
