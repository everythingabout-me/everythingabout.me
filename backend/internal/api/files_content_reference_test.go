package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
)

// referenceContentDriver serves the index-addressed read path and counts every call, so a
// test can prove content no longer walks ancestor directories.
type referenceContentDriver struct {
	entry storage.Entry
	data  string
	key   string

	statRefs, opens                int
	options                        []storage.ReadOptions
	traversalStats, traversalOpens int
}

func (d *referenceContentDriver) Describe() storage.Descriptor {
	return storage.Descriptor{RangeRead: true, Names: storage.NameRules{CaseSensitive: true}}
}
func (d *referenceContentDriver) Close() error { return nil }
func (d *referenceContentDriver) Stat(context.Context, storage.Path) (storage.Entry, error) {
	d.traversalStats++
	return d.entry, nil
}
func (d *referenceContentDriver) List(context.Context, storage.Path, storage.PageRequest) (storage.Page, error) {
	panic("content reads must not list directories")
}
func (d *referenceContentDriver) Open(_ context.Context, _ storage.Path, options storage.ReadOptions) (storage.Read, error) {
	d.traversalOpens++
	return d.read(options)
}
func (d *referenceContentDriver) StatReference(_ context.Context, p storage.Path, ref []byte) (storage.Entry, error) {
	d.statRefs++
	if string(ref) != d.key {
		return storage.Entry{}, storage.NewError("stale_reference", p, nil)
	}
	return d.entry, nil
}
func (d *referenceContentDriver) OpenReference(_ context.Context, p storage.Path, ref []byte, options storage.ReadOptions) (storage.Read, error) {
	d.opens++
	if string(ref) != d.key {
		return storage.Read{}, storage.NewError("stale_reference", p, nil)
	}
	d.options = append(d.options, options)
	return d.read(options)
}
func (d *referenceContentDriver) read(options storage.ReadOptions) (storage.Read, error) {
	length := int64(len(d.data)) - options.Offset
	if options.Length != nil {
		length = *options.Length
	}
	if options.Offset < 0 || length < 0 || options.Offset+length > int64(len(d.data)) {
		return storage.Read{}, storage.NewError("invalid_range", "file", nil)
	}
	return storage.Read{
		Body:   io.NopCloser(strings.NewReader(d.data[options.Offset : options.Offset+length])),
		Entry:  d.entry,
		Offset: options.Offset,
		Length: length,
	}, nil
}

func referenceContentRow(p, key string, size int64) nodes.Row {
	name := p
	if index := strings.LastIndexByte(p, '/'); index >= 0 {
		name = p[index+1:]
	}
	return nodes.Row{Node: nodes.Node{Path: p, Name: name, Size: size}, Key: key}
}

func referenceContentFixture(key string) (*referenceContentDriver, *Server, *fileRequest) {
	d := &referenceContentDriver{entry: storage.Entry{Path: "file", Kind: storage.File, NativeRef: []byte(key), Version: []byte("native-etag:abc"), Size: 10, MediaType: "text/plain"}, data: "0123456789", key: key}
	f := &fileRequest{Principal: &auth.Principal{User: &auth.User{ID: "owner-a"}}, Driver: d, Scope: "/", Namespace: "caller-scope", ContentNamespace: "owner-root-config"}
	return d, &Server{cfg: &config.Config{AppSecret: "stable-test-secret"}}, f
}

func runReferenceContent(t *testing.T, s *Server, f *fileRequest, method, path string, headers http.Header) (*httptest.ResponseRecorder, error) {
	t.Helper()
	r := httptest.NewRequest(method, "/api/files/content?"+url.Values{"path": {path}}.Encode(), nil)
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	w := httptest.NewRecorder()
	err := s.handleFilesContent(w, r, f)
	return w, err
}

