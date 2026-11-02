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
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// This provider fixture models retained object generations and current-key delete
// markers. It is intentionally not a live provider capability or quiescence proof.
type objectAPIVersion struct {
	ID        string
	Body      []byte
	ETag      string
	MediaType string
	Metadata  map[string]string
	Deleted   bool
}
type objectAPIProvider struct {
	mu                sync.Mutex
	versions          map[string][]objectAPIVersion
	puts              map[string]int
	deletes           map[string]int
	lostReplyKey      string
	invalidVersionKey string
	sequence          int
	versionReads      map[string]int
}

func newObjectAPIProvider() *objectAPIProvider {
	return &objectAPIProvider{versions: map[string][]objectAPIVersion{}, puts: map[string]int{}, deletes: map[string]int{}, versionReads: map[string]int{}}
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
	p.versions[key] = append(p.versions[key], v)
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
	defer p.mu.Unlock()
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
		if r.Header.Get("If-None-Match") == "*" && exists || r.Header.Get("If-Match") != "" && (!exists || r.Header.Get("If-Match") != current.ETag) {
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
		if key == p.invalidVersionKey {
			w.Header().Set("X-Amz-Version-Id", "null")
		}
		w.WriteHeader(200)
	case "DELETE":
		p.deletes[key]++
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
	proof := objectmutation.Admission{ConnectionID: "default", Endpoint: native.URL, Bucket: "bucket", Region: "test-region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated stateful S3 SDK fixture, not live cloud evidence", AtomicPublish: true, VersionedReads: true, ConditionalCreate: true, CurrentKeyDelete: true, CoordinatedExclusiveWriter: true, SingleGateway: true, LegacyWritersDrained: true}
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
		f.roots[i] = fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive, RetainsVersions: true}
		driver.Close()
		if e = journal.Initialize(ctx, f.roots[i], fileops.Baseline{LogicalLimit: 1 << 20, PhysicalLimit: 4 << 20, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"fixture":"fresh isolated versioned owner prefixes, zero current and retained bytes"}`)}); e != nil {
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
	t.Run("versioned_put_replace_and_physical_history", func(t *testing.T) {
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
		f.provider.mu.Lock()
		versionReads := f.provider.versionReads[nativeKey+"?"+old.ID]
		f.provider.mu.Unlock()
		if versionReads == 0 {
			t.Fatal("versioned API bytes did not select the native retained version ID")
		}
		if tag == "" || tag != strconv.Quote(created["contentRevision"].(string)) {
			t.Fatalf("actual strong content validator differs from metadata: %q %v", tag, created)
		}
		updated := objectsAPIJSON(t, f.request(t, "PUT", "/content", special, []byte("NEWER!"), http.Header{"If-Match": {tag}}), 200)
		if updated["contentRevision"] == created["contentRevision"] {
			t.Fatal("replacement retained old generation")
		}
		f.usage(t, 0, 6, 10)
		f.provider.mu.Lock()
		history := append([]objectAPIVersion(nil), f.provider.versions[nativeKey]...)
		f.provider.mu.Unlock()
		if len(history) != 2 || !bytes.Equal(history[0].Body, old.Body) {
			t.Fatal("replacement destroyed retained version")
		}
		stale := f.request(t, "PUT", "/content", special, []byte("stale"), http.Header{"If-Match": {tag}})
		objectsAPIJSON(t, stale, 412)
		f.usage(t, 0, 6, 10)
	})
	t.Run("native_marker_copy_move_and_current_key_delete", func(t *testing.T) {
		objectsAPIJSON(t, f.request(t, "POST", "", "", []byte(`{"kind":"folder","path":"/Folder/Empty","parents":true}`), nil), 201)
		objectsAPIJSON(t, f.request(t, "PUT", "/content", "/Folder/file", []byte("abcdef"), nil), 201)
		f.usage(t, 0, 12, 16)
		source := f.metadata(t, "/Folder")
		objectsAPIJSON(t, f.request(t, "POST", "/copy", "", []byte(fmt.Sprintf(`{"from":"/Folder","to":"/Copy","ifSourceRevision":%q}`, source["revision"])), nil), 201)
		f.usage(t, 0, 18, 22)
		before := f.metadata(t, "/Copy/file")
		original := f.metadata(t, "/Folder/file")
		if before["contentRevision"] == original["contentRevision"] {
			t.Fatal("copy aliased source content identity")
		}
		folder := f.metadata(t, "/Copy")
		objectsAPIJSON(t, f.request(t, "POST", "/move", "", []byte(fmt.Sprintf(`{"from":"/Copy","to":"/Moved","ifRevision":%q}`, folder["revision"])), nil), 200)
		f.usage(t, 0, 18, 28)
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
		f.usage(t, 0, 12, 28)
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
		f.usage(t, 0, 17, 33)
		body := f.request(t, "GET", "/content", "/Moved/file", nil, nil)
		if body.Code != 200 || body.Body.String() != "again" {
			t.Fatalf("recreated current key wrong %d %s", body.Code, body.Body.String())
		}
		objectsAPIJSON(t, f.request(t, "PUT", "/content", "/private", []byte("other"), http.Header{"Authorization": {"Bearer " + f.tokens[1]}}), 201)
		f.usage(t, 1, 5, 5)
		objectsAPIJSON(t, f.request(t, "GET", "/content", "/private", nil, nil), 404)
	})
	t.Run("implicit_prefix_mutation_is_not_silently_admitted", func(t *testing.T) {
		key := "drive/" + f.owners[0] + "/Implicit/file"
		f.provider.mu.Lock()
		f.provider.record(key, []byte("external"), "text/plain", map[string]string{}, false)
		before := len(f.provider.versions)
		f.provider.mu.Unlock()
		rejected := f.request(t, "PUT", "/content", "/Implicit/new", []byte("not-published"), nil)
		objectsAPIJSON(t, rejected, 422)
		f.provider.mu.Lock()
		defer f.provider.mu.Unlock()
		if len(f.provider.versions) != before || f.provider.puts["drive/"+f.owners[0]+"/Implicit/new"] != 0 {
			t.Fatal("unsupported implicit folder acquired a native effect")
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

func TestFilesObjectsParentMarkerRecreationInvalidatesIngress(t *testing.T) {
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
	f.provider.record(parentKey, nil, "", nil, true)
	f.provider.record(parentKey, nil, "application/x-directory", map[string]string{"eam-intent": "external-new-parent"}, false)
	current, _ := f.provider.current(parentKey)
	f.provider.mu.Unlock()
	if old.ID == current.ID {
		t.Fatal("fixture did not recreate the marker generation")
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

func TestFilesObjectsInvalidNativeVersionRemainsUnknown(t *testing.T) {
	f := newObjectsAPIFixture(t)
	key := "drive/" + f.owners[0] + "/invalid-version"
	f.provider.mu.Lock()
	f.provider.invalidVersionKey = key
	f.provider.mu.Unlock()
	response := f.request(t, "PUT", "/content", "/invalid-version", []byte("native"), nil)
	failure := objectsAPIJSON(t, response, 503)
	id, ok := failure["operationId"].(string)
	if !ok || id == "" {
		t.Fatal("missing durable identity after untrustworthy native response", failure)
	}
	state := objectsAPIJSON(t, f.request(t, "GET", "/operations/"+id, "", nil, nil), 200)
	if state["state"] != "outcome_unknown" {
		t.Fatal("invalid version acknowledgement became definite completion", state)
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
