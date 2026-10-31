package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/unicode/norm"
)

type filesReadTestDriver struct {
	entries      map[storage.Path]storage.Entry
	statErrors   map[storage.Path]error
	pageSize     int
	listCalls    int
	statCalls    int
	descriptor   storage.Descriptor
	listOverride func(storage.Path, storage.PageRequest) (storage.Page, error)
}

func (d *filesReadTestDriver) Describe() storage.Descriptor { return d.descriptor }
func (d *filesReadTestDriver) Close() error                 { return nil }
func (d *filesReadTestDriver) Open(context.Context, storage.Path, storage.ReadOptions) (storage.Read, error) {
	return storage.Read{}, errors.New("unexpected content read")
}
func (d *filesReadTestDriver) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	d.statCalls++
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	if err := d.statErrors[p]; err != nil {
		return storage.Entry{}, err
	}
	for key, entry := range d.entries {
		if norm.NFC.String(string(key)) == norm.NFC.String(string(p)) {
			return entry, nil
		}
	}
	return storage.Entry{}, storage.NewError("not_found", p, nil)
}
func (d *filesReadTestDriver) List(ctx context.Context, p storage.Path, q storage.PageRequest) (storage.Page, error) {
	d.listCalls++
	if err := ctx.Err(); err != nil {
		return storage.Page{}, err
	}
	if d.listOverride != nil {
		return d.listOverride(p, q)
	}
	var entries []storage.Entry
	for _, e := range d.entries {
		relative := string(e.Path)
		parent := ""
		if i := strings.LastIndex(relative, "/"); i >= 0 {
			parent = relative[:i]
		}
		if relative != "" && storage.Path(parent) == p {
			entries = append(entries, e)
		}
	}
	// Deliberately use a different order from the public folders/NFC/name sort.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path > entries[j].Path })
	offset := 0
	if q.Cursor != "" {
		if _, err := fmt.Sscanf(q.Cursor, "private-page-%d", &offset); err != nil {
			return storage.Page{}, err
		}
	}
	count := d.pageSize
	if count == 0 || count > q.Limit {
		count = q.Limit
	}
	end := offset + count
	if end > len(entries) {
		end = len(entries)
	}
	page := storage.Page{Entries: entries[offset:end]}
	if end < len(entries) {
		page.NextCursor = fmt.Sprintf("private-page-%d", end)
	}
	return page, nil
}

type filesReadTestIndex struct {
	rows          []nodes.Row
	owner, prefix string
	limit, calls  int
}

func (i *filesReadTestIndex) Live(_ context.Context, p, owner string) (*nodes.Row, error) {
	for j := range i.rows {
		if i.rows[j].Path == p {
			return &i.rows[j], nil
		}
	}
	return nil, nil
}
func (i *filesReadTestIndex) FileReadRecords(_ context.Context, owner, prefix string, limit int) ([]nodes.Row, error) {
	i.owner = owner
	i.prefix = prefix
	i.limit = limit
	i.calls++
	return i.rows, nil
}

