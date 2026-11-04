package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
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
	openCalls    int
	unavailable  bool
	descriptor   storage.Descriptor
	listOverride func(storage.Path, storage.PageRequest) (storage.Page, error)
}

func (d *filesReadTestDriver) Describe() storage.Descriptor { return d.descriptor }
func (d *filesReadTestDriver) Close() error                 { return nil }
func (d *filesReadTestDriver) Open(context.Context, storage.Path, storage.ReadOptions) (storage.Read, error) {
	d.openCalls++
	return storage.Read{}, errors.New("unexpected content read")
}
func (d *filesReadTestDriver) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	d.statCalls++
	if d.unavailable {
		return storage.Entry{}, errors.New("native storage is unavailable")
	}
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
	if d.unavailable {
		return storage.Page{}, errors.New("native storage is unavailable")
	}
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
	rows                        []nodes.Row
	revisions                   map[string]*string
	owner, prefix, namespace    string
	limit, calls                int
	queries                     []nodes.FileBrowseQuery
	metadataPaths               []string
	metadataHidden, usageHidden []string
	usageCalls                  int
	err, pageErr, usageErr      error
}

func filesReadHidden(p string, roots []string) bool {
	for _, root := range roots {
		if norm.NFC.String(p) == norm.NFC.String(root) || strings.HasPrefix(norm.NFC.String(p), norm.NFC.String(root)+"/") {
			return true
		}
	}
	return false
}
func (i *filesReadTestIndex) record(row nodes.Row, hidden []string) nodes.FileBrowseRecord {
	record := nodes.FileBrowseRecord{Row: row, ContentRevision: i.revisions[row.Path]}
	if row.IsDir {
		for _, child := range i.rows {
			if child.Path == row.Path || child.Parent != row.Path || filesReadHidden(child.Path, hidden) {
				continue
			}
			kind := "file"
			if child.IsDir {
				kind = "folder"
			}
			record.Membership = append(record.Membership, kind+":"+child.Path)
		}
	}
	return record
}
func (i *filesReadTestIndex) FileBrowseMetadata(ctx context.Context, owner, namespace, p string, hidden ...string) (*nodes.FileBrowseRecord, error) {
	i.calls++
	i.owner, i.namespace = owner, namespace
	i.metadataPaths = append(i.metadataPaths, p)
	i.metadataHidden = append([]string{}, hidden...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.err != nil {
		return nil, i.err
	}
	if filesReadHidden(p, hidden) {
		return nil, nil
	}
	for _, row := range i.rows {
		if row.Path == p {
			record := i.record(row, hidden)
			return &record, nil
		}
	}
	if p == "" {
		record := i.record(filesReadRow("", true), hidden)
		return &record, nil
	}
	return nil, nil
}
func filesReadIndexKey(row nodes.Row) nodes.FileBrowseKey {
	kind := 1
	if row.IsDir {
		kind = 0
	}
	return nodes.FileBrowseKey{Kind: kind, Name: norm.NFC.String(row.Name), Path: norm.NFC.String(row.Path)}
}
func (i *filesReadTestIndex) FileBrowsePage(ctx context.Context, owner, namespace string, q nodes.FileBrowseQuery) ([]nodes.FileBrowseRecord, error) {
	i.calls++
	i.owner, i.namespace, i.prefix, i.limit = owner, namespace, q.Path, q.Limit
	i.queries = append(i.queries, q)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.err != nil {
		return nil, i.err
	}
	if i.pageErr != nil {
		return nil, i.pageErr
	}
	rows := make([]nodes.Row, 0)
	for _, row := range i.rows {
		if filesReadHidden(row.Path, q.HiddenRoots) {
			continue
		}
		if q.List {
			if row.Path == q.Path || row.Parent != q.Path {
				continue
			}
		} else {
			if q.Path != "" && row.Path != q.Path && !strings.HasPrefix(row.Path, q.Path+"/") {
				continue
			}
			if q.Keyword != "" && !strings.Contains(strings.ToLower(norm.NFC.String("/"+row.Path)), strings.ToLower(norm.NFC.String(q.Keyword))) {
				continue
			}
			if q.Kind == "file" && row.IsDir || q.Kind == "folder" && !row.IsDir {
				continue
			}
			if q.MediaType != "" {
				if row.IsDir || row.Mime == nil {
					continue
				}
				filter := strings.ToLower(q.MediaType)
				media := strings.ToLower(*row.Mime)
				if strings.HasSuffix(filter, "*") {
					filter = strings.TrimSuffix(filter, "*")
				}
				if strings.HasSuffix(filter, "/") {
					if !strings.HasPrefix(media, filter) {
						continue
					}
				} else if media != filter {
					continue
				}
			}
		}
		if q.After != nil && !filesKeyLess(*q.After, filesReadIndexKey(row)) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(a, b int) bool { return filesKeyLess(filesReadIndexKey(rows[a]), filesReadIndexKey(rows[b])) })
	if len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	records := make([]nodes.FileBrowseRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, i.record(row, q.HiddenRoots))
	}
	return records, nil
}
func (i *filesReadTestIndex) FileBrowseUsage(ctx context.Context, owner, p string, hidden ...string) (int64, int64, error) {
	i.calls++
	i.usageCalls++
	i.owner, i.prefix = owner, p
	i.usageHidden = append([]string{}, hidden...)
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if i.err != nil {
		return 0, 0, i.err
	}
	if i.usageErr != nil {
		return 0, 0, i.usageErr
	}
	var bytes, files int64
	for _, row := range i.rows {
		if row.IsDir || filesReadHidden(row.Path, hidden) || p != "" && row.Path != p && !strings.HasPrefix(row.Path, p+"/") {
			continue
		}
		bytes += row.Size
		files++
	}
	return bytes, files, nil
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
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	mime := "text/plain"
	modified := "2026-10-30T08:09:10Z"
	row := nodes.Row{Node: nodes.Node{ID: "private-database-id", Path: p, Parent: parent, Name: path.Base(p), IsDir: folder, Size: 7, Mime: &mime, SourceModifiedAt: &modified, CreatedAt: "2025-01-01T01:02:03Z", UpdatedAt: "2026-01-01T01:02:03Z"}, Key: "secret-provider-key"}
	if folder {
		row.Size, row.Mime = 0, nil
	}
	if p == "" {
		row.Name = ""
	}
	return row
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

func filesReadAssertNoStorage(t *testing.T, d *filesReadTestDriver) {
	t.Helper()
	if d.statCalls != 0 || d.listCalls != 0 || d.openCalls != 0 {
		t.Fatalf("browsing accessed storage: Stat=%d List=%d Open=%d", d.statCalls, d.listCalls, d.openCalls)
	}
}

func filesReadMetadata(t *testing.T, s *Server, f *fileRequest, p string) fc.Metadata {
	t.Helper()
	w := filesReadCall(t, s, f, s.handleFilesMetadata, url.Values{"path": {p}}.Encode())
	var metadata fc.Metadata
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	return metadata
}

func TestFilesReadMetadataUsesIndexWhileStorageUnavailable(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	name := " Notes /Café +%2B#?.txt "
	row := filesReadRow(name, false)
	index.rows = []nodes.Row{row}
	saved := "cr_saved-by-publication"
	index.revisions = map[string]*string{name: &saved}
	got := filesReadMetadata(t, s, f, "/"+name)
	if got.Path != fc.Path("/"+name) || got.ParentPath != "/ Notes " || got.Name != "Café +%2B#?.txt " {
		t.Fatalf("indexed spelling lost: %+v", got)
	}
	if got.Size == nil || *got.Size != row.Size || got.MediaType == nil || *got.MediaType != *row.Mime || got.SourceModifiedAt == nil || got.SourceModifiedAt.Format(time.RFC3339) != *row.SourceModifiedAt {
		t.Fatalf("indexed fields lost: %+v", got)
	}
	if got.CreatedAt.Format(time.RFC3339) != row.CreatedAt || got.UpdatedAt.Format(time.RFC3339) != row.UpdatedAt || got.ContentRevision == nil || *got.ContentRevision != saved {
		t.Fatalf("indexed timestamps/revision lost: %+v", got)
	}
	if index.owner != "owner-a" || index.namespace != f.ContentNamespace || !reflect.DeepEqual(index.metadataPaths, []string{name}) {
		t.Fatalf("metadata query binding=%+v", index)
	}
	w := filesReadCall(t, s, f, s.handleFilesMetadata, url.Values{"path": {"/" + name}}.Encode())
	if got.ID != row.ID {
		t.Fatalf("metadata identity=%q want %q", got.ID, row.ID)
	}
	for _, secret := range []string{"secret-provider", `"ok"`, `"data"`, `"fileId"`, `"key"`, `"etag"`} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("metadata exposes %q", secret)
		}
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadIndexedVisibilityChangesOnlyWhenIndexChanges(t *testing.T) {
	s, f, d, index := filesReadFixture()
	filesReadAdd(d, "native-only.txt", storage.File, 99)
	index.rows = []nodes.Row{filesReadRow("indexed-only.txt", false)}
	page := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2F"))
	if len(page.Items) != 1 || page.Items[0].Path != "/indexed-only.txt" {
		t.Fatalf("indexed list=%+v", page)
	}
	filesReadMetadata(t, s, f, "/indexed-only.txt")
	err := s.handleFilesMetadata(httptest.NewRecorder(), filesReadRequest("path=%2Fnative-only.txt"), f)
	filesReadProblem(t, err, 404, "not_found")
	// A scan/import settles observed native changes into the index. Browsing itself never scans.
	index.rows = []nodes.Row{filesReadRow("native-only.txt", false)}
	page = filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "q=.txt"))
	if len(page.Items) != 1 || page.Items[0].Path != "/native-only.txt" {
		t.Fatalf("scanned list=%+v", page)
	}
	err = s.handleFilesMetadata(httptest.NewRecorder(), filesReadRequest("path=%2Findexed-only.txt"), f)
	filesReadProblem(t, err, 404, "not_found")
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadFolderRevisionsUseIndexedDirectMembership(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	index.rows = []nodes.Row{filesReadRow("Photos", true), filesReadRow("Photos/a.jpg", false)}
	a := filesReadMetadata(t, s, f, "/Photos")
	index.rows = append(index.rows, filesReadRow("Photos/b.jpg", false))
	b := filesReadMetadata(t, s, f, "/Photos")
	if a.Revision == b.Revision {
		t.Fatal("indexed direct membership did not change folder revision")
	}
	index.rows = append(index.rows, filesReadRow("Photos/nested", true))
	before := filesReadMetadata(t, s, f, "/Photos")
	index.rows = append(index.rows, filesReadRow("Photos/nested/inside.txt", false))
	after := filesReadMetadata(t, s, f, "/Photos")
	if before.Revision != after.Revision {
		t.Fatal("descendant membership became a recursive folder revision")
	}
	index.rows[1].Size++
	if changed := filesReadMetadata(t, s, f, "/Photos"); changed.Revision != after.Revision {
		t.Fatal("child content changed direct membership revision")
	}
	clone := *f
	clone.Namespace = "other-principal-binding"
	clone.Principal = &auth.Principal{User: f.Principal.User, Token: &auth.Token{ID: "other-app", CanRead: true}}
	if same := filesReadMetadata(t, s, &clone, "/Photos"); same.Revision != after.Revision {
		t.Fatal("folder revision depends on credential principal")
	}
	if after.Size != nil || after.MediaType != nil || after.Checksum != nil || after.ContentRevision != nil {
		t.Fatalf("folder has file fields: %+v", after)
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadListUsesBoundedIndexKeysetPagination(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	for _, p := range []string{"folder-Z", "folder-A"} {
		index.rows = append(index.rows, filesReadRow(p, true))
	}
	for _, p := range []string{"z.txt", "A.txt", "e\u0301.txt", "éz.txt"} {
		index.rows = append(index.rows, filesReadRow(p, false))
	}
	first := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2F&limit=2"))
	if len(first.Items) != 2 || first.Items[0].Path != "/folder-A" || first.Items[1].Path != "/folder-Z" || first.NextCursor == nil {
		t.Fatalf("first=%+v", first)
	}
	if len(index.queries) != 1 || index.queries[0].Path != "" || !index.queries[0].List || index.queries[0].Limit != 3 || index.queries[0].After != nil {
		t.Fatalf("first query=%+v", index.queries)
	}
	index.rows = append(index.rows, filesReadRow("folder-0", true), filesReadRow("B.txt", false))
	restarted := &Server{cfg: &config.Config{AppSecret: s.cfg.AppSecret}, filesIndex: index}
	query := url.Values{"path": {"/"}, "limit": {"1000"}, "cursor": {*first.NextCursor}}.Encode()
	second := filesReadCollection(t, filesReadCall(t, restarted, f, restarted.handleFilesList, query))
	want := []fc.Path{"/A.txt", "/B.txt", "/z.txt", "/e\u0301.txt", "/éz.txt"}
	if len(second.Items) != len(want) {
		t.Fatalf("second=%+v", second)
	}
	for n, p := range want {
		if second.Items[n].Path != p {
			t.Fatalf("item %d=%q want %q", n, second.Items[n].Path, p)
		}
	}
	if second.NextCursor != nil {
		t.Fatal("terminal page has a cursor")
	}
	last := index.queries[len(index.queries)-1]
	if last.Limit != 1001 || last.After == nil || *last.After != (nodes.FileBrowseKey{Kind: 0, Name: "folder-Z", Path: "folder-Z"}) {
		t.Fatalf("continuation query=%+v", last)
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadSearchScopesAndFiltersBeforeIndexPagination(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	f.Scope = "/Docs"
	f.Principal.Token = &auth.Token{ID: "reader", CanRead: true}
	for _, p := range []string{"Docs-old/outside.txt", "Docs/z.TXT", "Docs/A.txt", "Docs/secret.bin"} {
		index.rows = append(index.rows, filesReadRow(p, false))
	}
	index.rows = append(index.rows, filesReadRow("Docs", true))
	image := "image/png"
	index.rows[3].Mime = &image
	first := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "q=DoCs%2F&kind=file&mediaType=text%2F&limit=1"))
	if len(first.Items) != 1 || first.Items[0].Path != "/Docs/A.txt" || first.NextCursor == nil {
		t.Fatalf("first=%+v", first)
	}
	q := index.queries[0]
	if index.owner != "owner-a" || index.namespace != f.ContentNamespace || q.Path != "Docs" || q.List || q.Keyword != "DoCs/" || q.Kind != "file" || q.MediaType != "text/" || q.Limit != 2 {
		t.Fatalf("search query=%+v owner=%s namespace=%s", q, index.owner, index.namespace)
	}
	query := url.Values{"q": {"DoCs/"}, "kind": {"file"}, "mediaType": {"text/"}, "limit": {"1"}, "cursor": {*first.NextCursor}}.Encode()
	second := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, query))
	if len(second.Items) != 1 || second.Items[0].Path != "/Docs/z.TXT" || second.NextCursor != nil {
		t.Fatalf("second=%+v", second)
	}
	if q := index.queries[1]; q.After == nil || q.After.Path != "Docs/A.txt" || q.Limit != 2 {
		t.Fatalf("search keyset=%+v", q)
	}
	images := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, "mediaType=image%2F*&kind=file"))
	if len(images.Items) != 1 || images.Items[0].Path != "/Docs/secret.bin" {
		t.Fatalf("images=%+v", images)
	}
	before := index.calls
	err := s.handleFilesSearch(httptest.NewRecorder(), filesReadRequest("path=%2FDocs-old"), f)
	filesReadProblem(t, err, 404, "not_found")
	if index.calls != before {
		t.Fatal("out-of-scope search reached index")
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadMeAndCapabilitiesUseIndexWhileStorageUnavailable(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	f.Scope, f.CSRFToken = "/Docs", "csrf-cookie-bootstrap"
	index.rows = []nodes.Row{filesReadRow("Docs", true), filesReadRow("Docs/a", false), filesReadRow("Docs/missing-native", false), filesReadRow("Other/huge", false)}
	index.rows[1].Size, index.rows[2].Size, index.rows[3].Size = 7, 9, 9999
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
	if me.Usage.Bytes != 16 || me.Usage.Files != 2 || !me.Usage.Indexed || me.Quota != nil || me.Reservations != nil || me.CSRFToken != f.CSRFToken {
		t.Fatalf("me=%s", w.Body.String())
	}
	if index.usageCalls != 1 || index.owner != "owner-a" || index.prefix != "Docs" || len(index.queries) != 0 {
		t.Fatalf("usage did not use scoped aggregate: %+v", index)
	}
	w = filesReadCall(t, s, f, s.handleFilesCapabilities, "path=%2FDocs%2Fa")
	var capabilities map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &capabilities); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"write", "uploads", "thumbnails", "pinnedRead", "strongContentValidators"} {
		if capabilities[field] != false {
			t.Errorf("%s=%v", field, capabilities[field])
		}
	}
	if capabilities["range"] != true || capabilities["read"] != true || capabilities["path"] != "/Docs/a" {
		t.Fatalf("capabilities=%s", w.Body.String())
	}
	for _, forbidden := range []string{"provider", "secret", "owner-a", "COS", "bucket", "http:"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("capabilities expose %q", forbidden)
		}
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadUnavailableIndexAndMissingIndexedPaths(t *testing.T) {
	for _, endpoint := range []string{"metadata", "list", "search", "me", "capabilities"} {
		t.Run(endpoint, func(t *testing.T) {
			s, f, d, _ := filesReadFixture()
			d.unavailable = true
			handlers := map[string]fileHandler{"metadata": s.handleFilesMetadata, "list": s.handleFilesList, "search": s.handleFilesSearch, "me": s.handleFilesMe, "capabilities": s.handleFilesCapabilities}
			handler := handlers[endpoint]
			s.filesIndex = nil
			err := handler(httptest.NewRecorder(), filesReadRequest("path=%2F"), f)
			filesReadProblem(t, err, 503, "temporarily_unavailable")
			s.filesIndex = &filesReadTestIndex{}
			f.Scope = "/missing"
			err = handler(httptest.NewRecorder(), filesReadRequest("path=%2Fmissing"), f)
			filesReadProblem(t, err, 404, "not_found")
			filesReadAssertNoStorage(t, d)
		})
	}
}

