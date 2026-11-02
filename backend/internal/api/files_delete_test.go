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
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"github.com/jackc/pgx/v5"
)

// Unimplemented provider methods panic if a rejected request reaches storage.
type deleteStorage struct {
	storage.Storage
	objects   map[string]storage.ObjectInfo
	calls     int
	removes   int
	headErr   error
	removeErr error
}

func (s *deleteStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	s.calls++
	if s.headErr != nil {
		return nil, s.headErr
	}
	info, ok := s.objects[key]
	if !ok {
		return nil, nil
	}
	return &info, nil
}

func (s *deleteStorage) Remove(_ context.Context, key string) error {
	s.calls++
	s.removes++
	if s.removeErr != nil {
		return s.removeErr
	}
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
				Job jobs.Job `json:"job"`
			} `json:"data"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != status || got.OK != (status == http.StatusAccepted) || got.Data.Job.Total != deleted {
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
	request("file.txt", "", http.StatusAccepted, 1)
	request("Docs", "?recursive=1", http.StatusAccepted, 3)
	request("empty", "?recursive=1", http.StatusAccepted, 1)
	request("Docs", "?recursive=1", http.StatusNotFound, 0)
	if objects.calls != 0 || len(objects.objects) != before {
		t.Fatal("acceptance waited for native storage")
	}
	if row, e := server.nodes.Live(ctx, "Docs/nested/file.txt", owners[0]); e != nil || row != nil {
		t.Fatal("accepted subtree is still visible", e)
	}
	if e := server.checkBrowserUploadConflict(ctx, owners[0], "Docs/new.txt", true); e == nil {
		t.Fatal("pending deletion did not reserve subtree")
	}
	for _, tc := range []struct{ owner, path string }{{owners[0], "Docs-old.txt"}, {owners[1], "Docs/new.txt"}} {
		if e := server.checkBrowserUploadConflict(ctx, tc.owner, tc.path, true); e != nil {
			t.Fatal("reservation escaped owner or segment boundary", e)
		}
	}
	pendingJobs, e := server.jobs.PendingDeletes(ctx, owners[0])
	if e != nil || len(pendingJobs) != 3 {
		t.Fatal("pending deletion listing", pendingJobs, e)
	}
	otherCookie, _, e := server.auth.CreateSession(ctx, owners[1], "test", "")
	if e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		cookie string
		status int
	}{{cookie, 200}, {otherCookie, 404}} {
		r := httptest.NewRequest(http.MethodGet, "https://files.example/api/fs/delete/"+pendingJobs[0].ID, nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: tc.cookie})
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("deletion status HTTP=%d, want %d: %s", w.Code, tc.status, w.Body.String())
		}
	}
	// Retained presigned grants cannot be declared quiescent merely because
	// cancellation, completion or TTL expiry occurred in the database.
	if _, e := db.Pool().Exec(ctx, `insert into upload_sessions(id,user_id,path,size,mime,part_size,expires_at,completed_at,cancelled_at)
	 values('u_delete_guard',$1::uuid,'grants/file.txt',3,'text/plain',3,now()-interval '1 hour',now(),now())`, owners[0]); e != nil {
		t.Fatal(e)
	}
	if e := server.checkDeleteLegacyUploads(ctx, owners[0], "grants"); e == nil {
		t.Fatal("expired legacy grant allowed deletion")
	}
	if e := server.checkDeleteLegacyUploads(ctx, owners[0], "grants-old"); e != nil {
		t.Fatal("grant guard crossed path boundary", e)
	}
	if e := server.checkDeleteLegacyUploads(ctx, owners[1], "grants"); e != nil {
		t.Fatal("grant guard crossed owner", e)
	}
	if _, e := db.Pool().Exec(ctx, `insert into upload_index_watches(binding,owner_id,path,issued_until) values($1,$2::uuid,'watched/file.txt',now()-interval '1 hour')`, uploadIndexBinding(server.cfg), owners[0]); e != nil {
		t.Fatal(e)
	}
	if e := server.checkDeleteLegacyUploads(ctx, owners[0], "watched"); e == nil {
		t.Fatal("unresolved legacy watch allowed deletion")
	}
	// A new service simulates restart before any native effect. Its first pass
	// resumes the same captured keys instead of rescanning a changed namespace.
	server.jobs = jobs.New(db, objects, server.nodes)
	server.jobs.SetDeleteBinding(uploadIndexBinding(server.cfg))
	for i := 0; i < 2; i++ {
		if e := server.jobs.RunDeletesOnce(ctx); e != nil {
			t.Fatal(e)
		}
	}
	if pending, e := server.jobs.PendingDeletes(ctx, owners[0]); e != nil || len(pending) != 0 {
		t.Fatal("cleanup did not finish", pending, e)
	}
	if e := server.checkBrowserUploadConflict(ctx, owners[0], "Docs/new.txt", true); e != nil {
		t.Fatal("finished deletion still reserves subtree", e)
	}
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

	// Safe read errors retry without issuing a delete; the durable manifest is
	// unchanged by rerunning every migration while cleanup is pending.
	seed(owners[0], "retry.txt", false)
	request("retry.txt", "", http.StatusAccepted, 1)
	objects.headErr = errors.New("temporary read failure")
	removes := objects.removes
	if e := server.jobs.RunDeletesOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if objects.removes != removes {
		t.Fatal("failed read issued native delete")
	}
	for _, migration := range migrations {
		data, e := os.ReadFile(migration)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = db.Pool().Exec(ctx, string(data)); e != nil {
			t.Fatal(e)
		}
	}
	objects.headErr = nil
	if _, e := db.Pool().Exec(ctx, `update fs_jobs set next_attempt_at=now() where op='delete'`); e != nil {
		t.Fatal(e)
	}
	if e := server.jobs.RunDeletesOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if objects.removes != removes+1 {
		t.Fatal("safe retry did not remove captured object")
	}
	// A lost native reply must never be blindly retried, including after restart.
	seed(owners[0], "unknown.txt", false)
	request("unknown.txt", "", http.StatusAccepted, 1)
	objects.removeErr = errors.New("lost native reply")
	if e := server.jobs.RunDeletesOnce(ctx); e != nil {
		t.Fatal(e)
	}
	removes = objects.removes
	objects.removeErr = nil
	restarted := jobs.New(db, objects, server.nodes)
	restarted.SetDeleteBinding(uploadIndexBinding(server.cfg))
	if e := restarted.RunDeletesOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if objects.removes != removes {
		t.Fatal("unknown native removal replayed after restart")
	}
	pending, e := restarted.PendingDeletes(ctx, owners[0])
	if e != nil || len(pending) != 1 || pending[0].Status != "failed" {
		t.Fatal("unknown delete must remain visible as failed", pending, e)
	}
	if e := server.checkBrowserUploadConflict(ctx, owners[0], "unknown.txt", true); e == nil {
		t.Fatal("unknown deletion released path reservation")
	}
	// Crash after intent checkpoint is equivalent to a lost reply, even when no
	// native request was observed locally. Never infer safe replay from HEAD.
	seed(owners[0], "crash.txt", false)
	request("crash.txt", "", http.StatusAccepted, 1)
	if _, e := db.Pool().Exec(ctx, `update fs_delete_entries set state='removing' where path='crash.txt' and owner_id=$1::uuid`, owners[0]); e != nil {
		t.Fatal(e)
	}
	if e := restarted.RunDeletesOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if objects.removes != removes {
		t.Fatal("checkpointed native removal replayed")
	}
	// Old storage generations cannot execute on another bucket/root.
	seed(owners[0], "binding.txt", false)
	request("binding.txt", "", http.StatusAccepted, 1)
	restarted.SetDeleteBinding("other-root")
	if e := restarted.RunDeletesOnce(ctx); e != nil {
		t.Fatal(e)
	}
	if objects.removes != removes {
		t.Fatal("old deletion executed against a different root")
	}
}
