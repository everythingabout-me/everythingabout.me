package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/treescan"
	"github.com/jackc/pgx/v5"
)

// This opt-in test exercises the actual router, authentication SQL, native owner
// factory and index. It never loads application configuration or a .env file and
// creates/drops its own database in an explicitly supplied disposable Postgres.
func TestFilesRuntimePostgresConformance(t *testing.T) {
	databaseURL := os.Getenv("EAM_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; disposable actual runtime integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	database := "eam_files_test_" + hex.EncodeToString(random[:])
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, e := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); e != nil {
			t.Error(e)
		}
	}()
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	db, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("repository migrations unavailable", err)
	}
	for _, migration := range migrations {
		data, e := os.ReadFile(migration)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Pool().Exec(ctx, string(data)); e != nil {
			t.Fatalf("migration %s: %v", filepath.Base(migration), e)
		}
	}
	cfg := &config.Config{AppSecret: "runtime-conformance-independent-secret", SessionTTLDays: 1, FileCookieOrigins: []string{"https://files.example"}, Storage: config.Storage{Backend: "local", ConnectionID: "runtime-test", Generation: 1, Local: config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir()}}}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ownerIDs := make([]string, 2)
	content := map[string]string{"Photos/A.txt": "0123456789", "Photos/Z.txt": "upper", "Photos/a-lower.txt": "lower", "Photos/ +%?#.txt ": "special", "Photos/中文.txt": "unicode", "Photos/cafe\u0301.txt": "decomposed", "Photos/zero": "", "Photos/active.html": "<script>alert(1)</script>", "Photos/vector.svg": "<svg onload='alert(1)'/>", "Photos/literal%2F.txt": "literal", "Photos-old/secret.txt": "sibling-private"}
	for i := range ownerIDs {
		if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("runtime-%d@example.test", i)).Scan(&ownerIDs[i]); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(cfg.Storage.Local.ContentRoot, ownerIDs[i])
		system := filepath.Join(cfg.Storage.Local.SystemRoot, ownerIDs[i])
		if err = os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(system, 0700); err != nil {
			t.Fatal(err)
		}
		for name, data := range content {
			if i == 1 {
				data = "other-owner"
			}
			native := filepath.Join(root, name)
			if err = os.MkdirAll(filepath.Dir(native), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(native, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err = os.MkdirAll(filepath.Join(root, "Photos/Empty"), 0700); err != nil {
			t.Fatal(err)
		}
		driver, e := backend.Open(ctx, ownerIDs[i])
		if e != nil {
			t.Fatal(e)
		}
		scan, e := treescan.Scan(ctx, driver, treescan.Options{Names: driver.Describe().Names})
		driver.Close()
		if e != nil {
			t.Fatal(e)
		}
		if _, e = nodes.ApplyScan(ctx, db.Pool(), ownerIDs[i], scan, nodes.ScanApplyOptions{}); e != nil {
			t.Fatal(e)
		}
	}
	snapshot := func() string {
		t.Helper()
		var rows string
		if e := db.Pool().QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(n) order by owner_id,path),'[]'::jsonb)::text from nodes n`).Scan(&rows); e != nil {
			t.Fatal(e)
		}
		return rows
	}
	before := snapshot()
	defer func() {
		if snapshot() != before {
			t.Error("read/pending runtime routes mutated the file index")
		}
		for name, want := range content {
			got, e := os.ReadFile(filepath.Join(cfg.Storage.Local.ContentRoot, ownerIDs[0], name))
			if e != nil || string(got) != want {
				t.Errorf("native content changed at %q: %v", name, e)
			}
		}
	}()
	service := auth.New(db, cfg)
	cookie, _, err := service.CreateSession(ctx, ownerIDs[0], "runtime test", "")
	if err != nil {
		t.Fatal(err)
	}
	secondCookie, _, err := service.CreateSession(ctx, ownerIDs[0], "second session", "")
	if err != nil {
		t.Fatal(err)
	}
	token := func(owner, prefix string, read, write, events bool) string {
		t.Helper()
		plain, _, e := service.CreateToken(ctx, auth.NewTokenInput{UserID: owner, Name: "runtime", Kind: auth.KindApp, Prefix: prefix, CanRead: read, CanWrite: write, CanEvents: events})
		if e != nil {
			t.Fatal(e)
		}
		return plain
	}
	bearer := token(ownerIDs[0], "", true, false, false)
	scoped := token(ownerIDs[0], "Photos", true, false, false)
	other := token(ownerIDs[1], "", true, false, false)
	writeOnly := token(ownerIDs[0], "Photos", false, true, false)
	eventsOnly := token(ownerIDs[0], "Photos", false, false, true)
	server := New(cfg, db, backend.Legacy(), nil)
	server.SetStorageFactory(backend)
	handler := server.Handler()
	request := func(method, target, credential string, headers http.Header) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://files.example"+target, nil)
		r.Header = headers.Clone()
		if r.Header == nil {
			r.Header = make(http.Header)
		}
		if credential == "cookie" {
			r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		} else if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	endpoint := func(route, path string) string {
		return "/api/files" + route + "?" + url.Values{"path": {path}}.Encode()
	}
	object := func(w *httptest.ResponseRecorder, status int) map[string]any {
		t.Helper()
		if w.Code != status {
			t.Fatalf("HTTP=%d want=%d body=%s", w.Code, status, w.Body.String())
		}
		var obj map[string]any
		if e := json.Unmarshal(w.Body.Bytes(), &obj); e != nil {
			t.Fatal(e)
		}
		return obj
	}
	problem := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		p := object(w, status)
		if p["code"] != code || w.Header().Get("Content-Type") != "application/problem+json" || p["requestId"] == "" {
			t.Fatalf("invalid Problem: %v headers=%v", p, w.Header())
		}
	}

	t.Run("same routes with real cookie and bearer", func(t *testing.T) {
		for _, route := range []string{"", "/list", "/search", "/capabilities"} {
			target := endpoint(route, "/Photos")
			if route == "/search" {
				target += "&q=txt"
			}
			a := object(request("GET", target, "cookie", nil), 200)
			b := object(request("GET", target, bearer, nil), 200)
			if !reflect.DeepEqual(a, b) {
				t.Fatalf("cookie/Bearer resources diverge: %s\n%v\n%v", route, a, b)
			}
			wire, _ := json.Marshal(a)
			for _, private := range []string{"\"ok\":", "\"data\":", "\"id\":", "\"fileId\":", "\"parentId\":", "tmpSecret", "provider", "drive/"} {
				if strings.Contains(string(wire), private) {
					t.Fatalf("private representation leaked: %s", wire)
				}
			}
		}
		a := object(request("GET", "/api/files/me", "cookie", nil), 200)
		b := object(request("GET", "/api/files/me", bearer, nil), 200)
		if a["csrfToken"] == nil || b["csrfToken"] != nil || !reflect.DeepEqual(a["usage"], b["usage"]) || a["write"] != false {
			t.Fatalf("invalid principal/bootstrap resources: %v %v", a, b)
		}
		caps := object(request("GET", endpoint("/capabilities", "/Photos"), scoped, nil), 200)
		if caps["range"] != true || caps["pinnedRead"] != false || caps["write"] != false || caps["strongContentValidators"] != false {
			t.Fatalf("untruthful local capabilities: %v", caps)
		}
	})
	t.Run("auth selection permissions and owner scopes", func(t *testing.T) {
		for _, value := range []string{"", " \t", "Bearer invalid", "Basic unsupported"} {
			problem(request("GET", endpoint("", "/Photos/A.txt"), "cookie", http.Header{"Authorization": {value}}), 401, "unauthenticated")
		}
		if w := request("GET", endpoint("/content", "/Photos/A.txt"), bearer, http.Header{"Cookie": {auth.SessionCookie + "=invalid"}}); w.Code != 200 || w.Body.String() != "0123456789" {
			t.Fatal("valid Bearer fell back to invalid cookie")
		}
		search := object(request("GET", "/api/files/search?q=private", scoped, nil), 200)
		if len(search["items"].([]any)) != 0 {
			t.Fatal("search leaked sibling scope")
		}
		for _, credential := range []string{writeOnly, eventsOnly} {
			for _, route := range []string{"", "/list", "/search", "/me", "/capabilities", "/content"} {
				problem(request("GET", endpoint(route, "/Photos"), credential, nil), 403, "forbidden")
			}
		}
		problem(request("GET", endpoint("/content", "/Photos-old/secret.txt"), scoped, nil), 404, "not_found")
		problem(request("GET", endpoint("", "/"), scoped, nil), 404, "not_found")
		if w := request("GET", endpoint("/content", "/Photos/A.txt"), other, nil); w.Code != 200 || w.Body.String() != "other-owner" {
			t.Fatalf("owner isolation: %d %q", w.Code, w.Body.String())
		}
		for _, name := range []string{" +%?#.txt ", "中文.txt", "cafe\u0301.txt", "literal%2F.txt"} {
			w := request("GET", endpoint("/content", "/Photos/"+name), scoped, nil)
			if w.Code != 200 || w.Body.String() != content["Photos/"+name] {
				t.Fatalf("exact native spelling failed %q: %d %q", name, w.Code, w.Body.String())
			}
		}
		if w := request("GET", endpoint("/content", "/photos/A.txt"), scoped, nil); w.Code == 200 {
			t.Fatal("alternate native case silently accepted")
		}
	})
	t.Run("actual local content HTTP contract", func(t *testing.T) {
		target := endpoint("/content", "/Photos/A.txt")
		for _, credential := range []string{"cookie", bearer} {
			w := request("GET", target, credential, nil)
			if w.Code != 200 || w.Body.String() != "0123456789" || w.Header().Get("ETag") != "" || w.Header().Get("Content-Length") != "10" || w.Header().Get("Accept-Ranges") != "bytes" {
				t.Fatalf("invalid versionless read: %d %v %q", w.Code, w.Header(), w.Body.String())
			}
			for _, tc := range []struct {
				rangeValue, ifRange, want string
				status                    int
			}{{"bytes=2-5", "", "2345", 206}, {"bytes=-3", "", "789", 206}, {"bytes=8-", "", "89", 206}, {"bytes=0-1,4-5", "", "0123456789", 200}, {"bytes=2-5", "\"unproven\"", "0123456789", 200}} {
				headers := http.Header{"Range": {tc.rangeValue}}
				if tc.ifRange != "" {
					headers.Set("If-Range", tc.ifRange)
				}
				w = request("GET", target, credential, headers)
				if w.Code != tc.status || w.Body.String() != tc.want {
					t.Fatalf("range %s: %d %q", tc.rangeValue, w.Code, w.Body.String())
				}
			}
			w = request("HEAD", target, credential, http.Header{"Range": {"bytes=999-"}})
			if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
				t.Fatalf("HEAD mismatch: %d %v", w.Code, w.Header())
			}
			w = request("GET", target, credential, http.Header{"Range": {"bytes=999-"}})
			problem(w, 416, "range_not_satisfiable")
			if w.Header().Get("Content-Range") != "bytes */10" {
				t.Fatal("missing 416 size")
			}
		}
		problem(request("GET", target, bearer, http.Header{"If-Match": {`"unproven"`}}), 412, "precondition_failed")
		if w := request("GET", target, bearer, http.Header{"If-None-Match": {"*"}}); w.Code != 304 || w.Body.Len() != 0 {
			t.Fatal("existing representation wildcard condition failed")
		}
		zero := request("GET", endpoint("/content", "/Photos/zero"), bearer, nil)
		if zero.Code != 200 || zero.Body.Len() != 0 || zero.Header().Get("Content-Length") != "0" {
			t.Fatal("zero-byte read failed")
		}
		for _, name := range []string{"active.html", "vector.svg"} {
			w := request("GET", endpoint("/content", "/Photos/"+name)+"&disposition=inline", bearer, nil)
			if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("unsafe active content response: %v", w.Header())
			}
		}
		problem(request("GET", endpoint("/content", "/Photos"), bearer, nil), 409, "not_a_file")
	})
	t.Run("query scoped stable pagination", func(t *testing.T) {
		first := object(request("GET", endpoint("/list", "/Photos")+"&limit=2", scoped, nil), 200)
		cursor, ok := first["nextCursor"].(string)
		if !ok || cursor == "" {
			t.Fatal("missing bounded cursor")
		}
		paths := []string{}
		page := first
		for {
			for _, v := range page["items"].([]any) {
				paths = append(paths, v.(map[string]any)["path"].(string))
			}
			next, ok := page["nextCursor"].(string)
			if !ok {
				break
			}
			page = object(request("GET", endpoint("/list", "/Photos")+"&limit=2&cursor="+url.QueryEscape(next), scoped, nil), 200)
		}
		if len(paths) != 11 || paths[0] != "/Photos/Empty" {
			t.Fatalf("incomplete or unstable pagination: %v", paths)
		}
		positions := map[string]int{}
		for i, p := range paths {
			if _, ok := positions[p]; ok {
				t.Fatal("duplicate path")
			}
			positions[p] = i
		}
		if positions["/Photos/A.txt"] >= positions["/Photos/Z.txt"] || positions["/Photos/Z.txt"] >= positions["/Photos/a-lower.txt"] {
			t.Fatalf("NFC byte sort changed: %v", paths)
		}
		for _, target := range []string{endpoint("/list", "/Photos-old"), endpoint("/search", "/Photos") + "&q=txt", endpoint("/list", "/Photos") + "&kind=file"} {
			w := request("GET", target+"&cursor="+url.QueryEscape(cursor), scoped, nil)
			if w.Code < 400 {
				t.Fatalf("cursor query reuse accepted %s", target)
			}
		}
		search := object(request("GET", endpoint("/search", "/Photos")+"&q=txt&limit=1", scoped, nil), 200)
		searchCursor, ok := search["nextCursor"].(string)
		if !ok || searchCursor == "" {
			t.Fatal("search cursor missing")
		}
		for _, filter := range []string{"q=active", "q=txt&kind=folder", "q=txt&mediaType=image%2Fpng"} {
			if w := request("GET", endpoint("/search", "/Photos")+"&"+filter+"&cursor="+url.QueryEscape(searchCursor), scoped, nil); w.Code < 400 {
				t.Fatal("search query cursor reused across filters")
			}
		}
		if w := request("GET", endpoint("/list", "/Photos")+"&cursor="+url.QueryEscape(cursor+"x"), scoped, nil); w.Code < 400 {
			t.Fatal("tampered cursor accepted")
		}
		if w := request("GET", endpoint("/list", "/Photos")+"&cursor="+url.QueryEscape(cursor), bearer, nil); w.Code < 400 {
			t.Fatal("cursor crossed principal")
		}
	})
	t.Run("cookie CSRF before pending mutations and CORS", func(t *testing.T) {
		csrf := object(request("GET", "/api/files/me", "cookie", nil), 200)["csrfToken"].(string)
		for _, route := range []string{"/content?path=%2FPhotos%2Fnew", "/uploads/u_fixture/parts/1", "/operations/o_fixture/cancel"} {
			method := "PUT"
			if strings.Contains(route, "cancel") {
				method = "POST"
			}
			target := "/api/files" + route
			problem(request(method, target, "cookie", nil), 403, "forbidden")
			problem(request(method, target, "cookie", http.Header{"Origin": {"https://evil.example"}, "X-CSRF-Token": {csrf}, "X-Forwarded-Host": {"files.example"}}), 403, "forbidden")
			problem(request(method, target, "cookie", http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}}), 501, "not_implemented")
			h := http.Header{"Origin": {"https://files.example"}, "X-CSRF-Token": {csrf}, "Cookie": {auth.SessionCookie + "=" + secondCookie}}
			problem(request(method, target, "", h), 403, "forbidden")
			problem(request(method, target, bearer, nil), 501, "not_implemented")
		}
		for _, tc := range []struct {
			origin, headers    string
			status             int
			allow, credentials string
		}{{"https://app.example", "authorization,range,content-digest", 204, "*", ""}, {"https://files.example", "x-csrf-token,content-type", 204, "https://files.example", "true"}, {"https://evil.example", "x-csrf-token", 403, "", ""}} {
			w := request("OPTIONS", "/api/files/content", "", http.Header{"Origin": {tc.origin}, "Access-Control-Request-Method": {"PUT"}, "Access-Control-Request-Headers": {tc.headers}})
			if w.Code != tc.status || w.Header().Get("Access-Control-Allow-Origin") != tc.allow || w.Header().Get("Access-Control-Allow-Credentials") != tc.credentials {
				t.Fatalf("preflight boundary: %d %v", w.Code, w.Header())
			}
		}
	})
}