func TestFilesReadSyntheticRootAndEmptyList(t *testing.T) {
	s, f, d, _ := filesReadFixture()
	d.unavailable = true
	root := filesReadMetadata(t, s, f, "/")
	if root.Path != "/" || root.ParentPath != "" || root.Name != "" || root.Kind != "folder" || root.ContentRevision != nil {
		t.Fatalf("synthetic root=%+v", root)
	}
	w := filesReadCall(t, s, f, s.handleFilesList, "path=%2F")
	if strings.TrimSpace(w.Body.String()) != `{"items":[],"nextCursor":null}` {
		t.Fatalf("empty list=%s", w.Body.String())
	}
	filesReadCall(t, s, f, s.handleFilesSearch, "")
	filesReadCall(t, s, f, s.handleFilesMe, "")
	filesReadCall(t, s, f, s.handleFilesCapabilities, "")
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadPendingDeleteRootsFilterEveryIndexedView(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	index.rows = []nodes.Row{filesReadRow("gone", true), filesReadRow("gone/a.txt", false), filesReadRow("gone-old.txt", false), filesReadRow("visible.txt", false)}
	index.rows[1].Size, index.rows[2].Size, index.rows[3].Size = 100, 3, 5
	before := filesReadMetadata(t, s, f, "/")
	f.Driver = &pendingDeleteDriver{Driver: d, roots: []string{"gone"}}
	after := filesReadMetadata(t, s, f, "/")
	if before.Revision == after.Revision {
		t.Fatal("hidden direct member remained in root revision")
	}
	for _, handler := range []fileHandler{s.handleFilesList, s.handleFilesSearch} {
		page := filesReadCollection(t, filesReadCall(t, s, f, handler, "path=%2F&kind=file&limit=1"))
		if len(page.Items) != 1 || page.Items[0].Path != "/gone-old.txt" || page.NextCursor == nil {
			t.Fatalf("pending deletion affected page boundary: %+v", page)
		}
		q := index.queries[len(index.queries)-1]
		if !reflect.DeepEqual(q.HiddenRoots, []string{"gone"}) || q.Limit != 2 {
			t.Fatalf("hidden roots were not pushed into query: %+v", q)
		}
	}
	for _, handler := range []fileHandler{s.handleFilesMetadata, s.handleFilesList, s.handleFilesSearch, s.handleFilesCapabilities} {
		err := handler(httptest.NewRecorder(), filesReadRequest("path=%2Fgone"), f)
		filesReadProblem(t, err, 404, "not_found")
	}
	w := filesReadCall(t, s, f, s.handleFilesMe, "")
	if !strings.Contains(w.Body.String(), `"bytes":8`) || !strings.Contains(w.Body.String(), `"files":2`) || !reflect.DeepEqual(index.usageHidden, []string{"gone"}) || !reflect.DeepEqual(index.metadataHidden, []string{"gone"}) {
		t.Fatalf("hidden usage=%s metadata=%v usage=%v", w.Body.String(), index.metadataHidden, index.usageHidden)
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadIndexErrorsNeverFallBackToStorage(t *testing.T) {
	for _, endpoint := range []string{"metadata", "page", "usage"} {
		t.Run(endpoint, func(t *testing.T) {
			s, f, d, index := filesReadFixture()
			d.unavailable = true
			failure := errors.New("database unavailable")
			handler := s.handleFilesMetadata
			query := "path=%2F"
			switch endpoint {
			case "metadata":
				index.err = failure
			case "page":
				index.pageErr = failure
				handler = s.handleFilesList
			case "usage":
				index.usageErr = failure
				handler = s.handleFilesMe
			}
			w := httptest.NewRecorder()
			if err := handler(w, filesReadRequest(query), f); !errors.Is(err, failure) {
				t.Fatalf("error=%v want database failure", err)
			}
			if w.Body.Len() != 0 {
				t.Fatalf("index failure produced partial response: %s", w.Body.String())
			}
			filesReadAssertNoStorage(t, d)
		})
	}
}

func TestFilesReadSearchFullPathTieBreaker(t *testing.T) {
	s, f, d, index := filesReadFixture()
	d.unavailable = true
	for _, p := range []string{"Z/same.txt", "A/same.txt", "é/same.txt"} {
		index.rows = append(index.rows, filesReadRow(p, false))
	}
	query := url.Values{"kind": {"file"}, "q": {"same.txt"}, "limit": {"1"}}
	want := []fc.Path{"/A/same.txt", "/Z/same.txt", "/é/same.txt"}
	for n, p := range want {
		page := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesSearch, query.Encode()))
		if len(page.Items) != 1 || page.Items[0].Path != p {
			t.Fatalf("page %d=%+v want %q", n, page, p)
		}
		if n == len(want)-1 {
			if page.NextCursor != nil {
				t.Fatal("terminal cursor")
			}
		} else {
			if page.NextCursor == nil {
				t.Fatal("missing continuation")
			}
			query.Set("cursor", *page.NextCursor)
		}
	}
	filesReadAssertNoStorage(t, d)
}

func TestFilesReadCursorRejectsTamperingAndDifferentBindings(t *testing.T) {
	s, f, _, index := filesReadFixture()
	index.rows = []nodes.Row{filesReadRow("a", false), filesReadRow("b", false)}
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
