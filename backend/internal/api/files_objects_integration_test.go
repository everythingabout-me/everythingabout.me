package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/treescan"
	"github.com/jackc/pgx/v5"
)

// This provider fixture can retain history independently of application accounting.
// Its mutation contract uses current ETags and current-key deletion, without requiring
// bucket versioning. It is not live provider capability or quiescence evidence.
type objectAPIVersion struct {
	ID        string
	Body      []byte
	ETag      string
	MediaType string
	Metadata  map[string]string
	Deleted   bool
}
type objectAPIProvider struct {
	mu               sync.Mutex
	versions         map[string][]objectAPIVersion
	puts             map[string]int
	deletes          map[string]int
	lostReplyKey     string
	invalidETagKey   string
	sequence         int
	unversioned      bool
	versionReads     map[string]int
	etagReads        map[string]int
	multipart        map[string]*objectAPIMultipart
	partRequests     int
	completeRequests map[string]int
	partEntered      chan int
	partRelease      chan struct{}
}

func newObjectAPIProvider() *objectAPIProvider {
	return &objectAPIProvider{versions: map[string][]objectAPIVersion{}, puts: map[string]int{}, deletes: map[string]int{}, versionReads: map[string]int{}, etagReads: map[string]int{}}
}
func (p *objectAPIProvider) current(key string) (objectAPIVersion, bool) {
	v := p.versions[key]
	if len(v) == 0 || v[len(v)-1].Deleted {
		return objectAPIVersion{}, false
	}
	return v[len(v)-1], true
}
func (p *objectAPIProvider) record(key string, body []byte, media string, meta map[string]string, deleted bool) objectAPIVersion {
	p.sequence++
	digest := sha256.Sum256(body)
	v := objectAPIVersion{ID: fmt.Sprintf("native-v-%d", p.sequence), Body: bytes.Clone(body), ETag: `"` + hex.EncodeToString(digest[:]) + `"`, MediaType: media, Metadata: meta, Deleted: deleted}
	if p.unversioned {
		v.ID = ""
		p.versions[key] = []objectAPIVersion{v}
	} else {
		p.versions[key] = append(p.versions[key], v)
	}
	return v
}
func (p *objectAPIProvider) serve(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") || r.Header.Get("X-Amz-Security-Token") != "object-api-session" {
		t.Error("native request did not use explicit official SDK authentication")
	}
	if r.URL.Path != "/bucket" && !strings.HasPrefix(r.URL.Path, "/bucket/") {
		t.Errorf("unexpected provider path %q", r.URL.Path)
		w.WriteHeader(400)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	p.mu.Lock()
	entered, release := p.partEntered, p.partRelease
	p.mu.Unlock()
	if entered != nil && r.Method == "PUT" && r.URL.Query().Get("uploadId") != "" {
		number, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		entered <- number
		<-release
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.serveMultipart(t, w, r, key) {
		return
	}
	if r.Method == "GET" && r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		type item struct {
			Key          string
			Size         int
			LastModified string
			ETag         string
		}
		type common struct{ Prefix string }
		page := struct {
			XMLName        xml.Name `xml:"ListBucketResult"`
			IsTruncated    bool
			Contents       []item
			CommonPrefixes []common
		}{IsTruncated: false}
		keys := make([]string, 0, len(p.versions))
		for k := range p.versions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		prefixes := map[string]bool{}
		for _, k := range keys {
			v, ok := p.current(k)
			if !ok || !strings.HasPrefix(k, prefix) {
				continue
			}
			rest := strings.TrimPrefix(k, prefix)
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				prefixes[prefix+rest[:i+1]] = true
				continue
			}
			page.Contents = append(page.Contents, item{k, len(v.Body), "2026-11-02T01:00:00Z", v.ETag})
		}
		for k := range prefixes {
			page.CommonPrefixes = append(page.CommonPrefixes, common{k})
		}
		sort.Slice(page.CommonPrefixes, func(i, j int) bool { return page.CommonPrefixes[i].Prefix < page.CommonPrefixes[j].Prefix })
		w.Header().Set("Content-Type", "application/xml")
		if err := xml.NewEncoder(w).Encode(page); err != nil {
			t.Error(err)
		}
		return
	}
	current, exists := p.current(key)
	switch r.Method {
	case "PUT":
		p.puts[key]++
		body, err := io.ReadAll(io.LimitReader(r.Body, (32<<20)+1))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		// COS admission does not promise PUT If-Match enforcement. Replacement
		// must reject a stale ETag before dispatching this sole native request.
		if r.Header.Get("If-None-Match") == "*" && exists {
			w.WriteHeader(412)
			io.WriteString(w, `<Error><Code>PreconditionFailed</Code></Error>`)
			return
		}
		if int64(len(body)) != r.ContentLength {
			t.Errorf("known-length native body changed: got %d header %d", len(body), r.ContentLength)
		}
		meta := map[string]string{}
		for name, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
				meta[strings.TrimPrefix(strings.ToLower(name), "x-amz-meta-")] = values[0]
			}
		}
		if meta["eam-intent"] == "" {
			t.Error("published native object omitted its private intent nonce")
		}
		result := p.record(key, body, r.Header.Get("Content-Type"), meta, false)
		if key == p.lostReplyKey {
			conn, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("ETag", result.ETag)
		w.Header().Set("X-Amz-Version-Id", result.ID)
		if key == p.invalidETagKey {
			w.Header().Del("ETag")
		}
		w.WriteHeader(200)
	case "DELETE":
		p.deletes[key]++
		if r.Header.Get("If-Match") == "" {
			t.Error("current-key deletion omitted its captured ETag condition")
			w.WriteHeader(400)
			return
		}
		if r.URL.Query().Has("versionId") {
			t.Error("current-key deletion attempted to delete a retained version")
			w.WriteHeader(400)
			return
		}
		if match := r.Header.Get("If-Match"); match != "" && (!exists || match != current.ETag) {
			w.WriteHeader(412)
			return
		}
		marker := p.record(key, nil, "", nil, true)
		w.Header().Set("X-Amz-Version-Id", marker.ID)
		w.Header().Set("X-Amz-Delete-Marker", "true")
		w.WriteHeader(204)
	case "HEAD", "GET":
		selected := current
		found := exists
		if id := r.URL.Query().Get("versionId"); id != "" {
			if r.Method == "GET" {
				p.versionReads[key+"?"+id]++
			}
			found = false
			for _, v := range p.versions[key] {
				if v.ID == id && !v.Deleted {
					selected = v
					found = true
					break
				}
			}
		}
		if !found {
			w.WriteHeader(404)
			return
		}
		if match := r.Header.Get("If-Match"); match != "" {
			if r.Method == "GET" {
				p.etagReads[key+"?"+match]++
			}
			if match != selected.ETag {
				w.WriteHeader(412)
				return
			}
		}
		body := selected.Body
		status := 200
		w.Header().Set("ETag", selected.ETag)
		w.Header().Set("X-Amz-Version-Id", selected.ID)
		w.Header().Set("Content-Type", selected.MediaType)
		w.Header().Set("Last-Modified", "Mon, 02 Nov 2026 01:00:00 GMT")
		for name, value := range selected.Metadata {
			w.Header().Set("X-Amz-Meta-"+name, value)
		}
		if value := r.Header.Get("Range"); value != "" && r.Method == "GET" {
			var a, b int
			if _, err := fmt.Sscanf(value, "bytes=%d-%d", &a, &b); err != nil || a < 0 || b < a || b >= len(body) {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
				w.WriteHeader(416)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(body)))
			body = body[a : b+1]
			status = 206
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if r.Method == "GET" {
			_, _ = w.Write(body)
		}
	default:
		t.Errorf("unexpected native method %s", r.Method)
		w.WriteHeader(405)
	}
}

