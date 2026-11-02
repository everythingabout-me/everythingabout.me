package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// Unimplemented provider methods panic if a rejected request reaches storage.
type deleteStorage struct {
	storage.Storage
	objects map[string]storage.ObjectInfo
	calls   int
}

func (s *deleteStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	s.calls++
	info, ok := s.objects[key]
	if !ok {
		return nil, nil
	}
	return &info, nil
}

func (s *deleteStorage) Remove(_ context.Context, key string) error {
	s.calls++
	delete(s.objects, key)
	return nil
}

func TestLegacyDeleteAuthentication(t *testing.T) {
	cfg := &config.Config{AppSecret: "delete-test"}
	server := &Server{cfg: cfg, auth: auth.New(nil, cfg)}
	for _, tc := range []struct {
		name, origin, bearer string
		status               int
	}{
		{"anonymous", "", "", http.StatusUnauthorized},
		{"Bearer does not replace cookie", "", "eam_live_test", http.StatusUnauthorized},
		{"cross-origin", "https://other.example", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "https://files.example/api/fs/delete?recursive=1", strings.NewReader(`{"path":"Docs"}`))
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), `"ok":false`) {
				t.Fatalf("HTTP=%d body=%s, want status %d and legacy error envelope", w.Code, w.Body.String(), tc.status)
			}
		})
	}
}

// Exercise the real cookie auth, handler, owner-scoped index and purge without COS.
// As with other runtime integration tests, use only an explicit disposable database.
func TestLegacyDeletePostgres(t *testing.T) {
	address := os.Getenv("EAM_TEST_DATABASE_URL")
	if address == "" {
		t.Skip("EAM_TEST_DATABASE_URL unset; disposable Postgres integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	name := fmt.Sprintf("eam_legacy_delete_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err = admin.Exec(ctx, "create database "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	}()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	db, err := store.Open(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("migrations unavailable", err)
	}
	for _, migration := range migrations {
		data, err := os.ReadFile(migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	objects := &deleteStorage{objects: map[string]storage.ObjectInfo{}}
	server := New(&config.Config{AppSecret: "legacy-delete-test", SessionTTLDays: 1}, db, objects, nil)
	owners := make([]string, 2)
	for i := range owners {
		if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("delete-%d@example.test", i)).Scan(&owners[i]); err != nil {
			t.Fatal(err)
		}
	}
	cookie, _, err := server.auth.CreateSession(ctx, owners[0], "test", "")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(owner, path string, dir bool) {
		t.Helper()
		key := paths.KeyFor(path, owner)
		if dir {
			key = paths.DirKeyFor(path, owner)
			_, err = server.nodes.CreateDirectory(ctx, path, owner)
		} else {
			_, err = server.nodes.UpsertFile(ctx, nodes.UpsertInput{OwnerID: owner, Path: path, Size: 3})
		}
		if err != nil {
			t.Fatal(err)
		}
		objects.objects[key] = storage.ObjectInfo{Key: key, Size: 3}
	}
	for _, owner := range owners {
		seed(owner, "Docs", true)
		seed(owner, "Docs/nested", true)
		seed(owner, "Docs/nested/file.txt", false)
		seed(owner, "Docs-old.txt", false)
		seed(owner, "empty", true)
		seed(owner, "file.txt", false)
	}
	seed(owners[1], "private", true)
	handler := server.Handler()
	request := func(path, query string, status, deleted int) {
		t.Helper()
		body, err := json.Marshal(pathBody{Path: path})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "https://files.example/api/fs/delete"+query, strings.NewReader(string(body)))
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		r.Header.Set("Origin", "https://files.example")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		var got struct {
			OK   bool `json:"ok"`
			Data struct {
				Deleted int `json:"deleted"`
			} `json:"data"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != status || got.OK != (status == http.StatusOK) || got.Data.Deleted != deleted {
			t.Fatalf("delete %q%s: HTTP=%d body=%s, want status=%d deleted=%d", path, query, w.Code, w.Body.String(), status, deleted)
		}
		if status == http.StatusConflict && !strings.Contains(got.Error, "?recursive=1") {
			t.Fatalf("conflict does not explain recursive opt-in: %q", got.Error)
		}
	}
	before := len(objects.objects)
	for _, path := range []string{"Docs", "empty"} {
		for _, query := range []string{"", "?recursive=", "?recursive=0", "?recursive=true"} {
			request(path, query, http.StatusConflict, 0)
		}
	}
	request("", "?recursive=1", http.StatusBadRequest, 0)
	request("missing", "?recursive=1", http.StatusNotFound, 0)
	request("private", "?recursive=1", http.StatusNotFound, 0)
	if objects.calls != 0 || len(objects.objects) != before {
		t.Fatal("rejected deletion reached object storage")
	}
	var indexed int
	if err = db.Pool().QueryRow(ctx, `select count(*) from nodes`).Scan(&indexed); err != nil || indexed != before {
		t.Fatal("rejected deletion changed the index", err)
	}
	request("file.txt", "", http.StatusOK, 1)
	request("Docs", "?recursive=1", http.StatusOK, 3)
	request("empty", "?recursive=1", http.StatusOK, 1)
	request("Docs", "?recursive=1", http.StatusNotFound, 0)
	for key := range objects.objects {
		path := strings.TrimPrefix(key, paths.KeyFor("", owners[0]))
		if path == "file.txt" || path == "empty/" || strings.HasPrefix(path, "Docs/") {
			t.Fatalf("deleted object still present: %q", key)
		}
	}
	for _, owner := range owners {
		for _, path := range []string{"Docs", "Docs/nested", "Docs/nested/file.txt", "empty", "file.txt", "Docs-old.txt"} {
			row, err := server.nodes.Live(ctx, path, owner)
			wantPresent := owner == owners[1] || path == "Docs-old.txt"
			if err != nil || (row != nil) != wantPresent {
				t.Fatalf("index owner=%s path=%q present=%v want=%v err=%v", owner, path, row != nil, wantPresent, err)
			}
			if wantPresent {
				key := paths.KeyFor(path, owner)
				if row.IsDir {
					key = paths.DirKeyFor(path, owner)
				}
				if _, ok := objects.objects[key]; !ok {
					t.Fatalf("unrelated object removed: %q", key)
				}
			}
		}
	}
}