func TestFilesContentReferenceRangeOpensOnce(t *testing.T) {
	d, s, f := referenceContentFixture("drive/owner-a/file")
	s.filesIndex = &filesReadTestIndex{rows: []nodes.Row{referenceContentRow("file", d.key, 10)}}
	w, err := runReferenceContent(t, s, f, http.MethodGet, "/file", http.Header{"Range": {"bytes=2-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusPartialContent || w.Body.String() != "2345" || w.Header().Get("Content-Range") != "bytes 2-5/10" || w.Header().Get("Content-Length") != "4" {
		t.Fatalf("range response: %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if d.statRefs != 1 || d.opens != 1 {
		t.Fatalf("reference read made %d observations and %d transfers", d.statRefs, d.opens)
	}
	if d.traversalStats != 0 || d.traversalOpens != 0 {
		t.Fatal("range read fell back to path traversal")
	}
	if len(d.options) != 1 || d.options[0].Offset != 2 || d.options[0].Length == nil || *d.options[0].Length != 4 {
		t.Fatalf("transfer did not carry the requested range: %+v", d.options)
	}
	if w.Header().Get("ETag") == "" || !strings.Contains(w.Header().Get("Cache-Control"), "no-cache") {
		t.Fatalf("content validators: %v", w.Header())
	}
}

func TestFilesContentReferenceConditionalSkipsTransfer(t *testing.T) {
	d, s, f := referenceContentFixture("drive/owner-a/file")
	s.filesIndex = &filesReadTestIndex{rows: []nodes.Row{referenceContentRow("file", d.key, 10)}}
	tag := strconv.Quote(*filesContentRevision(f.ContentNamespace, d.entry))
	w, err := runReferenceContent(t, s, f, http.MethodGet, "/file", http.Header{"If-None-Match": {tag}, "Range": {"bytes=2-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("conditional response: %d %q", w.Code, w.Body.String())
	}
	if d.statRefs != 1 || d.opens != 0 {
		t.Fatalf("revalidation made %d observations and %d transfers", d.statRefs, d.opens)
	}
}

func TestFilesContentReferenceHeadOpensNoBody(t *testing.T) {
	d, s, f := referenceContentFixture("drive/owner-a/file")
	s.filesIndex = &filesReadTestIndex{rows: []nodes.Row{referenceContentRow("file", d.key, 10)}}
	w, err := runReferenceContent(t, s, f, http.MethodHead, "/file", nil)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || w.Header().Get("Content-Length") != "10" || w.Header().Get("Accept-Ranges") != "bytes" {
		t.Fatalf("head response: %d %v", w.Code, w.Header())
	}
	if d.statRefs != 1 || d.opens != 0 {
		t.Fatalf("head made %d observations and %d transfers", d.statRefs, d.opens)
	}
}

func TestFilesContentFallsBackWithoutIndexKey(t *testing.T) {
	for _, test := range []struct {
		name string
		rows []nodes.Row
	}{
		{"missing row", nil},
		{"stale key", []nodes.Row{referenceContentRow("file", "drive/owner-a/old-name", 10)}},
		{"indexed folder", []nodes.Row{{Node: nodes.Node{Path: "file", Name: "file", IsDir: true}, Key: "drive/owner-a/file/"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, s, f := referenceContentFixture("drive/owner-a/file")
			s.filesIndex = &filesReadTestIndex{rows: test.rows}
			w, err := runReferenceContent(t, s, f, http.MethodGet, "/file", http.Header{"Range": {"bytes=2-5"}})
			if err != nil {
				t.Fatal(err)
			}
			// Storage stays the authority for bytes: the traversal path answers instead of
			// the request failing while the index catches up.
			if w.Code != http.StatusPartialContent || w.Body.String() != "2345" {
				t.Fatalf("fallback response: %d %q", w.Code, w.Body.String())
			}
			if d.traversalStats != 1 || d.traversalOpens == 0 {
				t.Fatalf("fallback did not use path traversal: %d stats %d opens", d.traversalStats, d.traversalOpens)
			}
		})
	}
}

func TestFilesContentReferenceNativeMissingIsReported(t *testing.T) {
	d, s, f := referenceContentFixture("drive/owner-a/file")
	d.entry = storage.Entry{}
	missing := &missingReferenceDriver{referenceContentDriver: d}
	s.filesIndex = &filesReadTestIndex{rows: []nodes.Row{referenceContentRow("file", d.key, 10)}}
	f.Driver = missing
	w, err := runReferenceContent(t, s, f, http.MethodGet, "/file", nil)
	if err == nil {
		t.Fatalf("missing native object served %d", w.Code)
	}
	if !storage.IsCode(err, "not_found") {
		t.Fatalf("missing native object error: %v", err)
	}
}

type missingReferenceDriver struct{ *referenceContentDriver }

func (d *missingReferenceDriver) StatReference(_ context.Context, p storage.Path, ref []byte) (storage.Entry, error) {
	d.statRefs++
	return storage.Entry{}, storage.NewError("not_found", p, nil)
}