func filesReadFixture() (*Server, *fileRequest, *filesReadTestDriver, *filesReadTestIndex) {
	d := &filesReadTestDriver{entries: map[storage.Path]storage.Entry{"": {Path: "", Kind: storage.Folder}}, statErrors: make(map[storage.Path]error), pageSize: 2, descriptor: storage.Descriptor{RangeRead: true, Names: storage.NameRules{CaseSensitive: true, MaxSegmentBytes: 255, MaxPathBytes: 4096, MaxDepth: 256}}}
	index := &filesReadTestIndex{}
	s := &Server{cfg: &config.Config{AppSecret: "stable-test-secret"}, filesIndex: index}
	f := &fileRequest{Principal: &auth.Principal{User: &auth.User{ID: "owner-a"}}, Driver: d, Scope: "/", Namespace: "private-binding", ContentNamespace: "native-root-owner-a"}
	return s, f, d, index
}
func filesReadAdd(d *filesReadTestDriver, p string, kind storage.Kind, size int64) storage.Entry {
	e := storage.Entry{Path: storage.Path(p), Kind: kind, Size: size, ModifiedAt: time.Date(2026, 10, 30, 8, 9, 10, 0, time.UTC), MediaType: "text/plain", NativeRef: []byte("secret-provider-reference")}
	if kind == storage.Folder {
		e.MediaType = ""
		e.Size = 0
	}
	d.entries[e.Path] = e
	return e
}
func filesReadRow(p string, folder bool) nodes.Row {
	return nodes.Row{Node: nodes.Node{ID: "private-database-id", Path: p, Name: "untrusted-index-name", IsDir: folder, Size: 987654, CreatedAt: "2025-01-01T01:02:03Z", UpdatedAt: "2026-01-01T01:02:03Z"}, Key: "secret-provider-key"}
}
func filesReadRequest(query string) *http.Request {
	return httptest.NewRequest("GET", "/api/files?"+query, nil)
}
func filesReadCall(t *testing.T, s *Server, f *fileRequest, handler fileHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	if err := handler(w, filesReadRequest(query), f); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("metadata cache policy=%q", w.Header().Get("Cache-Control"))
	}
	return w
}
func filesReadCollection(t *testing.T, w *httptest.ResponseRecorder) fc.Collection[fc.Metadata] {
	t.Helper()
	var result fc.Collection[fc.Metadata]
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Items == nil {
		t.Fatal("items must be an array")
	}
	return result
}
func filesReadProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var problem *fc.Problem
	if !errors.As(err, &problem) || problem.Status != status || problem.Code != code {
		t.Fatalf("error=%v, want %d %s", err, status, code)
	}
}