type objectsAPIFixture struct {
	ctx      context.Context
	db       *store.DB
	cfg      *config.Config
	backend  *storagebackend.Backend
	server   *Server
	provider *objectAPIProvider
	owners   [2]string
	tokens   [2]string
	cookie   string
	roots    [2]fileops.Root
}

func newObjectsAPIFixture(t *testing.T) *objectsAPIFixture {
	t.Helper()
	address := os.Getenv("EAM_TEST_DATABASE_URL")
	if address == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; real SDK/object mutation/P4 API conformance requires disposable Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	name := "eam_objects_api_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, e := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); e != nil {
			t.Error(e)
		}
	})
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	db, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var actual string
	if err = db.Pool().QueryRow(ctx, "select current_database()").Scan(&actual); err != nil || actual != name {
		t.Fatalf("database isolation failed: %s %v", actual, err)
	}
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 22 {
		t.Fatalf("full schema unavailable: %d %v", len(migrations), err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, path := range migrations {
			sql, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.Pool().Exec(ctx, string(sql)); e != nil {
				t.Fatalf("migration %s pass%d: %v", path, pass, e)
			}
		}
	}
	provider := newObjectAPIProvider()
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { provider.serve(t, w, r) }))
	t.Cleanup(native.Close)
	proof := objectmutation.Admission{ConnectionID: "default", Endpoint: native.URL, Bucket: "bucket", Region: "test-region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated stateful S3 SDK fixture, not live cloud evidence", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true, MultipartUploads: true}
	proofPath := filepath.Join(t.TempDir(), "admission.json")
	encoded, _ := json.Marshal(proof)
	if err = os.WriteFile(proofPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AppSecret: "object-api-independent-secret", SessionTTLDays: 1, FileCookieOrigins: []string{"https://files.example"}, Storage: config.Storage{Backend: "s3", ConnectionID: "default", Generation: 1, CoordinatedWrites: true, MaxSinglePutBytes: 32 << 20, ObjectAdmissionFile: proofPath, ObjectSpoolRoot: t.TempDir(), S3: config.S3{Endpoint: native.URL, Region: "test-region", Bucket: "bucket", AccessKeyID: "object-api-id", SecretAccessKey: "object-api-secret", SessionToken: "object-api-session", PathStyle: true, ContentPrefix: "drive/", SystemPrefix: "system/"}}}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	f := &objectsAPIFixture{ctx: ctx, db: db, cfg: cfg, backend: backend, provider: provider}
	journal := fileops.NewPostgres(db.Pool())
	authentication := auth.New(db, cfg)
	for i := range f.owners {
		if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("objects-%d@example.test", i)).Scan(&f.owners[i]); err != nil {
			t.Fatal(err)
		}
		driver, e := backend.Open(ctx, f.owners[i])
		if e != nil {
			t.Fatal(e)
		}
		native, ok := driver.(fileops.NativeActions)
		if !ok {
			driver.Close()
			t.Fatal("configured S3 SDK tree omitted object mutation driver")
		}
		binding := native.StorageBinding()
		f.roots[i] = fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive, RetainsVersions: native.Describe().RetainsVersions}
		driver.Close()
		if e = journal.Initialize(ctx, f.roots[i], fileops.Baseline{LogicalLimit: 1 << 20, PhysicalLimit: 4 << 20, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"fixture":"fresh isolated owner prefixes, zero current and retained bytes"}`)}); e != nil {
			t.Fatal(e)
		}
		f.tokens[i], _, e = authentication.CreateToken(ctx, auth.NewTokenInput{UserID: f.owners[i], Name: "objects API", Kind: auth.KindApp, CanRead: true, CanWrite: true})
		if e != nil {
			t.Fatal(e)
		}
	}
	f.cookie, _, err = authentication.CreateSession(ctx, f.owners[0], "object fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	f.server = New(cfg, db, backend.Legacy(), nil)
	f.server.SetStorageFactory(backend)
	t.Cleanup(func() {
		workers := f.server.workers()
		deadline := time.Now().Add(5 * time.Second)
		for {
			workers.mu.Lock()
			active := len(workers.active)
			workers.mu.Unlock()
			if active == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Error("object operation worker remained active")
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	return f
}
func (f *objectsAPIFixture) request(t *testing.T, method, resource, path string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/files" + resource
	if path != "" {
		target += "?" + url.Values{"path": {path}}.Encode()
	}
	r := httptest.NewRequest(method, "https://files.example"+target, bytes.NewReader(body)).WithContext(f.ctx)
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	if r.Header.Get("Authorization") == "" {
		r.Header.Set("Authorization", "Bearer "+f.tokens[0])
	}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	return w
}
func objectsAPIJSON(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP%d want%d headers=%v body=%s", w.Code, status, w.Header(), w.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["ok"]; ok {
		t.Fatal("legacy envelope returned")
	}
	return value
}
func (f *objectsAPIFixture) metadata(t *testing.T, path string) map[string]any {
	return objectsAPIJSON(t, f.request(t, "GET", "", path, nil, nil), 200)
}
func (f *objectsAPIFixture) usage(t *testing.T, owner int, logical, physical int64) {
	t.Helper()
	u, err := fileops.NewPostgres(f.db.Pool()).Usage(f.ctx, f.roots[owner], fileops.Actor{ID: "owner:" + f.owners[owner], CanRead: true, CanWrite: true, ManageOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	if u.LogicalBytes != logical || u.PhysicalBytes != physical || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
		t.Fatalf("usage=%+v want logical%d physical%d with no retained reservation", u, logical, physical)
	}
}

func TestFilesObjectsPostgresS3MutationConformance(t *testing.T) {
	f := newObjectsAPIFixture(t)
	const special = "/ a+%2F?#中文.bin "
	t.Run("etag_replace_credits_current_bytes_despite_optional_history", func(t *testing.T) {
		zero := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/zero", nil, nil), 201)
		if zero["size"] != float64(0) {
			t.Fatal(zero)
		}
		created := objectsAPIJSON(t, f.request(t, "PUT", "/content", special, []byte{0, 255, 13, 10}, nil), 201)
		if created["path"] != special || created["contentRevision"] == nil {
			t.Fatal("exact object representation lost", created)
		}
		observed := f.metadata(t, special)
		createdJSON, _ := json.Marshal(created)
		observedJSON, _ := json.Marshal(observed)
		if !bytes.Equal(createdJSON, observedJSON) {
			t.Fatalf("durable PUT resource differs from native metadata GET: put=%s get=%s", createdJSON, observedJSON)
		}
		f.usage(t, 0, 4, 4)
		nativeKey := "drive/" + f.owners[0] + special
		f.provider.mu.Lock()
		old := f.provider.versions[nativeKey][0]
		f.provider.mu.Unlock()
		if created["contentRevision"] == old.ID || created["contentRevision"] == old.ETag {
			t.Fatal("provider validator exposed as application revision")
		}
		got := f.request(t, "GET", "/content", special, nil, nil)
		if got.Code != 200 || !bytes.Equal(got.Body.Bytes(), []byte{0, 255, 13, 10}) {
			t.Fatalf("native bytes HTTP%d %q", got.Code, got.Body.Bytes())
		}
		r := httptest.NewRequest("GET", "https://files.example/api/files/content?"+url.Values{"path": {special}}.Encode(), nil).WithContext(f.ctx)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: f.cookie})
		cookieRead := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(cookieRead, r)
		if cookieRead.Code != 200 || !bytes.Equal(cookieRead.Body.Bytes(), got.Body.Bytes()) || cookieRead.Header().Get("ETag") != got.Header().Get("ETag") {
			t.Fatal("cookie and Bearer object reads diverged")
		}
		tag := got.Header().Get("ETag")
		driver, err := f.backend.Open(f.ctx, f.owners[0])
		if err != nil {
			t.Fatal(err)
		}
		entry, err := driver.Stat(f.ctx, storage.Path(strings.TrimPrefix(special, "/")))
		description := driver.Describe()
		driver.Close()
		if err != nil || !bytes.Equal(entry.Revision, []byte("native-etag:"+old.ETag)) || description.RetainsVersions || description.PinnedRead {
			t.Fatalf("object revision must use the current ETag without retained-version guarantees: entry=%+v description=%+v error=%v", entry, description, err)
		}
		if tag == "" || tag != strconv.Quote(created["contentRevision"].(string)) {
			t.Fatalf("actual strong content validator differs from metadata: %q %v", tag, created)
		}
		updated := objectsAPIJSON(t, f.request(t, "PUT", "/content", special, []byte("NEWER!"), http.Header{"If-Match": {tag}}), 200)
		if updated["contentRevision"] == created["contentRevision"] {
			t.Fatal("replacement retained old generation")
		}
		f.usage(t, 0, 6, 6)
		f.provider.mu.Lock()
		history := append([]objectAPIVersion(nil), f.provider.versions[nativeKey]...)
		f.provider.mu.Unlock()
		if len(history) != 2 || !bytes.Equal(history[0].Body, old.Body) {
			t.Fatal("replacement destroyed retained version")
		}
		stale := f.request(t, "PUT", "/content", special, []byte("stale"), http.Header{"If-Match": {tag}})
		objectsAPIJSON(t, stale, 412)
		f.provider.mu.Lock()
		puts := f.provider.puts[nativeKey]
		f.provider.mu.Unlock()
		if puts != 2 {
			t.Fatalf("stale ETag reached a provider that does not enforce PUT If-Match: %d native PUTs", puts)
		}
		f.usage(t, 0, 6, 6)
	})
	t.Run("native_marker_copy_move_and_current_key_delete", func(t *testing.T) {
		objectsAPIJSON(t, f.request(t, "POST", "", "", []byte(`{"kind":"folder","path":"/Folder/Empty","parents":true}`), nil), 201)
		objectsAPIJSON(t, f.request(t, "PUT", "/content", "/Folder/file", []byte("abcdef"), nil), 201)
		f.usage(t, 0, 12, 12)
		source := f.metadata(t, "/Folder")
		objectsAPIJSON(t, f.request(t, "POST", "/copy", "", []byte(fmt.Sprintf(`{"from":"/Folder","to":"/Copy","ifSourceRevision":%q}`, source["revision"])), nil), 201)
		f.usage(t, 0, 18, 18)
		f.provider.mu.Lock()
		sourceKey := "drive/" + f.owners[0] + "/Folder/file"
		nativeSource, _ := f.provider.current(sourceKey)
		conditionalReads := f.provider.etagReads[sourceKey+"?"+nativeSource.ETag]
		f.provider.mu.Unlock()
		if conditionalReads == 0 {
			t.Fatal("copy did not read its source with native If-Match")
		}
		before := f.metadata(t, "/Copy/file")
		original := f.metadata(t, "/Folder/file")
		if before["contentRevision"] == original["contentRevision"] {
			t.Fatal("copy aliased source content identity")
		}
		folder := f.metadata(t, "/Copy")
		objectsAPIJSON(t, f.request(t, "POST", "/move", "", []byte(fmt.Sprintf(`{"from":"/Copy","to":"/Moved","ifRevision":%q}`, folder["revision"])), nil), 200)
		f.usage(t, 0, 18, 18)
		after := f.metadata(t, "/Moved/file")
		if after["contentRevision"] != before["contentRevision"] {
			t.Fatal("copy/delete move lost content generation")
		}
		if empty := f.metadata(t, "/Moved/Empty"); empty["kind"] != "folder" {
			t.Fatal(empty)
		}
		deleted := f.request(t, "DELETE", "", "/Moved/file", nil, nil)
		if deleted.Code != 204 {
			t.Fatalf("delete HTTP%d %s", deleted.Code, deleted.Body.String())
		}
		f.usage(t, 0, 12, 12)
		missing := f.request(t, "GET", "/content", "/Moved/file", nil, nil)
		objectsAPIJSON(t, missing, 404)
		key := "drive/" + f.owners[0] + "/Moved/file"
		f.provider.mu.Lock()
		v := append([]objectAPIVersion(nil), f.provider.versions[key]...)
		f.provider.mu.Unlock()
		if len(v) != 2 || v[0].Deleted || !v[1].Deleted || string(v[0].Body) != "abcdef" {
			t.Fatal("current deletion resurfaced or erased old bytes", v)
		}
		again := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/Moved/file", []byte("again"), nil), 201)
		if again["contentRevision"] == after["contentRevision"] {
			t.Fatal("deleted/current-key recreation reused an old content generation")
		}
		objectsAPIJSON(t, f.request(t, "PUT", "/content", "/Moved/file", []byte("stale"), http.Header{"If-Match": {strconv.Quote(after["contentRevision"].(string))}}), 412)
		f.usage(t, 0, 17, 17)
		body := f.request(t, "GET", "/content", "/Moved/file", nil, nil)
		if body.Code != 200 || body.Body.String() != "again" {
			t.Fatalf("recreated current key wrong %d %s", body.Code, body.Body.String())
		}
		objectsAPIJSON(t, f.request(t, "PUT", "/content", "/private", []byte("other"), http.Header{"Authorization": {"Bearer " + f.tokens[1]}}), 201)
		f.usage(t, 1, 5, 5)
		objectsAPIJSON(t, f.request(t, "GET", "/content", "/private", nil, nil), 404)
	})
	t.Run("imported_implicit_prefix_accepts_children_without_a_marker", func(t *testing.T) {
		key := "drive/" + f.owners[0] + "/Implicit/file"
		f.provider.mu.Lock()
		f.provider.record(key, nil, "text/plain", map[string]string{}, false)
		f.provider.mu.Unlock()
		// Explicitly import the external zero-byte child and its implicit parent.
		// This leaves the established byte baseline unchanged.
		scan, err := treescan.Inventory(f.ctx, strings.NewReader("key,size,lastmodifieddate\n"+key+",0,2020-01-02T03:04:05Z\n"), "drive/"+f.owners[0]+"/", treescan.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = nodes.ApplyScan(f.ctx, f.db.Pool(), f.owners[0], scan, nodes.ScanApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		driver, err := f.backend.Open(f.ctx, f.owners[0])
		if err != nil {
			t.Fatal(err)
		}
		parent, err := driver.Stat(f.ctx, "Implicit")
		driver.Close()
		if err != nil || parent.Kind != storage.Folder || len(parent.Revision) != 0 {
			t.Fatalf("implicit parent must exist without a native validator: %+v %v", parent, err)
		}
		created := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/Implicit/new", []byte("published"), nil), 201)
		if created["path"] != "/Implicit/new" || created["parentId"] == nil {
			t.Fatal("implicit parent identity was not preserved", created)
		}
		body := f.request(t, "GET", "/content", "/Implicit/new", nil, nil)
		if body.Code != 200 || body.Body.String() != "published" {
			t.Fatalf("implicit child contents HTTP%d %q", body.Code, body.Body.String())
		}
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		_, marker := f.provider.current("drive/" + f.owners[0] + "/Implicit/")
		if marker || f.provider.puts["drive/"+f.owners[0]+"/Implicit/"] != 0 || f.provider.puts["drive/"+f.owners[0]+"/Implicit/new"] != 1 {
			t.Fatal("implicit parent acquired a synthetic native marker or child publication repeated")
		}
	})
}

type objectAPIGatedBody struct {
	reader  *bytes.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *objectAPIGatedBody) Read(data []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(data)
}

func TestFilesObjectsParentMarkerETagChangeInvalidatesIngress(t *testing.T) {
	f := newObjectsAPIFixture(t)
	objectsAPIJSON(t, f.request(t, "POST", "", "", []byte(`{"kind":"folder","path":"/Parent"}`), nil), 201)
	body := &objectAPIGatedBody{reader: bytes.NewReader([]byte("body")), entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("PUT", "https://files.example/api/files/content?path=%2FParent%2Ffile", body).WithContext(f.ctx)
	r.ContentLength = 4
	r.Header.Set("Authorization", "Bearer "+f.tokens[0])
	done := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		done <- w
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(body.release) }) }
	t.Cleanup(func() {
		release()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("staged fixture request remained active")
		}
	})
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter staged ingress")
	}
	parentKey := "drive/" + f.owners[0] + "/Parent/"
	f.provider.mu.Lock()
	old, _ := f.provider.current(parentKey)
	current := old
	// A new VersionID alone is not a current-ETag change. Keep the optional
	// version ID fixed and change only the validator captured during admission.
	current.ETag = `"external-marker-etag"`
	f.provider.versions[parentKey] = append(f.provider.versions[parentKey], current)
	f.provider.mu.Unlock()
	if old.ETag == current.ETag || old.ID != current.ID {
		t.Fatal("fixture did not isolate a parent ETag change")
	}
	release()
	var response *httptest.ResponseRecorder
	select {
	case response = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not resolve changed parent")
	}
	if response.Code != 412 {
		t.Fatalf("changed native parent should fail its captured condition: HTTP%d body=%s", response.Code, response.Body.String())
	}
	f.provider.mu.Lock()
	published := f.provider.puts["drive/"+f.owners[0]+"/Parent/file"]
	f.provider.mu.Unlock()
	if published != 0 {
		t.Fatal("staged request published into a recreated parent")
	}
	f.usage(t, 0, 0, 0)
}

func TestFilesObjectsUnversionedETagReplacement(t *testing.T) {
	f := newObjectsAPIFixture(t)
	f.provider.mu.Lock()
	f.provider.unversioned = true
	f.provider.mu.Unlock()
	created := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/plain", []byte("old"), nil), 201)
	original := f.request(t, "GET", "/content", "/plain", nil, nil)
	if original.Code != 200 || original.Body.String() != "old" || original.Header().Get("ETag") == "" {
		t.Fatalf("unversioned ETag read HTTP%d %q headers=%v", original.Code, original.Body.String(), original.Header())
	}
	updated := objectsAPIJSON(t, f.request(t, "PUT", "/content", "/plain", []byte("newer"), http.Header{"If-Match": {original.Header().Get("ETag")}}), 200)
	if updated["id"] != created["id"] || updated["contentRevision"] == created["contentRevision"] {
		t.Fatal("unversioned replacement lost identity or retained the old revision", created, updated)
	}
	objectsAPIJSON(t, f.request(t, "PUT", "/content", "/plain", []byte("stale"), http.Header{"If-Match": {original.Header().Get("ETag")}}), 412)
	current := f.request(t, "GET", "/content", "/plain", nil, nil)
	if current.Code != 200 || current.Body.String() != "newer" || current.Header().Get("ETag") == original.Header().Get("ETag") {
		t.Fatalf("replacement bytes/validator HTTP%d %q headers=%v", current.Code, current.Body.String(), current.Header())
	}
	f.usage(t, 0, 5, 5)
	key := "drive/" + f.owners[0] + "/plain"
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	object, exists := f.provider.current(key)
	if !exists || object.ID != "" || len(f.provider.versions[key]) != 1 || f.provider.puts[key] != 2 || len(f.provider.versionReads) != 0 {
		t.Fatal("unversioned fixture required history or dispatched a stale replacement")
	}
}

func TestFilesObjectsMissingPublicationETagRemainsUnknown(t *testing.T) {
	f := newObjectsAPIFixture(t)
	key := "drive/" + f.owners[0] + "/missing-etag"
	f.provider.mu.Lock()
	f.provider.invalidETagKey = key
	f.provider.mu.Unlock()
	response := f.request(t, "PUT", "/content", "/missing-etag", []byte("native"), nil)
	failure := objectsAPIJSON(t, response, 503)
	id, ok := failure["operationId"].(string)
	if !ok || id == "" {
		t.Fatal("missing durable identity after untrustworthy native response", failure)
	}
	state := objectsAPIJSON(t, f.request(t, "GET", "/operations/"+id, "", nil, nil), 200)
	if state["state"] != "outcome_unknown" {
		t.Fatal("missing ETag acknowledgement became definite completion", state)
	}
	f.provider.mu.Lock()
	current, exists := f.provider.current(key)
	calls := f.provider.puts[key]
	f.provider.mu.Unlock()
	if !exists || string(current.Body) != "native" || calls != 1 {
		t.Fatal("ambiguous native response was replayed or hidden")
	}
	journal := fileops.NewPostgres(f.db.Pool())
	u, err := journal.Usage(f.ctx, f.roots[0], fileops.Actor{ID: "owner:" + f.owners[0], CanRead: true, CanWrite: true, ManageOwner: true})
	if err != nil || u.LogicalBytes != 0 || u.LogicalReserved != 6 || u.PhysicalReserved != 12 {
		t.Fatalf("unresolved liabilities incorrectly released: %+v %v", u, err)
	}
}

func TestFilesObjectsLostPublicationReplyRemainsGuarded(t *testing.T) {
	f := newObjectsAPIFixture(t)
	key := "drive/" + f.owners[0] + "/uncertain"
	f.provider.mu.Lock()
	f.provider.lostReplyKey = key
	f.provider.mu.Unlock()
	result := f.request(t, "PUT", "/content", "/uncertain", []byte("applied"), http.Header{"Idempotency-Key": {"lost-final-response"}})
	failure := objectsAPIJSON(t, result, 503)
	id, ok := failure["operationId"].(string)
	if !ok || id == "" || result.Header().Get("Location") != "/api/files/operations/"+id {
		t.Fatalf("unknown mutation lacks durable identity: %v %v", failure, result.Header())
	}
	f.provider.mu.Lock()
	visible, exists := f.provider.current(key)
	attempts := f.provider.puts[key]
	f.provider.mu.Unlock()
	if !exists || string(visible.Body) != "applied" || attempts != 1 {
		t.Fatalf("lost reply fixture did not apply exactly once: %t %+v attempts%d", exists, visible, attempts)
	}
	// Rebuild both startup-owned factory and API service against the same durable
	// spool and database; a current HEAD is never proof that no delayed request remains.
	restarted, err := storagebackend.New(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	server := New(f.cfg, f.db, restarted.Legacy(), nil)
	server.SetStorageFactory(restarted)
	f.server = server
	visibleHead := f.request(t, "HEAD", "/content", "/uncertain", nil, nil)
	visibleGet := f.request(t, "GET", "/content", "/uncertain", nil, nil)
	if visibleHead.Code != 200 || visibleGet.Code != 200 || visibleGet.Body.String() != "applied" {
		t.Fatal("fixture native effect was not visible through the actual read API")
	}
	for i := 0; i < 3; i++ {
		state := objectsAPIJSON(t, f.request(t, "GET", "/operations/"+id, "", nil, nil), 200)
		if state["state"] != "outcome_unknown" {
			t.Fatal("restart/poll falsely resolved native uncertainty", state)
		}
	}
	cancelled := objectsAPIJSON(t, f.request(t, "POST", "/operations/"+id+"/cancel", "", nil, nil), 202)
	if cancelled["state"] != "outcome_unknown" || cancelled["cancelRequested"] != true {
		t.Fatal("cancel falsely aborted native publication", cancelled)
	}
	objectsAPIJSON(t, f.request(t, "PUT", "/content", "/newer", []byte("blocked"), nil), 409)
	f.provider.mu.Lock()
	attempts = f.provider.puts[key]
	newer := f.provider.puts["drive/"+f.owners[0]+"/newer"]
	f.provider.mu.Unlock()
	if attempts != 1 || newer != 0 {
		t.Fatalf("uncertain native publication replayed or guard bypassed old%d newer%d", attempts, newer)
	}
	op, err := fileops.NewPostgres(f.db.Pool()).Get(f.ctx, f.roots[0], fileops.Actor{ID: "owner:" + f.owners[0], CanRead: true, CanWrite: true, ManageOwner: true}, id)
	if err != nil || op.State != fileops.Unknown || len(op.Checkpoint) == 0 {
		t.Fatalf("recovery provenance lost %+v %v", op, err)
	}
}

// A deletion settles what the index holds even when the bytes are already gone — removed by
// another tool, or a folder that only ever existed as its children's prefix. Refusing with
// 404 left a row nothing could remove: the listing showed an entry, and every attempt to
// delete it failed on the native observation. What must not change: a path with no rows is
// still 404, and a non-recursive deletion still never removes a subtree.
func TestFilesObjectsDeleteSettlesAnEntryTheTreeAlreadyLost(t *testing.T) {
	f := newObjectsAPIFixture(t)
	if _, err := f.db.Pool().Exec(f.ctx, `update file_budgets set logical_limit=$1,physical_limit=$2`, int64(1<<30), int64(2<<30)); err != nil {
		t.Fatal(err)
	}
	key := func(path string) string { return "drive/" + f.owners[0] + "/" + path }
	rows := func(path string) int {
		t.Helper()
		var count int
		if err := f.db.Pool().QueryRow(f.ctx, `select count(*) from file_entries where owner_id=$1::uuid and path=$2`, f.owners[0], path).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	// drop removes the bytes exactly as an outside tool would: the object (and a folder's
	// marker) is gone, while the index still holds the entry.
	drop := func(path string) {
		t.Helper()
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		for _, k := range []string{key(path), key(path) + "/"} {
			if _, exists := f.provider.current(k); exists {
				f.provider.record(k, nil, "", nil, true)
			}
		}
	}
	settle := func(id string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			status := objectsAPIJSON(t, f.request(t, "GET", "/uploads/"+id, "", nil, nil), 200)
			if status["state"] == "committed" {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("upload did not settle: %v", status)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	upload := func(path string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"path": path, "size": 4, "parents": true})
		created := objectsAPIJSON(t, f.request(t, "POST", "/uploads", "", body, http.Header{"Idempotency-Key": {"ghost-" + path}, "Content-Type": {"application/json"}}), 201)
		id := created["id"].(string)
		objectsAPIJSON(t, f.request(t, "PUT", "/uploads/"+id+"/parts/1", "", []byte("data"), nil), 201)
		response := f.request(t, "POST", "/uploads/"+id+"/commit", "", nil, nil)
		if response.Code != 200 && response.Code != 202 {
			t.Fatalf("commit %q HTTP%d body=%s", path, response.Code, response.Body.String())
		}
		settle(id)
	}
	remove := func(path string, recursive bool) *httptest.ResponseRecorder {
		target := "/api/files?" + url.Values{"path": {path}, "recursive": {strconv.FormatBool(recursive)}}.Encode()
		r := httptest.NewRequest("DELETE", "https://files.example"+target, nil).WithContext(f.ctx)
		r.Header.Set("Authorization", "Bearer "+f.tokens[0])
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		return w
	}

	// A file whose object vanished still deletes: the row is the only thing left.
	upload("/Gone.txt")
	drop("Gone.txt")
	expectStatus(t, remove("/Gone.txt", false), 204)
	if count := rows("Gone.txt"); count != 0 {
		t.Fatalf("row for a lost object survived the delete: %d", count)
	}
	// A folder whose marker vanished does too.
	objectsAPIJSON(t, f.request(t, "POST", "", "", []byte(`{"kind":"folder","path":"/Empty"}`), http.Header{"Content-Type": {"application/json"}}), 201)
	drop("Empty")
	expectStatus(t, remove("/Empty", false), 204)
	if count := rows("Empty"); count != 0 {
		t.Fatalf("folder row survived the delete: %d", count)
	}
	// Nothing is invented: a path the index never had is still not found.
	uploadsHTTPProblem(t, remove("/Never-Existed.txt", false), 404, "not_found")
	// A folder whose bytes are all gone is settled with the rows it still holds: the tree
	// is what decides "empty" here, exactly as it does for a folder that exists natively
	// with a stale child. The non-recursive delete still refuses while the bytes are there.
	upload("/Ghosted/a.txt")
	uploadsHTTPProblem(t, remove("/Ghosted", false), 409, "directory_not_empty")
	drop("Ghosted/a.txt")
	drop("Ghosted")
	expectStatus(t, remove("/Ghosted", false), 204)
	if count := rows("Ghosted"); count != 0 {
		t.Fatalf("subtree survived the delete: %d", count)
	}
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP%d want%d headers=%v body=%s", w.Code, status, w.Header(), w.Body.String())
	}
}
