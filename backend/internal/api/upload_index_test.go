package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/uploadindex"
	"github.com/jackc/pgx/v5"
)

type recoveryStorage struct {
	storage.Storage
	mu          sync.Mutex
	objects     map[string]storage.ObjectInfo
	afterFolder func()
}

func (s *recoveryStorage) TemporaryCredentials(context.Context, string, int) (*storage.Credentials, error) {
	return &storage.Credentials{TmpSecretID: "test-only", TmpSecretKey: "must-not-leak-on-error", ExpiredTime: time.Now().Add(time.Hour).Unix()}, nil
}
func (s *recoveryStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.objects[key]
	if !ok {
		return nil, nil
	}
	return &info, nil
}
func (s *recoveryStorage) PutEmpty(_ context.Context, key string) error {
	s.mu.Lock()
	s.objects[key] = storage.ObjectInfo{Key: key, ETag: "folder-marker"}
	s.mu.Unlock()
	if s.afterFolder != nil {
		s.afterFolder()
	}
	return nil
}

func TestBrowserUploadIndexPostgresRecovery(t *testing.T) {
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
	name := fmt.Sprintf("eam_browser_recovery_%d", time.Now().UnixNano())
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
	cfg := &config.Config{AppSecret: "independent-recovery-test", SessionTTLDays: 1, COS: config.COS{Bucket: "synthetic-123456789", Region: "test-region"}}
	objects := &recoveryStorage{objects: map[string]storage.ObjectInfo{}}
	server := New(cfg, db, objects, nil)
	var owner string
	if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('browser-recovery@example.test','') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	cookie, _, err := server.auth.CreateSession(ctx, owner, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	request := func(endpoint, body string) *http.Request {
		r := httptest.NewRequest("POST", "http://files.example"+endpoint, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		return r
	}
	path := "Health/apple-health/steps.json"
	w := httptest.NewRecorder()
	if err = server.handleUploadSign(w, request("/api/fs/upload/sign", `{"path":"`+path+`","size":3}`)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.Pool().QueryRow(ctx, `select count(*) from upload_index_watches where owner_id=$1::uuid and path=$2`, owner, path).Scan(&count); err != nil || count != 1 {
		t.Fatal("credentials returned without durable watch", err)
	}
	if row, _ := server.nodes.Live(ctx, path, owner); row != nil {
		t.Fatal("sign created a phantom file")
	}
	// The browser disappears after COS success and never sends confirm. A new
	// server instance must discover the durable watch and repair its index.
	key := paths.KeyFor(path, owner)
	objects.objects[key] = storage.ObjectInfo{Key: key, Size: 3, ETag: "uploaded", ContentType: "application/json", LastModified: time.Now().UTC().Truncate(time.Second)}
	restarted := New(cfg, db, objects, nil)
	worker := restarted.uploadIndex.(*uploadindex.Service)
	if err = worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ := restarted.nodes.Live(ctx, path, owner); row == nil || row.Size != 3 {
		t.Fatal("restart did not recover upload without browser confirmation")
	}
	// A canceled directory request can leave its marker. Independent recovery
	// must not inherit that request's cancellation or confuse it with a file.
	r := request("/api/fs/mkdir", `{"path":"Health/empty"}`)
	requestCtx, stopRequest := context.WithCancel(ctx)
	objects.afterFolder = stopRequest
	if err = restarted.handleMkdir(httptest.NewRecorder(), r.WithContext(requestCtx)); err == nil {
		t.Fatal("canceled directory request unexpectedly completed")
	}
	objects.afterFolder = nil
	if err = worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if row, _ := restarted.nodes.Live(ctx, "Health/empty", owner); row == nil || !row.IsDir {
		t.Fatal("directory marker not recovered after request cancellation")
	}
	// Normal confirmation keeps the watcher, lowers its frequency, and records
	// the original client timestamp while the unchanged observer preserves it.
	if err = restarted.handleUploadConfirm(httptest.NewRecorder(), request("/api/fs/upload/confirm", `{"path":"`+path+`","mime":"application/json","modifiedAt":1443268800000}`)); err != nil {
		t.Fatal(err)
	}
	var settled bool
	if err = db.Pool().QueryRow(ctx, `select settled from upload_index_watches where owner_id=$1::uuid and path=$2`, owner, path).Scan(&settled); err != nil || !settled {
		t.Fatal("successful confirm discarded or failed to settle watch", err)
	}
	// A failed watch write must never expose the already minted credentials.
	if _, err = db.Pool().Exec(ctx, `drop table upload_index_watches`); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	if err = restarted.handleUploadSign(w, request("/api/fs/upload/sign", `{"path":"Health/blocked.json","size":0}`)); err == nil {
		t.Fatal("sign succeeded with no durable journal")
	}
	if w.Body.Len() != 0 {
		t.Fatal("credentials exposed despite failed durable registration")
	}
}