func TestFilesReadMetadataNativeAuthorityAndRawSpelling(t *testing.T) {
	s, f, d, index := filesReadFixture()
	native := " Notes /Cafe\u0301 +%2B#?.txt "
	filesReadAdd(d, " Notes ", storage.Folder, 0)
	entry := filesReadAdd(d, native, storage.File, 7)
	index.rows = []nodes.Row{filesReadRow(native, false)}
	w := filesReadCall(t, s, f, s.handleFilesMetadata, url.Values{"path": {"/" + native}}.Encode())
	var got fc.Metadata
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Path != fc.Path("/"+native) || got.ParentPath != "/ Notes " || got.Name != "Cafe\u0301 +%2B#?.txt " {
		t.Fatalf("native spelling was changed: %+v", got)
	}
	if got.Size == nil || *got.Size != 7 || got.MediaType == nil || *got.MediaType != "text/plain" {
		t.Fatalf("native attributes lost: %+v", got)
	}
	if got.ContentRevision != nil {
		t.Fatal("unversioned native content received a revision")
	}
	if got.CreatedAt.Format(time.RFC3339) != index.rows[0].CreatedAt || got.UpdatedAt.Format(time.RFC3339) != index.rows[0].UpdatedAt {
		t.Fatalf("index timestamps lost: %+v", got)
	}
	if got.SourceModifiedAt == nil || !got.SourceModifiedAt.Equal(entry.ModifiedAt) {
		t.Fatalf("native source timestamp lost: %+v", got)
	}
	for _, secret := range []string{"private-database-id", "secret-provider", `"ok"`, `"data"`, `"fileId"`, `"key"`, `"etag"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("representation exposes %q", secret)
		}
	}
	if !strings.HasPrefix(got.Revision, "nr_") {
		t.Fatalf("revision=%q", got.Revision)
	}
	// A native file remains visible when it has never been indexed.
	index.rows = nil
	w = filesReadCall(t, s, f, s.handleFilesMetadata, url.Values{"path": {"/" + native}}.Encode())
	if !strings.Contains(w.Body.String(), `"size":7`) {
		t.Fatal("unindexed native entry disappeared")
	}
	delete(d.entries, storage.Path(native))
	err := s.handleFilesMetadata(httptest.NewRecorder(), filesReadRequest(url.Values{"path": {"/" + native}}.Encode()), f)
	if !storage.IsCode(err, "not_found") {
		t.Fatalf("stale index resurrected missing file: %v", err)
	}
}

func TestFilesReadRevisionsObserveMembershipAndAncestorMoves(t *testing.T) {
	s, f, d, _ := filesReadFixture()
	folder := filesReadAdd(d, "Photos", storage.Folder, 0)
	file := filesReadAdd(d, "Photos/a.jpg", storage.File, 8)
	file.Version = []byte("native-generation-7")
	d.entries[file.Path] = file
	get := func(e storage.Entry) fc.Metadata {
		t.Helper()
		m, err := s.filesMetadata(context.Background(), f, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a := get(folder)
	filesReadAdd(d, "Photos/b.jpg", storage.File, 9)
	b := get(folder)
	if a.Revision == b.Revision {
		t.Fatal("direct membership did not change folder revision with unchanged folder mtime")
	}
	nested := filesReadAdd(d, "Photos/nested", storage.Folder, 0)
	before := get(folder)
	filesReadAdd(d, string(nested.Path)+"/inside.txt", storage.File, 3)
	after := get(folder)
	if before.Revision != after.Revision {
		t.Fatal("descendant membership became a recursive folder revision")
	}
	original := get(file)
	file.Path = "Archive/a.jpg"
	moved := get(file)
	if original.Revision == moved.Revision {
		t.Fatal("ancestor move did not change child metadata revision")
	}
	// P2 has no durable move anchor. Observing the same per-path native version
	// at another path cannot establish a shared content generation or identity.
	if original.ContentRevision == nil || moved.ContentRevision == nil || *original.ContentRevision == *moved.ContentRevision {
		t.Fatal("different native paths with the same version shared a content revision")
	}
	clone := *f
	clone.Namespace = "other-principal-binding"
	clone.Principal = &auth.Principal{User: f.Principal.User, Token: &auth.Token{ID: "other-app", CanRead: true}}
	same, err := s.filesMetadata(context.Background(), &clone, file, nil)
	if err != nil {
		t.Fatal(err)
	}
	if same.Revision != moved.Revision || same.ContentRevision == nil || *same.ContentRevision != *moved.ContentRevision {
		t.Fatal("the same native representation depends on the credential principal")
	}
	if original.Path == moved.Path || original.ParentPath == moved.ParentPath {
		t.Fatal("different observed native locations retained an application identity")
	}
	if after.Size != nil || after.MediaType != nil || after.Checksum != nil || after.ContentRevision != nil {
		t.Fatalf("folder has file-only fields: %+v", after)
	}
}

func TestFilesReadListDriverPaginationStableSortAndConcurrentWrites(t *testing.T) {
	s, f, d, _ := filesReadFixture()
	filesReadAdd(d, "folder-Z", storage.Folder, 0)
	filesReadAdd(d, "folder-A", storage.Folder, 0)
	filesReadAdd(d, "z.txt", storage.File, 1)
	filesReadAdd(d, "A.txt", storage.File, 1)
	filesReadAdd(d, "e\u0301.txt", storage.File, 1)
	filesReadAdd(d, "éz.txt", storage.File, 1)
	first := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2F&limit=2"))
	if len(first.Items) != 2 || first.Items[0].Path != "/folder-A" || first.Items[1].Path != "/folder-Z" || first.NextCursor == nil {
		t.Fatalf("first=%+v", first)
	}
	if d.listCalls < 3 {
		t.Fatal("driver pagination was not exhausted")
	}
	if strings.Contains(*first.NextCursor, "private-page") {
		t.Fatal("driver cursor exposed")
	}
	// Insert before and after the keyset. A new server with the same secret can
	// continue; ordinary writes must not invalidate the whole query.
	filesReadAdd(d, "folder-0", storage.Folder, 0)
	filesReadAdd(d, "B.txt", storage.File, 1)
	restarted := &Server{cfg: &config.Config{AppSecret: s.cfg.AppSecret}, filesIndex: s.filesIndex}
	query := url.Values{"path": {"/"}, "limit": {"1000"}, "cursor": {*first.NextCursor}}.Encode()
	second := filesReadCollection(t, filesReadCall(t, restarted, f, restarted.handleFilesList, query))
	want := []fc.Path{"/A.txt", "/B.txt", "/z.txt", "/e\u0301.txt", "/éz.txt"}
	if len(second.Items) != len(want) {
		t.Fatalf("second=%+v", second)
	}
	for i, p := range want {
		if second.Items[i].Path != p {
			t.Fatalf("item %d=%q want %q", i, second.Items[i].Path, p)
		}
	}
	if second.NextCursor != nil {
		t.Fatal("terminal page has a cursor")
	}
}

func TestFilesReadCursorRejectsTamperingAndDifferentBindings(t *testing.T) {
	s, f, d, _ := filesReadFixture()
	filesReadAdd(d, "a", storage.File, 1)
	filesReadAdd(d, "b", storage.File, 1)
	first := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2F&limit=1"))
	token := *first.NextCursor
	for _, test := range []struct {
		name   string
		change func(*fileRequest, *filesReadQuery)
		route  string
	}{
		{"namespace", func(f *fileRequest, q *filesReadQuery) { f.Namespace = "another-root" }, "list"},
		{"principal", func(f *fileRequest, q *filesReadQuery) {
			f.Principal = &auth.Principal{User: f.Principal.User, Token: &auth.Token{ID: "another", CanRead: true}}
		}, "list"},
		{"scope", func(f *fileRequest, q *filesReadQuery) { f.Scope = "/a" }, "list"},
		{"query", func(f *fileRequest, q *filesReadQuery) { q.Keyword = "changed" }, "list"},
		{"path", func(f *fileRequest, q *filesReadQuery) { q.Path = "/a" }, "list"},
		{"kind", func(f *fileRequest, q *filesReadQuery) { q.Kind = "file" }, "list"},
		{"media", func(f *fileRequest, q *filesReadQuery) { q.MediaType = "text/plain" }, "list"},
		{"route", func(f *fileRequest, q *filesReadQuery) {}, "search"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clone := *f
			q := filesReadQuery{Path: "/", Limit: 1}
			test.change(&clone, &q)
			_, err := s.filesDecodeCursor(token, s.filesCursorBinding(&clone, q, test.route))
			filesReadProblem(t, err, 400, "invalid_request")
		})
	}
	q := filesReadQuery{Path: "/", Limit: 1}
	binding := s.filesCursorBinding(f, q, "list")
	altered := []byte(token)
	if altered[len(altered)/2] == 'A' {
		altered[len(altered)/2] = 'B'
	} else {
		altered[len(altered)/2] = 'A'
	}
	_, err := s.filesDecodeCursor(string(altered), binding)
	filesReadProblem(t, err, 400, "invalid_request")
	rotated := &Server{cfg: &config.Config{AppSecret: "different-deployment-secret"}}
	_, err = rotated.filesDecodeCursor(token, binding)
	filesReadProblem(t, err, 400, "invalid_request")
	if strings.Contains(token, "owner-a") || strings.Contains(token, "binding") {
		t.Fatal("cursor leaked internal references")
	}
}

func TestFilesReadSearchScopesBeforePaginationAndUsesVisibleNativeFields(t *testing.T) {
	s, f, d, index := filesReadFixture()
	f.Scope = "/Docs"
	f.Principal.Token = &auth.Token{ID: "reader", CanRead: true}
	filesReadAdd(d, "Docs", storage.Folder, 0)
	for _, p := range []string{"Docs/z.TXT", "Docs/A.txt", "Docs/secret.bin", "Docs-old/outside.txt"} {
		filesReadAdd(d, p, storage.File, 4)
	}
	e := d.entries["Docs/secret.bin"]
	e.MediaType = "image/png"
	d.entries[e.Path] = e
	index.rows = []nodes.Row{filesReadRow("Docs-old/outside.txt", false), filesReadRow("Docs/missing.txt", false), filesReadRow("Docs/z.TXT", false), filesReadRow("Docs/A.txt", false), filesReadRow("Docs/secret.bin", false)}
	first := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "q=DoCs%2F&kind=file&mediaType=text%2F&limit=1"))
	if len(first.Items) != 1 || first.Items[0].Path != "/Docs/A.txt" || first.NextCursor == nil {
		t.Fatalf("first=%+v", first)
	}
	if index.owner != "owner-a" || index.prefix != "Docs" || index.limit != filesReadBound+1 {
		t.Fatalf("index query not owner/subtree bounded: %+v", index)
	}
	q := url.Values{"q": {"DoCs/"}, "kind": {"file"}, "mediaType": {"text/"}, "limit": {"1"}, "cursor": {*first.NextCursor}}.Encode()
	second := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, q))
	if len(second.Items) != 1 || second.Items[0].Path != "/Docs/z.TXT" || second.NextCursor != nil {
		t.Fatalf("second=%+v", second)
	}
	hidden := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "q=untrusted-index-name"))
	if len(hidden.Items) != 0 {
		t.Fatal("search matched an index-only name")
	}
	images := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "mediaType=image%2F*&kind=file"))
	if len(images.Items) != 1 || images.Items[0].Path != "/Docs/secret.bin" {
		t.Fatalf("media filter=%+v", images)
	}
	err := s.handleFilesSearch(httptest.NewRecorder(), filesReadRequest("path=%2FDocs-old"), f)
	filesReadProblem(t, err, 404, "not_found")
}

func TestFilesReadIndexedQueriesReportNativeNameFailures(t *testing.T) {
	for _, code := range []string{"ambiguous_path", "invalid_name", "outside_root"} {
		t.Run(code, func(t *testing.T) {
			s, f, d, index := filesReadFixture()
			filesReadAdd(d, "a.txt", storage.File, 1)
			index.rows = []nodes.Row{filesReadRow("a.txt", false)}
			d.listOverride = func(p storage.Path, _ storage.PageRequest) (storage.Page, error) {
				return storage.Page{}, storage.NewError(code, p, nil)
			}
			for _, handler := range []fileHandler{s.handleFilesSearch, s.handleFilesMe} {
				w := httptest.NewRecorder()
				err := handler(w, filesReadRequest(""), f)
				if !storage.IsCode(err, code) {
					t.Fatalf("native %s was hidden: %v", code, err)
				}
				if w.Body.Len() != 0 {
					t.Fatalf("native failure produced partial success: %s", w.Body.String())
				}
			}
		})
	}
}

func TestFilesReadPermissionsAndInvalidQueries(t *testing.T) {
	s, f, d, index := filesReadFixture()
	filesReadAdd(d, "Docs", storage.Folder, 0)
	f.Scope = "/Docs"
	for _, token := range []*auth.Token{{ID: "write-only", CanWrite: true}, {ID: "events-only", CanEvents: true}} {
		f.Principal.Token = token
		for _, handler := range []fileHandler{s.handleFilesMetadata, s.handleFilesList, s.handleFilesSearch, s.handleFilesMe, s.handleFilesCapabilities} {
			err := handler(httptest.NewRecorder(), filesReadRequest("path=%2FDocs"), f)
			filesReadProblem(t, err, 403, "forbidden")
		}
	}
	if index.calls != 0 {
		t.Fatal("index accessed without read permission")
	}
	f.Principal.Token = nil
	for _, query := range []string{"path=%2FDocs&limit=0", "path=%2FDocs&limit=1001", "path=%2FDocs&limit=x", "path=%2FDocs&limit=", "path=%2FDocs&kind=dir", "path=%2FDocs&path=%2F", "path=%2FDocs&cursor=a&cursor=b", "path=%zz"} {
		_, err := parseFilesReadQuery(filesReadRequest(query), f.Scope, true)
		filesReadProblem(t, err, 400, "invalid_request")
	}
	_, err := parseFilesReadQuery(filesReadRequest("path=%2FDocs%2F.."), f.Scope, true)
	filesReadProblem(t, err, 400, "invalid_path")
	_, err = parseFilesReadQuery(filesReadRequest(""), f.Scope, true)
	filesReadProblem(t, err, 400, "invalid_request")
	q, err := parseFilesReadQuery(filesReadRequest(""), f.Scope, false)
	if err != nil || q.Path != "/Docs" || q.Limit != 200 {
		t.Fatalf("defaults=%+v %v", q, err)
	}
}

func TestFilesReadMeScopedIndexedUsageAndTruthfulCapabilities(t *testing.T) {
	s, f, d, index := filesReadFixture()
	f.Scope = "/Docs"
	f.CSRFToken = "csrf-cookie-bootstrap"
	filesReadAdd(d, "Docs", storage.Folder, 0)
	filesReadAdd(d, "Docs/a", storage.File, 7)
	filesReadAdd(d, "Other/huge", storage.File, 9999)
	index.rows = []nodes.Row{filesReadRow("Docs", true), filesReadRow("Docs/a", false), filesReadRow("Docs/missing", false), filesReadRow("Other/huge", false)}
	w := filesReadCall(t, s, f, s.handleFilesMe, "")
	var me struct {
		Usage struct {
			Bytes, Files int64
			Indexed      bool
		}
		Quota, Reservations any
		CSRFToken           string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Usage.Bytes != 7 || me.Usage.Files != 1 || !me.Usage.Indexed || me.Quota != nil || me.Reservations != nil || me.CSRFToken != "csrf-cookie-bootstrap" {
		t.Fatalf("me=%s", w.Body.String())
	}
	w = filesReadCall(t, s, f, s.handleFilesCapabilities, "")
	var capabilities map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"write", "uploads", "thumbnails", "pinnedRead", "strongContentValidators"} {
		if capabilities[field] != false {
			t.Errorf("%s=%v", field, capabilities[field])
		}
	}
	if capabilities["range"] != true || capabilities["read"] != true || capabilities["path"] != "/Docs" {
		t.Fatalf("capabilities=%s", w.Body.String())
	}
	if capabilities["reservations"] != nil || capabilities["maxFileBytes"] != nil || capabilities["quota"] != nil {
		t.Fatalf("unknown values must be null: %s", w.Body.String())
	}
	for _, forbidden := range []string{"provider", "secret", "owner-a", "COS", "bucket", "http:"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("capabilities expose %q", forbidden)
		}
	}
}

func TestFilesReadExplicitResourceBoundsAndBadDriverPagination(t *testing.T) {
	s, f, d, index := filesReadFixture()
	index.rows = make([]nodes.Row, filesReadBound+1)
	err := s.handleFilesSearch(httptest.NewRecorder(), filesReadRequest(""), f)
	filesReadProblem(t, err, 503, "resource_limit")
	d.listOverride = func(storage.Path, storage.PageRequest) (storage.Page, error) {
		return storage.Page{Entries: make([]storage.Entry, filesReadBound+1)}, nil
	}
	err = s.handleFilesList(httptest.NewRecorder(), filesReadRequest("path=%2F"), f)
	filesReadProblem(t, err, 503, "resource_limit")
	d.listOverride = func(storage.Path, storage.PageRequest) (storage.Page, error) {
		return storage.Page{NextCursor: "never-progresses"}, nil
	}
	err = s.handleFilesList(httptest.NewRecorder(), filesReadRequest("path=%2F"), f)
	filesReadProblem(t, err, 502, "storage_failure")
	d.listOverride = func(storage.Path, storage.PageRequest) (storage.Page, error) {
		return storage.Page{Entries: []storage.Entry{{Path: "../escaped", Kind: storage.File}}}, nil
	}
	err = s.handleFilesList(httptest.NewRecorder(), filesReadRequest("path=%2F"), f)
	filesReadProblem(t, err, 502, "storage_failure")
}

func TestFilesReadLocalTreeWithoutIndex(t *testing.T) {
	s, f, _, _ := filesReadFixture()
	s.filesIndex = nil
	base := t.TempDir()
	cfg := storage.Config{OwnerID: "owner-a", ConnectionID: "read-handler-test", Generation: 1, ContentRoot: filepath.Join(base, "content"), SystemRoot: filepath.Join(base, "system")}
	for _, root := range []string{cfg.ContentRoot, cfg.SystemRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	spelling := "cafe\u0301 +%2B#?.txt"
	if err := os.WriteFile(filepath.Join(cfg.ContentRoot, spelling), []byte("native bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "empty folder"), 0700); err != nil {
		t.Fatal(err)
	}
	factory, err := localstorage.NewFactory(localstorage.Options{CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := driver.Close(); err != nil {
			t.Error(err)
		}
	})
	f.Driver = driver
	// Observe spelling independently: macOS may normalize the native filename.
	names, err := os.ReadDir(cfg.ContentRoot)
	if err != nil {
		t.Fatal(err)
	}
	actual := ""
	for _, entry := range names {
		if !entry.IsDir() {
			actual = entry.Name()
		}
	}
	w := filesReadCall(t, s, f, s.handleFilesMetadata, url.Values{"path": {"/" + actual}}.Encode())
	var metadata fc.Metadata
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Path != fc.Path("/"+actual) || metadata.ContentRevision != nil || metadata.Size == nil || *metadata.Size != 12 {
		t.Fatalf("local metadata=%+v", metadata)
	}
	result := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2F"))
	if len(result.Items) != 2 || result.Items[0].Path != "/empty folder" || result.Items[1].Path != fc.Path("/"+actual) {
		t.Fatalf("local list=%+v", result)
	}
	capabilities := filesReadCall(t, s, f, s.handleFilesCapabilities, "")
	if !strings.Contains(capabilities.Body.String(), `"pinnedRead":false`) || !strings.Contains(capabilities.Body.String(), `"range":true`) {
		t.Fatalf("local capabilities=%s", capabilities.Body.String())
	}
}

func TestFilesReadSearchFullPathTieBreaker(t *testing.T) {
	s, f, d, index := filesReadFixture()
	for _, p := range []string{"Z/same.txt", "A/same.txt", "é/same.txt"} {
		filesReadAdd(d, strings.Split(p, "/")[0], storage.Folder, 0)
		filesReadAdd(d, p, storage.File, 1)
		index.rows = append(index.rows, filesReadRow(p, false))
	}
	query := url.Values{"kind": {"file"}, "q": {"same.txt"}, "limit": {"1"}}
	want := []fc.Path{"/A/same.txt", "/Z/same.txt", "/é/same.txt"}
	for i, p := range want {
		page := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, query.Encode()))
		if len(page.Items) != 1 || page.Items[0].Path != p {
			t.Fatalf("page %d=%+v want %q", i, page, p)
		}
		if i == len(want)-1 {
			if page.NextCursor != nil {
				t.Fatal("last page has a cursor")
			}
		} else {
			if page.NextCursor == nil {
				t.Fatal("continuation cursor missing")
			}
			query.Set("cursor", *page.NextCursor)
		}
	}
}

func TestFilesReadListRejectsNormalizedNativeCollisions(t *testing.T) {
	s, f, d, _ := filesReadFixture()
	filesReadAdd(d, "é.txt", storage.File, 1)
	filesReadAdd(d, "e\u0301.txt", storage.File, 1)
	err := s.handleFilesList(httptest.NewRecorder(), filesReadRequest("path=%2F"), f)
	filesReadProblem(t, err, 409, "name_conflict")
}

func TestFilesReadIndexedFlatDirectoryUsesPagesNotFileStats(t *testing.T) {
	for _, endpoint := range []string{"search", "me"} {
		t.Run(endpoint, func(t *testing.T) {
			s, f, d, index := filesReadFixture()
			d.pageSize = 1000
			const count = 2500
			for i := 0; i < count; i++ {
				p := fmt.Sprintf("file-%04d.txt", i)
				filesReadAdd(d, p, storage.File, 2)
				index.rows = append(index.rows, filesReadRow(p, false))
			}
			handler := s.handleFilesSearch
			if endpoint == "me" {
				handler = s.handleFilesMe
			}
			w := filesReadCall(t, s, f, handler, "kind=file&limit=1")
			if d.statCalls != 1 || d.listCalls != 3 {
				t.Fatalf("%d sibling files required %d Stat calls and %d List calls; want one root Stat and three native pages", count, d.statCalls, d.listCalls)
			}
			if endpoint == "me" && !strings.Contains(w.Body.String(), `"bytes":5000`) {
				t.Fatalf("scoped usage=%s", w.Body.String())
			}
		})
	}
}

func TestFilesReadIndexedResolutionValidatesNativeAncestry(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.pageSize = 1000
	filesReadAdd(d, "folder", storage.Folder, 0)
	filesReadAdd(d, "folder/nested", storage.Folder, 0)
	filesReadAdd(d, "folder/nested/a.txt", storage.File, 7)
	// Parent directories may be absent from the index; native ancestry resolves them.
	index.rows = []nodes.Row{filesReadRow("folder/nested/a.txt", false), filesReadRow("missing/phantom.txt", false)}
	page := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "kind=file"))
	if len(page.Items) != 1 || page.Items[0].Path != "/folder/nested/a.txt" || d.statCalls != 1 || d.listCalls != 3 {
		t.Fatalf("page=%+v Stat=%d List=%d", page, d.statCalls, d.listCalls)
	}
	filesReadAdd(d, "blocked", storage.File, 1)
	index.rows = []nodes.Row{filesReadRow("blocked/child.txt", false)}
	err := s.handleFilesSearch(httptest.NewRecorder(), filesReadRequest(""), f)
	if !storage.IsCode(err, "not_folder") {
		t.Fatalf("native file parent accepted: %v", err)
	}
	index.rows = []nodes.Row{filesReadRow("folder/nested/a.txt", true)}
	page = filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, ""))
	if len(page.Items) != 0 {
		t.Fatal("stale indexed kind overrode native kind")
	}
}

func TestFilesReadIndexedDirectoryBudgetIsRequestWide(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.pageSize = 1
	// One root listing consumes 501 pages, then each candidate directory costs one.
	// The request must stop at 1000 pages even though each directory is small.
	for i := 0; i < 501; i++ {
		p := fmt.Sprintf("dir-%04d", i)
		filesReadAdd(d, p, storage.Folder, 0)
		index.rows = append(index.rows, filesReadRow(p+"/missing.txt", false))
	}
	err := s.handleFilesMe(httptest.NewRecorder(), filesReadRequest(""), f)
	filesReadProblem(t, err, 503, "resource_limit")
	if d.listCalls != filesNativePageBound || d.statCalls != 1 {
		t.Fatalf("budget operations Stat=%d List=%d", d.statCalls, d.listCalls)
	}
}

func TestFilesReadIndexedFolderMetadataReusesDirectoryObservations(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.pageSize = 1000
	filesReadAdd(d, "folder", storage.Folder, 0)
	filesReadAdd(d, "folder/a.txt", storage.File, 1)
	index.rows = []nodes.Row{filesReadRow("", true), filesReadRow("folder", true), filesReadRow("folder/a.txt", false)}
	page := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, ""))
	if len(page.Items) != 3 || d.statCalls != 1 || d.listCalls != 2 {
		t.Fatalf("folder revisions rescanned native directories: items=%d Stat=%d List=%d", len(page.Items), d.statCalls, d.listCalls)
	}
	filesReadAdd(d, "folder/e\u0301.txt", storage.File, 1)
	index.rows = []nodes.Row{filesReadRow("folder/é.txt", false)}
	err := s.handleFilesSearch(httptest.NewRecorder(), filesReadRequest(""), f)
	if !storage.IsCode(err, "invalid_name") {
		t.Fatalf("native spelling alias accepted: %v", err)
	}
}

func TestFilesReadListNeverReturnsIndexOnlyEntriesAndEmptyShape(t *testing.T) {
	s, f, _, index := filesReadFixture()
	index.rows = []nodes.Row{filesReadRow("phantom", false)}
	w := filesReadCall(t, s, f, s.handleFilesList, "path=%2F")
	if strings.TrimSpace(w.Body.String()) != `{"items":[],"nextCursor":null}` {
		t.Fatalf("empty list=%s", w.Body.String())
	}
}
