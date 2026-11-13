package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// This opt-in suite exercises actual HTTP/authentication, Postgres transactions and
// admitted native local actions. It creates its own database and empty owner roots;
// it never loads application environment files or touches an existing user's tree.
func TestFilesMutationsPostgresLocalConformance(t *testing.T) {
	scratch := testpg.CreateDatabase(t, "eam_mutations_test_")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 20 {
		t.Fatalf("P4 migrations unavailable: %d %v", len(migrations), err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, migration := range migrations {
			data, e := os.ReadFile(migration)
			if e != nil {
				t.Fatal(e)
			}
			if _, e = db.Pool().Exec(ctx, string(data)); e != nil {
				t.Fatalf("migration %s pass %d: %v", filepath.Base(migration), pass+1, e)
			}
		}
	}
	cfg := &config.Config{AppSecret: "p4-http-independent-test-secret", SessionTTLDays: 1, FileCookieOrigins: []string{"https://files.example"}, Storage: config.Storage{Backend: "local", ConnectionID: "p4-runtime", Generation: 1, CoordinatedWrites: true, MaxSinglePutBytes: 4096, Local: config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir()}}}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	journal := fileops.NewPostgres(db.Pool())
	var owners [2]string
	for i := range owners {
		if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("mutation-%d@example.test", i)).Scan(&owners[i]); err != nil {
			t.Fatal(err)
		}
		for _, base := range []string{cfg.Storage.Local.ContentRoot, cfg.Storage.Local.SystemRoot} {
			if err = os.Mkdir(filepath.Join(base, owners[i]), 0700); err != nil {
				t.Fatal(err)
			}
		}
		driver, e := backend.Open(ctx, owners[i])
		if e != nil {
			t.Fatal(e)
		}
		native, ok := driver.(fileops.NativeActions)
		if !ok {
			driver.Close()
			t.Fatal("admitted local root does not implement native actions")
		}
		binding := native.StorageBinding()
		root := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive}
		driver.Close()
		if e = journal.Initialize(ctx, root, fileops.Baseline{LogicalLimit: 1 << 20, PhysicalLimit: 2 << 20, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"test":"fresh empty native roots and empty owner index, no legacy writers"}`)}); e != nil {
			t.Fatal(e)
		}
	}
	service := auth.New(db, cfg)
	cookie, _, err := service.CreateSession(ctx, owners[0], "P4 HTTP fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	makeToken := func(owner, prefix string, read, write, events bool) string {
		plain, _, e := service.CreateToken(ctx, auth.NewTokenInput{UserID: owner, Name: "P4 HTTP", Kind: auth.KindApp, Prefix: prefix, CanRead: read, CanWrite: write, CanEvents: events})
		if e != nil {
			t.Fatal(e)
		}
		return plain
	}
	bearer := makeToken(owners[0], "", true, true, false)
	unrelated := makeToken(owners[0], "", true, true, false)
	readOnly := makeToken(owners[0], "", true, false, false)
	writeOnly := makeToken(owners[0], "Scope", false, true, false)
	scoped := makeToken(owners[0], "Scope", true, true, false)
	eventsOnly := makeToken(owners[0], "", false, false, true)
	otherOwner := makeToken(owners[1], "", true, true, false)
	server := New(cfg, db, backend.Legacy(), nil)
	server.SetStorageFactory(backend)
	workerCtx, stopWorkers := context.WithCancel(ctx)
	server.workers().ctx = workerCtx
	defer func() {
		stopWorkers()
		deadline := time.Now().Add(5 * time.Second)
		for {
			workers := server.workers()
			workers.mu.Lock()
			active := len(workers.active)
			workers.mu.Unlock()
			if active == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Error("HTTP action workers did not stop before fixture cleanup")
				break
			}
			time.Sleep(time.Millisecond)
		}
	}()
	handler := server.Handler()
	endpoint := func(resource, path string) string {
		return "/api/files" + resource + "?" + url.Values{"path": {path}}.Encode()
	}
	request := func(method, target, credential string, body []byte, headers http.Header) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://files.example"+target, bytes.NewReader(body)).WithContext(ctx)
		r.Header = headers.Clone()
		if r.Header == nil {
			r.Header = make(http.Header)
		}
		if credential == "cookie" {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		} else if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		if raw := r.Header.Get("Content-Length"); raw != "" {
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil {
				panic(e)
			}
			r.ContentLength = n
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	object := func(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP=%d want=%d headers=%v body=%s", w.Code, status, w.Header(), w.Body.String())
		}
		var value map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &value); e != nil {
			t.Fatal(e)
		}
		if _, ok := value["ok"]; ok {
			t.Fatal("legacy response envelope exposed")
		}
		return value
	}
	problem := func(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		value := object(t, w, status)
		if value["code"] != code || w.Header().Get("Content-Type") != "application/problem+json" || value["requestId"] == "" {
			t.Fatalf("unexpected Problem: %v %v", value, w.Header())
		}
	}
	metadata := func(t *testing.T, path string) map[string]any {
		t.Helper()
		return object(t, request("GET", endpoint("", path), bearer, nil, nil), 200)
	}
	nativeBytes := func(t *testing.T, owner, path string) []byte {
		t.Helper()
		data, e := os.ReadFile(filepath.Join(cfg.Storage.Local.ContentRoot, owner, path))
		if e != nil {
			t.Fatal(e)
		}
		return data
	}
	assertAbsent := func(t *testing.T, owner, path string) {
		t.Helper()
		if _, e := os.Lstat(filepath.Join(cfg.Storage.Local.ContentRoot, owner, path)); !os.IsNotExist(e) {
			t.Fatalf("native path still exists %q: %v", path, e)
		}
	}
	put := func(t *testing.T, path string, data []byte, headers http.Header, status int) map[string]any {
		t.Helper()
		return object(t, request("PUT", endpoint("/content", path), bearer, data, headers), status)
	}
	mkdir := func(t *testing.T, path string, parents bool, status int) map[string]any {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"kind": "folder", "path": path, "parents": parents})
		return object(t, request("POST", "/api/files", bearer, data, http.Header{"Content-Type": {"application/json"}}), status)
	}
	action := func(method, route, from, to, revision string, headers http.Header) *httptest.ResponseRecorder {
		body := map[string]any{"from": from, "to": to}
		if route == "copy" {
			body["ifSourceRevision"] = revision
		} else {
			body["ifRevision"] = revision
		}
		data, _ := json.Marshal(body)
		return request(method, "/api/files/"+route, bearer, data, headers)
	}
	operationPath := func(t *testing.T, w *httptest.ResponseRecorder) string {
		t.Helper()
		location := w.Header().Get("Location")
		u, e := url.Parse(location)
		if e != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/api/files/operations/") {
			t.Fatalf("invalid operation location %q", location)
		}
		return u.Path
	}
	poll := func(t *testing.T, path, credential string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			value := object(t, request("GET", path, credential, nil, nil), 200)
			state := value["state"]
			if state == "succeeded" || state == "failed" || state == "cancelled" {
				return value
			}
			if time.Now().After(deadline) {
				t.Fatalf("operation did not finish: %v", value)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	csrf := object(t, request("GET", "/api/files/me", "cookie", nil, nil), 200)["csrfToken"].(string)

	t.Run("exact streaming PUT and immutable replay", func(t *testing.T) {
		path := "/ +%?#.bin "
		data := []byte{0, 255, 13, 10, 42, 0, 7}
		sum := sha256.Sum256(data)
		digest := "sha-256=:" + base64.StdEncoding.EncodeToString(sum[:]) + ":"
		h := http.Header{"Content-Digest": {digest}, "Content-Type": {"application/octet-stream"}, "Idempotency-Key": {"http-native-create"}, "X-Source-Modified-At": {"2024-03-04T05:06:07Z"}}
		response := request("PUT", endpoint("/content", path), bearer, data, h)
		value := object(t, response, 201)
		if value["path"] != path || value["sourceModifiedAt"] != "2024-03-04T05:06:07Z" || value["contentRevision"] == nil {
			t.Fatalf("accepted native representation lost: %v", value)
		}
		if !bytes.Equal(nativeBytes(t, owners[0], path), data) {
			t.Fatal("native binary bytes changed")
		}
		fileInfo, e := os.Stat(filepath.Join(cfg.Storage.Local.ContentRoot, owners[0], path))
		if e != nil || !fileInfo.ModTime().Equal(time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)) {
			t.Fatal("native source mtime not preserved", e)
		}
		wire, _ := json.Marshal(value)
		if id, ok := value["id"].(string); !ok || !fc.ValidEntryID(id) {
			t.Fatalf("missing public file identity: %v", value)
		}
		for _, secret := range []string{"NativeRef", "ContentIdentity", "Checkpoint", "drive/", "tmpSecret"} {
			if strings.Contains(string(wire), secret) {
				t.Fatalf("private mutation state leaked: %s", wire)
			}
		}
		get := metadata(t, path)
		if get["revision"] != value["revision"] || get["contentRevision"] != value["contentRevision"] {
			t.Fatalf("settled PUT response diverges from GET: %v / %v", value, get)
		}
		replay := object(t, request("PUT", endpoint("/content", path), bearer, data, h), 201)
		a, _ := json.Marshal(value)
		b, _ := json.Marshal(replay)
		if !bytes.Equal(a, b) {
			t.Fatal("idempotent PUT changed durable resource")
		}
		different := append([]byte(nil), data...)
		different[0] = 1
		problem(t, request("PUT", endpoint("/content", path), bearer, different, h), 409, "idempotency_conflict")
		problem(t, request("PUT", endpoint("/content", path), bearer, data, nil), 409, "name_conflict")
		problem(t, request("PUT", endpoint("/content", path), bearer, data, http.Header{"If-None-Match": {"*"}}), 412, "precondition_failed")
		zero := put(t, "/zero", nil, nil, 201)
		if zero["size"] != float64(0) || len(nativeBytes(t, owners[0], "zero")) != 0 {
			t.Fatal("zero-byte PUT lost")
		}
		op := object(t, request("GET", operationPath(t, response), bearer, nil, nil), 200)
		if op["state"] != "succeeded" {
			t.Fatalf("settled PUT operation unavailable: %v", op)
		}
	})
	t.Run("conditional replacement length and integrity", func(t *testing.T) {
		first := put(t, "/replace.txt", []byte("old-complete"), nil, 201)
		tag := `"` + first["contentRevision"].(string) + `"`
		problem(t, request("PUT", endpoint("/content", "/replace.txt"), bearer, []byte("bad"), http.Header{"If-Match": {tag}, "Content-Length": {"7"}}), 400, "length_mismatch")
		if string(nativeBytes(t, owners[0], "replace.txt")) != "old-complete" {
			t.Fatal("failed ingress destroyed old bytes")
		}
		problem(t, request("PUT", endpoint("/content", "/replace.txt"), bearer, []byte("bad"), http.Header{"If-Match": {tag}, "Content-Length": {"2"}}), 400, "length_mismatch")
		bad := sha256.Sum256([]byte("different"))
		problem(t, request("PUT", endpoint("/content", "/replace.txt"), bearer, []byte("new"), http.Header{"If-Match": {tag}, "Content-Digest": {"sha-256=:" + base64.StdEncoding.EncodeToString(bad[:]) + ":"}}), 422, "checksum_mismatch")
		if string(nativeBytes(t, owners[0], "replace.txt")) != "old-complete" {
			t.Fatal("checksum failure changed native content")
		}
		replacement := put(t, "/replace.txt", []byte("new"), http.Header{"If-Match": {tag}}, 200)
		if replacement["contentRevision"] == first["contentRevision"] || string(nativeBytes(t, owners[0], "replace.txt")) != "new" {
			t.Fatal("replacement not published")
		}
		problem(t, request("PUT", endpoint("/content", "/replace.txt"), bearer, []byte("stale"), http.Header{"If-Match": {tag}}), 412, "precondition_failed")
		problem(t, request("PUT", endpoint("/content", "/unknown-length"), bearer, []byte("x"), http.Header{"Content-Length": {"-1"}}), 411, "length_required")
		problem(t, request("PUT", endpoint("/content", "/too-large"), bearer, bytes.Repeat([]byte("x"), 4097), nil), 413, "request_too_large")
		problem(t, request("PUT", endpoint("/content", "/missing/a"), bearer, []byte("x"), nil), 409, "parent_missing")
		assertAbsent(t, owners[0], "missing")
	})
	t.Run("cookie bearer and scoped mutation access", func(t *testing.T) {
		mkdir(t, "/Scope", false, 201)
		target := endpoint("/content", "/Scope/cookie.txt")
		problem(t, request("PUT", target, "cookie", []byte("cookie"), nil), 403, "forbidden")
		problem(t, request("PUT", target, "cookie", []byte("cookie"), http.Header{"Origin": {"https://evil.example"}, "X-CSRF-Token": {csrf}}), 403, "forbidden")
		h := http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}}
		object(t, request("PUT", target, "cookie", []byte("cookie"), h), 201)
		for _, explicit := range []string{"", " \t", "Bearer invalid"} {
			h2 := h.Clone()
			h2.Set("Authorization", explicit)
			problem(t, request("PUT", endpoint("/content", "/Scope/explicit"), "cookie", []byte("x"), h2), 401, "unauthenticated")
		}
		for _, credential := range []string{readOnly, eventsOnly} {
			problem(t, request("PUT", endpoint("/content", "/Scope/forbidden"), credential, []byte("x"), nil), 403, "forbidden")
		}
		object(t, request("PUT", endpoint("/content", "/Scope/write-only"), writeOnly, []byte("w"), nil), 201)
		problem(t, request("GET", endpoint("/content", "/Scope/write-only"), writeOnly, nil, nil), 403, "forbidden")
		problem(t, request("PUT", endpoint("/content", "/Scope-old/private"), scoped, []byte("x"), nil), 404, "not_found")
		object(t, request("PUT", endpoint("/content", "/owner-isolated"), otherOwner, []byte("owner-b"), nil), 201)
		assertAbsent(t, owners[0], "owner-isolated")
		problem(t, request("GET", endpoint("", "/owner-isolated"), bearer, nil, nil), 404, "not_found")
	})
	t.Run("native mkdir hierarchy and collisions", func(t *testing.T) {
		created := mkdir(t, "/Tree/nested/Empty", true, 201)
		for _, p := range []string{"Tree", "Tree/nested", "Tree/nested/Empty"} {
			info, e := os.Stat(filepath.Join(cfg.Storage.Local.ContentRoot, owners[0], p))
			if e != nil || !info.IsDir() {
				t.Fatal("native ancestor not created", p, e)
			}
		}
		repeated := mkdir(t, "/Tree/nested/Empty", true, 200)
		if repeated["revision"] != created["revision"] {
			t.Fatalf("existing mkdir changed revision: %v %v", created, repeated)
		}
		current := metadata(t, "/Tree/nested/Empty")
		if current["revision"] != created["revision"] {
			t.Fatal("mkdir result differs from GET")
		}
		problem(t, request("POST", "/api/files", bearer, []byte(`{"kind":"folder","path":"/Absent/child"}`), nil), 409, "parent_missing")
		assertAbsent(t, owners[0], "Absent")
		put(t, "/occupied", []byte("keep"), nil, 201)
		problem(t, request("POST", "/api/files", bearer, []byte(`{"kind":"folder","path":"/occupied"}`), nil), 409, "name_conflict")
		if string(nativeBytes(t, owners[0], "occupied")) != "keep" {
			t.Fatal("mkdir collision deleted existing file")
		}
	})
	t.Run("finite folder copy and move preserve native paths", func(t *testing.T) {
		mkdir(t, "/Source/child/Empty", true, 201)
		source := put(t, "/Source/child/a.txt", []byte("independent"), nil, 201)
		root := metadata(t, "/Source")
		copied := object(t, action("POST", "copy", "/Source", "/Copied", root["revision"].(string), nil), 201)
		if copied["path"] != "/Copied" || string(nativeBytes(t, owners[0], "Copied/child/a.txt")) != "independent" {
			t.Fatal("native folder copy did not finish")
		}
		if info, e := os.Stat(filepath.Join(cfg.Storage.Local.ContentRoot, owners[0], "Copied/child/Empty")); e != nil || !info.IsDir() {
			t.Fatal("copy lost empty directory")
		}
		if got := metadata(t, "/Copied"); got["revision"] != copied["revision"] {
			t.Fatalf("copy root response differs from GET: %v %v", copied, got)
		}
		put(t, "/Source/child/a.txt", []byte("source-changed"), http.Header{"If-Match": {`"` + source["contentRevision"].(string) + `"`}}, 200)
		if string(nativeBytes(t, owners[0], "Copied/child/a.txt")) != "independent" {
			t.Fatal("copy shares mutable bytes")
		}
		before := metadata(t, "/Copied/child/a.txt")
		copyRoot := metadata(t, "/Copied")
		moved := object(t, action("POST", "move", "/Copied", "/Moved", copyRoot["revision"].(string), nil), 200)
		assertAbsent(t, owners[0], "Copied")
		after := metadata(t, "/Moved/child/a.txt")
		if after["contentRevision"] != before["contentRevision"] || after["revision"] == before["revision"] || after["path"] != "/Moved/child/a.txt" {
			t.Fatalf("move lost content generation or path revision: before%v after%v", before, after)
		}
		if got := metadata(t, "/Moved"); got["revision"] != moved["revision"] {
			t.Fatal("move root response differs from GET")
		}
		response := request("GET", endpoint("/content", "/Moved/child/a.txt"), bearer, nil, nil)
		if response.Code != 200 || response.Body.String() != "independent" || response.Header().Get("ETag") != `"`+after["contentRevision"].(string)+`"` {
			t.Fatal("moved content ETag is not durable generation")
		}
		put(t, "/collision", []byte("destination"), nil, 201)
		sourceRoot := metadata(t, "/Source")
		problem(t, action("POST", "copy", "/Source", "/collision", sourceRoot["revision"].(string), nil), 409, "name_conflict")
		if string(nativeBytes(t, owners[0], "collision")) != "destination" {
			t.Fatal("copy collision pre-deleted destination")
		}
		problem(t, action("POST", "copy", "/Source", "/Tree", sourceRoot["revision"].(string), nil), 409, "name_conflict")
		if _, e := os.Stat(filepath.Join(cfg.Storage.Local.ContentRoot, owners[0], "Tree/nested/Empty")); e != nil {
			t.Fatal("directory collision changed preexisting native tree", e)
		}
		assertAbsent(t, owners[0], "Tree/child")
		problem(t, action("POST", "move", "/Source", "/Source/inside", sourceRoot["revision"].(string), nil), 400, "invalid_request")
		problem(t, action("POST", "copy", "/Source", "/NeedsRevision", "", nil), 428, "precondition_required")
		problem(t, action("POST", "move", "/Source", "/Stale", "nr_stale", nil), 412, "precondition_failed")
		assertAbsent(t, owners[0], "Stale")
	})
	t.Run("native deletion and retained operation provenance", func(t *testing.T) {
		mkdir(t, "/Delete/Empty", true, 201)
		put(t, "/Delete/a", []byte("remove"), nil, 201)
		problem(t, request("DELETE", endpoint("", "/Delete"), bearer, nil, nil), 409, "directory_not_empty")
		if string(nativeBytes(t, owners[0], "Delete/a")) != "remove" {
			t.Fatal("nonrecursive deletion removed descendant")
		}
		w := request("DELETE", endpoint("", "/Delete/Empty"), bearer, nil, nil)
		if w.Code != 204 {
			t.Fatalf("empty delete: %d %s", w.Code, w.Body.String())
		}
		assertAbsent(t, owners[0], "Delete/Empty")
		w = request("DELETE", endpoint("", "/Delete")+"&recursive=true", bearer, nil, http.Header{"Idempotency-Key": {"delete-finite"}})
		if w.Code != 204 {
			t.Fatalf("recursive delete: %d %s", w.Code, w.Body.String())
		}
		assertAbsent(t, owners[0], "Delete")
		var savedDeleteID string
		if e := db.Pool().QueryRow(ctx, `select id::text from file_operations where owner_id=$1::uuid and action='delete' and idempotency_key='delete-finite'`, owners[0]).Scan(&savedDeleteID); e != nil {
			t.Fatal(e)
		}
		location := "/api/files/operations/" + savedDeleteID
		operation := object(t, request("GET", location, bearer, nil, nil), 200)
		if operation["state"] != "succeeded" || operation["result"].(map[string]any)["deleted"] != true {
			t.Fatalf("deleted-source result lost: %v", operation)
		}
		problem(t, request("GET", location, unrelated, nil, nil), 403, "forbidden")
		problem(t, request("GET", location, readOnly, nil, nil), 403, "forbidden")
		replay := request("DELETE", endpoint("", "/Delete")+"&recursive=true", bearer, nil, http.Header{"Idempotency-Key": {"delete-finite"}})
		if replay.Code != 204 {
			t.Fatalf("delete replay needs removed source: %d %s", replay.Code, replay.Body.String())
		}
		var deleteCount int
		if e := db.Pool().QueryRow(ctx, `select count(*) from file_operations where owner_id=$1::uuid and action='delete' and idempotency_key='delete-finite'`, owners[0]).Scan(&deleteCount); e != nil || deleteCount != 1 {
			t.Fatalf("delete replay duplicated intent: %d %v", deleteCount, e)
		}
		put(t, "/DeleteFile", []byte("one"), nil, 201)
		w = request("DELETE", endpoint("", "/DeleteFile"), bearer, nil, nil)
		if w.Code != 204 {
			t.Fatalf("file delete: %d %s", w.Code, w.Body.String())
		}
		assertAbsent(t, owners[0], "DeleteFile")
	})
	t.Run("terminal PUT replay survives parent moves and deletion", func(t *testing.T) {
		mkdir(t, "/ReplayParent", false, 201)
		h := http.Header{"Idempotency-Key": {"replay-after-parent-change"}}
		first := request("PUT", endpoint("/content", "/ReplayParent/file"), bearer, []byte("saved-body"), h)
		accepted := object(t, first, 201)
		putLocation := operationPath(t, first)
		parent := metadata(t, "/ReplayParent")
		object(t, action("POST", "move", "/ReplayParent", "/ReplayMoved", parent["revision"].(string), nil), 200)
		assertAbsent(t, owners[0], "ReplayParent")
		for pass := 0; pass < 2; pass++ {
			replayed := request("PUT", endpoint("/content", "/ReplayParent/file"), bearer, []byte("saved-body"), h)
			result := object(t, replayed, 201)
			a, _ := json.Marshal(accepted)
			b, _ := json.Marshal(result)
			if !bytes.Equal(a, b) || operationPath(t, replayed) != putLocation {
				t.Fatal("terminal replay depended on live old parent or changed result")
			}
			assertAbsent(t, owners[0], "ReplayParent")
			if pass == 0 {
				deleted := request("DELETE", endpoint("", "/ReplayMoved")+"&recursive=true", bearer, nil, nil)
				if deleted.Code != 204 {
					t.Fatalf("remove moved parent: %d %s", deleted.Code, deleted.Body.String())
				}
				assertAbsent(t, owners[0], "ReplayMoved")
			}
		}
	})
	t.Run("stable identity through direct ID operations", func(t *testing.T) {
		root := metadata(t, "/")
		rootID := root["id"].(string)
		if !fc.ValidEntryID(rootID) || root["parentId"] != nil || root["parentPath"] != nil {
			t.Fatalf("root identity: %v", root)
		}
		folder := mkdir(t, "/Identity", false, 201)
		folderID := folder["id"].(string)
		scopedIdentity := makeToken(owners[0], "Identity", true, true, false)
		scopedFolder := object(t, request("GET", "/api/files?id="+folderID, scopedIdentity, nil, nil), 200)
		if scopedFolder["parentId"] != nil || scopedFolder["parentPath"] != nil {
			t.Fatalf("concealed parent leaked: %v", scopedFolder)
		}
		problem(t, request("GET", "/api/files?id="+rootID, scopedIdentity, nil, nil), 404, "not_found")
		body := []byte(`{"kind":"folder","destination":{"parent":{"id":"` + folderID + `"},"name":"Nested"}}`)
		nested := object(t, request("POST", "/api/files", bearer, body, nil), 201)
		if nested["parentId"] != folderID {
			t.Fatalf("wrong parent identity: %v", nested)
		}
		createTarget := "/api/files/content?parentId=" + folderID + "&name=note.txt"
		createHeaders := http.Header{"Idempotency-Key": {"identity-create"}}
		created := object(t, request("PUT", createTarget, bearer, []byte("old"), createHeaders), 201)
		id := created["id"].(string)
		problem(t, request("GET", "/api/files?id="+id, otherOwner, nil, nil), 404, "not_found")
		byID := object(t, request("GET", "/api/files?id="+id, bearer, nil, nil), 200)
		if byID["path"] != "/Identity/note.txt" || byID["parentId"] != folderID {
			t.Fatalf("ID lookup lost path: %v", byID)
		}
		read := request("GET", "/api/files/content?id="+id, bearer, nil, nil)
		if read.Code != 200 || read.Body.String() != "old" {
			t.Fatalf("ID content: %d %s", read.Code, read.Body.String())
		}
		replaceHeaders := http.Header{"If-Match": {`"` + created["contentRevision"].(string) + `"`}, "Idempotency-Key": {"identity-replace"}}
		replaced := object(t, request("PUT", "/api/files/content?id="+id, bearer, []byte("new"), replaceHeaders), 200)
		if replaced["id"] != id {
			t.Fatal("replacement changed identity")
		}
		moveBody := []byte(`{"from":{"id":"` + id + `"},"to":{"parent":{"id":"` + nested["id"].(string) + `"},"name":"renamed.txt"},"ifRevision":"` + replaced["revision"].(string) + `"}`)
		moveHeaders := http.Header{"Idempotency-Key": {"identity-move"}}
		moved := object(t, request("POST", "/api/files/move", bearer, moveBody, moveHeaders), 200)
		if moved["id"] != id || moved["path"] != "/Identity/Nested/renamed.txt" {
			t.Fatalf("move identity: %v", moved)
		}
		moveAgain := object(t, request("POST", "/api/files/move", bearer, moveBody, moveHeaders), 200)
		if moveAgain["revision"] != moved["revision"] {
			t.Fatal("ID move replay changed saved result")
		}
		replaceAgain := object(t, request("PUT", "/api/files/content?id="+id, bearer, []byte("new"), replaceHeaders), 200)
		if replaceAgain["revision"] != replaced["revision"] {
			t.Fatal("ID replacement replay followed moved path")
		}
		assertAbsent(t, owners[0], "Identity/note.txt")
		byID = object(t, request("GET", "/api/files?id="+id, bearer, nil, nil), 200)
		if byID["path"] != moved["path"] {
			t.Fatal("ID did not follow completed move")
		}
		copied := object(t, action("POST", "copy", moved["path"].(string), "/Identity/copy.txt", moved["revision"].(string), nil), 201)
		if copied["id"] == id {
			t.Fatal("copy reused source identity")
		}
		object(t, action("POST", "move", "/Identity/copy.txt", "/EscapedIdentity.txt", copied["revision"].(string), nil), 200)
		problem(t, request("GET", "/api/files/content?id="+copied["id"].(string), scopedIdentity, nil, nil), 404, "not_found")
		escapedCopy := []byte(`{"from":{"id":"` + copied["id"].(string) + `"},"to":"/Identity/forbidden.txt","ifSourceRevision":"` + copied["revision"].(string) + `"}`)
		problem(t, request("POST", "/api/files/copy", scopedIdentity, escapedCopy, nil), 404, "not_found")
		deleteHeaders := http.Header{"Idempotency-Key": {"identity-delete"}}
		deleted := request("DELETE", "/api/files?id="+id, bearer, nil, deleteHeaders)
		if deleted.Code != 204 {
			t.Fatalf("ID delete: %d %s", deleted.Code, deleted.Body.String())
		}
		problem(t, request("GET", "/api/files?id="+id, bearer, nil, nil), 404, "not_found")
		recreated := put(t, moved["path"].(string), []byte("recreated"), nil, 201)
		if recreated["id"] == id {
			t.Fatal("recreated file inherited retired identity")
		}
		if again := request("DELETE", "/api/files?id="+id, bearer, nil, deleteHeaders); again.Code != 204 {
			t.Fatalf("retired ID delete replay: %d %s", again.Code, again.Body.String())
		}
		object(t, request("POST", "/api/files/move", bearer, moveBody, moveHeaders), 200)
		object(t, request("PUT", "/api/files/content?id="+id, bearer, []byte("new"), replaceHeaders), 200)
		if string(nativeBytes(t, owners[0], "Identity/Nested/renamed.txt")) != "recreated" {
			t.Fatal("retired ID replay mutated recreated entry")
		}
		liveParent := metadata(t, "/Identity")
		object(t, action("POST", "move", "/Identity", "/IdentityMoved", liveParent["revision"].(string), nil), 200)
		createAgain := object(t, request("PUT", createTarget, bearer, []byte("old"), createHeaders), 201)
		if createAgain["id"] != created["id"] || createAgain["revision"] != created["revision"] {
			t.Fatal("parent ID create replay changed saved identity")
		}
		assertAbsent(t, owners[0], "Identity")
		assertAbsent(t, owners[0], "IdentityMoved/note.txt")
		problem(t, request("GET", "/api/files?id="+id+"&path=%2FIdentity", bearer, nil, nil), 400, "invalid_selector")
	})
	t.Run("upload and recursive mkdir replay survive changed parents", func(t *testing.T) {
		mkdirBody := []byte(`{"kind":"folder","path":"/ReplayAncestors/A/B","parents":true}`)
		mkdirHeaders := http.Header{"Idempotency-Key": {"mkdir-ancestor-replay"}}
		first := object(t, request("POST", "/api/files", bearer, mkdirBody, mkdirHeaders), 201)
		again := object(t, request("POST", "/api/files", bearer, mkdirBody, mkdirHeaders), 201)
		if first["id"] != again["id"] || first["revision"] != again["revision"] {
			t.Fatal("recursive mkdir replay changed saved result")
		}
		parent := mkdir(t, "/UploadReplay", false, 201)
		body := []byte(`{"destination":{"parent":{"id":"` + parent["id"].(string) + `"},"name":"empty.txt"},"size":0}`)
		headers := http.Header{"Idempotency-Key": {"upload-parent-replay"}}
		upload := object(t, request("POST", "/api/files/uploads", bearer, body, headers), 201)
		// An ordinary commit has nothing detached about it: the request settles the
		// publication and answers 200 with the file it published. A 202 here would make
		// the client poll for a result the server already had.
		committed := object(t, request("POST", upload["commitUrl"].(string), bearer, nil, nil), 200)
		if committed["path"] != "/UploadReplay/empty.txt" || committed["size"] != float64(0) {
			t.Fatalf("in-request commit did not return the published file: %v", committed)
		}
		current := metadata(t, "/UploadReplay")
		object(t, action("POST", "move", "/UploadReplay", "/UploadReplayMoved", current["revision"].(string), nil), 200)
		replay := object(t, request("POST", "/api/files/uploads", bearer, body, headers), 200)
		if replay["id"] != upload["id"] || replay["state"] != "committed" {
			t.Fatalf("moved parent upload replay: %v", replay)
		}
		assertAbsent(t, owners[0], "UploadReplay")
		if removed := request("DELETE", endpoint("", "/UploadReplayMoved")+"&recursive=true", bearer, nil, nil); removed.Code != 204 {
			t.Fatalf("delete upload parent: %d %s", removed.Code, removed.Body.String())
		}
		replay = object(t, request("POST", "/api/files/uploads", bearer, body, headers), 200)
		if replay["id"] != upload["id"] {
			t.Fatal("retired parent upload replay allocated a new session")
		}
	})
	t.Run("async status and cancellation before native work", func(t *testing.T) {
		mkdir(t, "/AsyncSource/Empty", true, 201)
		put(t, "/AsyncSource/file", []byte("async"), nil, 201)
		root := metadata(t, "/AsyncSource")
		accepted := action("POST", "copy", "/AsyncSource", "/AsyncTarget", root["revision"].(string), http.Header{"Prefer": {"respond-async"}})
		object(t, accepted, 202)
		location := operationPath(t, accepted)
		done := poll(t, location, bearer)
		if done["state"] != "succeeded" {
			t.Fatalf("async copy failed: %v", done)
		}
		if string(nativeBytes(t, owners[0], "AsyncTarget/file")) != "async" {
			t.Fatal("async result reported before native copy")
		}
		driver, e := backend.Open(ctx, owners[0])
		if e != nil {
			t.Fatal(e)
		}
		defer driver.Close()
		native := driver.(fileops.NativeActions)
		binding := native.StorageBinding()
		scopeRoot := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive}
		actor := fileops.Actor{ID: "owner:" + owners[0], CanRead: true, CanWrite: true, ManageOwner: true}
		actions, e := fileops.NewActions(journal, fileops.ActionsOptions{Authorize: service.AuthorizeFileOperation, Validate: func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil }, Commit: nodes.CommitFileAction})
		if e != nil {
			t.Fatal(e)
		}
		queued, _, e := actions.Prepare(ctx, fileops.ActionRequest{Root: scopeRoot, Actor: actor, Kind: "copy", From: "AsyncSource", To: "CancelledTarget", IfRevision: root["revision"].(string), IdempotencyKey: "cancel-before-native"}, native)
		if e != nil {
			t.Fatal(e)
		}
		cancelPath := "/api/files/operations/" + queued.ID
		// No worker has seen this admitted operation yet; cancellation must win before
		// the cancel endpoint queues its first safe processing step.
		h := http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}}
		response := request("POST", cancelPath+"/cancel", "cookie", nil, h)
		if response.Code != 200 && response.Code != 202 {
			t.Fatalf("cancel: %d %s", response.Code, response.Body.String())
		}
		cancelled := poll(t, cancelPath, "cookie")
		if cancelled["state"] != "cancelled" {
			t.Fatalf("pre-effect cancel did not settle truthfully: %v", cancelled)
		}
		assertAbsent(t, owners[0], "CancelledTarget")
		if string(nativeBytes(t, owners[0], "AsyncSource/file")) != "async" {
			t.Fatal("cancelled copy altered source")
		}
	})
}
