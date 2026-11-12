package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"everythingabout.me/backend/internal/browserupload"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"everythingabout.me/backend/internal/uploadindex"
	"github.com/jackc/pgx/v5"
)

// recoveryStorage models unpublished reception separately from final native
// objects. It deliberately cannot turn an interrupted Stage into a final file.
type recoveryStorage struct {
	storage.Storage
	mu                                                                               sync.Mutex
	objects                                                                          map[string]storage.ObjectInfo
	markers                                                                          map[string]string
	stageCalls, publishCalls, inspectCalls, discardCalls, headCalls, credentialCalls int
	afterFolder                                                                      func()
}

func (s *recoveryStorage) TemporaryCredentials(context.Context, string, int) (*storage.Credentials, error) {
	s.mu.Lock()
	s.credentialCalls++
	s.mu.Unlock()
	return nil, errors.New("browser must not receive provider credentials")
}
func (s *recoveryStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.headCalls++
	info, ok := s.objects[key]
	if !ok {
		return nil, nil
	}
	return &info, nil
}
func (s *recoveryStorage) PutEmpty(_ context.Context, key string) error {
	s.mu.Lock()
	s.objects[key] = storage.ObjectInfo{Key: key, ETag: "folder-marker"}
	hook := s.afterFolder
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}
func (s *recoveryStorage) Stage(ctx context.Context, session browserupload.Session, body io.Reader, save func(context.Context, json.RawMessage) error) (json.RawMessage, error) {
	s.mu.Lock()
	s.stageCalls++
	s.mu.Unlock()
	stage, _ := json.Marshal(map[string]string{"marker": session.ID})
	if err := save(ctx, stage); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(body, session.Size+1))
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != session.Size {
		return nil, io.ErrUnexpectedEOF
	}
	return stage, nil
}
func (s *recoveryStorage) Publish(ctx context.Context, session browserupload.Session, stage json.RawMessage) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var checkpoint struct {
		Marker string `json:"marker"`
	}
	if err := json.Unmarshal(stage, &checkpoint); err != nil {
		return false, err
	}
	if checkpoint.Marker != session.ID {
		return false, errors.New("staging marker differs from the intended upload")
	}
	key := paths.KeyFor(session.Path, session.OwnerID)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishCalls++
	s.objects[key] = storage.ObjectInfo{Key: key, Size: session.Size, ETag: "uploaded-" + session.ID, ContentType: session.Mime, LastModified: time.Now().UTC().Truncate(time.Second)}
	s.markers[key] = session.ID
	return true, nil
}
func (s *recoveryStorage) Inspect(_ context.Context, session browserupload.Session) (browserupload.Inspection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inspectCalls++
	return browserupload.Inspection{Applied: s.markers[paths.KeyFor(session.Path, session.OwnerID)] == session.ID, Terminal: false}, nil
}
func (s *recoveryStorage) Discard(context.Context, browserupload.Session) error {
	s.mu.Lock()
	s.discardCalls++
	s.mu.Unlock()
	return nil
}
func (s *recoveryStorage) BrowserUploadMarker(_ context.Context, owner, path, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markers[paths.KeyFor(path, owner)] != id {
		return errors.New("native upload marker mismatch")
	}
	return nil
}
func (s *recoveryStorage) counters() (heads, publishes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headCalls, s.publishCalls
}

type interruptedUploadReader struct{ cancel context.CancelFunc }

func (r *interruptedUploadReader) Read(out []byte) (int, error) {
	r.cancel()
	if len(out) > 0 {
		out[0] = 'x'
		return 1, io.ErrUnexpectedEOF
	}
	return 0, io.ErrUnexpectedEOF
}

func TestBrowserUploadIndexPostgresRecovery(t *testing.T) {
	address := testpg.Require(t)
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
	for range 2 {
		for _, migration := range migrations {
			data, err := os.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
				t.Fatalf("migration %s: %v", filepath.Base(migration), err)
			}
		}
	}
	cfg := &config.Config{AppSecret: "independent-recovery-test", SessionTTLDays: 1, COS: config.COS{Bucket: "synthetic-123456789", Region: "test-region"}}
	native := &recoveryStorage{objects: map[string]storage.ObjectInfo{}, markers: map[string]string{}}
	server := New(cfg, db, native, nil)
	var owner string
	if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('browser-recovery@example.test','') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	cookie, _, err := server.auth.CreateSession(ctx, owner, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = server.nodes.EnsureParents(ctx, "Health/apple-health/steps.json", owner); err != nil {
		t.Fatal(err)
	}
	request := func(endpoint string, body io.Reader) *http.Request {
		r := httptest.NewRequest("POST", "http://files.example"+endpoint, body)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cookie})
		return r
	}
	jsonRequest := func(endpoint, body string) *http.Request { return request(endpoint, strings.NewReader(body)) }
	state := func(id string) string {
		t.Helper()
		var current string
		if err := db.Pool().QueryRow(ctx, `select state from browser_uploads where owner_id=$1::uuid and id=$2`, owner, id).Scan(&current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	countWatches := func(path string) int {
		t.Helper()
		var count int
		if err := db.Pool().QueryRow(ctx, `select count(*) from upload_index_watches where owner_id=$1::uuid and path=$2`, owner, path).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	sign := func(path string, size int) string {
		t.Helper()
		w := httptest.NewRecorder()
		body := fmt.Sprintf(`{"path":%q,"size":%d,"mime":"application/json","modifiedAt":1443268800000}`, path, size)
		if err := server.handleUploadSign(w, jsonRequest("/api/fs/upload/sign", body)); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			OK   bool                       `json:"ok"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if !reply.OK || len(reply.Data) != 1 {
			t.Fatalf("unexpected sign response: %s", w.Body.String())
		}
		var id string
		if err := json.Unmarshal(reply.Data["uploadId"], &id); err != nil || id == "" {
			t.Fatal("missing application upload ID", err)
		}
		if state(id) != "open" {
			t.Fatal("sign did not persist an unstarted upload")
		}
		if countWatches(path) != 0 {
			t.Fatal("new upload registered a permanent per-path watch")
		}
		if row, _ := server.nodes.Live(ctx, path, owner); row != nil {
			t.Fatal("sign created a phantom file")
		}
		native.mu.Lock()
		credentials := native.credentialCalls
		native.mu.Unlock()
		if credentials != 0 {
			t.Fatal("new upload minted provider credentials")
		}
		return id
	}
	binary := func(id, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		if err := server.handleUploadConfirm(w, request("/api/fs/upload/confirm?uploadId="+url.QueryEscape(id), strings.NewReader(body))); err != nil {
			t.Fatal(err)
		}
		return w
	}
	assertStatus := func(err error, status int) {
		t.Helper()
		var typed *httpx.Error
		if !errors.As(err, &typed) || typed.Status != status {
			t.Fatalf("wanted %d, got %v", status, err)
		}
	}

	t.Run("immediate publish and receipt replay", func(t *testing.T) {
		path := "Health/apple-health/steps.json"
		id := sign(path, 3)
		w := binary(id, "abc")
		var reply struct {
			OK   bool `json:"ok"`
			Data struct {
				Node struct {
					Path string `json:"path"`
					Size int64  `json:"size"`
				} `json:"node"`
			} `json:"data"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &reply); err != nil || !reply.OK || reply.Data.Node.Path != path || reply.Data.Node.Size != 3 {
			t.Fatal("binary confirm did not return indexed node", err)
		}
		if state(id) != "completed" {
			t.Fatal("successful upload remained in recovery")
		}
		row, err := server.nodes.Live(ctx, path, owner)
		if err != nil || row == nil || row.SourceModifiedAt == nil || !strings.HasPrefix(*row.SourceModifiedAt, "2015-") {
			t.Fatal("original client timestamp was not indexed", err)
		}
		heads, publishes := native.counters()
		replay := httptest.NewRecorder()
		if err = server.handleUploadConfirm(replay, jsonRequest("/api/fs/upload/confirm", `{"uploadId":"`+id+`"}`)); err != nil {
			t.Fatal(err)
		}
		if err = server.browserUploads.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if h, p := native.counters(); h != heads || p != publishes {
			t.Fatal("completed upload was republished or polled")
		}
	})
	t.Run("empty file", func(t *testing.T) {
		path := "Health/zero.json"
		id := sign(path, 0)
		binary(id, "")
		row, err := server.nodes.Live(ctx, path, owner)
		if err != nil || row == nil || row.IsDir || row.Size != 0 || state(id) != "completed" {
			t.Fatal("zero-byte file failed or became a directory", err)
		}
	})
	t.Run("interrupted body", func(t *testing.T) {
		path := "Health/interrupted.json"
		id := sign(path, 3)
		requestCtx, stop := context.WithCancel(ctx)
		defer stop()
		r := request("/api/fs/upload/confirm?uploadId="+id, &interruptedUploadReader{cancel: stop}).WithContext(requestCtx)
		r.ContentLength = 3
		err := server.handleUploadConfirm(httptest.NewRecorder(), r)
		assertStatus(err, 410)
		if state(id) != "aborted" {
			t.Fatal("interrupted receive retained an active intent")
		}
		if row, _ := server.nodes.Live(ctx, path, owner); row != nil {
			t.Fatal("interrupted body became a file")
		}
		native.mu.Lock()
		_, exists := native.objects[paths.KeyFor(path, owner)]
		native.mu.Unlock()
		if exists {
			t.Fatal("interrupted body published native bytes")
		}
		assertStatus(server.handleUploadConfirm(httptest.NewRecorder(), jsonRequest("/api/fs/upload/confirm", `{"uploadId":"`+id+`"}`)), 410)
	})
	t.Run("native success then index failure and restart", func(t *testing.T) {
		path := "Health/fail-index.json"
		id := sign(path, 3)
		if _, err = db.Pool().Exec(ctx, `alter table nodes add constraint reject_test_upload check(path <> 'Health/fail-index.json')`); err != nil {
			t.Fatal(err)
		}
		_, beforePublish := native.counters()
		err = server.handleUploadConfirm(httptest.NewRecorder(), request("/api/fs/upload/confirm?uploadId="+id, strings.NewReader("abc")))
		if err == nil {
			t.Fatal("forced index failure hidden")
		}
		if state(id) != "applied" {
			t.Fatal("native success not recorded before failing index write")
		}
		native.mu.Lock()
		_, exists := native.objects[paths.KeyFor(path, owner)]
		native.mu.Unlock()
		if !exists {
			t.Fatal("index failure deleted the native file")
		}
		if row, _ := server.nodes.Live(ctx, path, owner); row != nil {
			t.Fatal("failed transaction left partial index")
		}
		if _, err = db.Pool().Exec(ctx, `alter table nodes drop constraint reject_test_upload`); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, `update browser_uploads set next_check_at=clock_timestamp()-interval '1 second' where id=$1`, id); err != nil {
			t.Fatal(err)
		}
		restarted := New(cfg, db, native, nil)
		if err = restarted.browserUploads.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if state(id) != "completed" {
			t.Fatal("restart did not settle known native success")
		}
		if row, _ := restarted.nodes.Live(ctx, path, owner); row == nil {
			t.Fatal("restart did not repair the missing index row")
		}
		_, afterPublish := native.counters()
		if afterPublish != beforePublish+1 {
			t.Fatal("restart repeated native publication")
		}
		heads, publishes := native.counters()
		if err = restarted.browserUploads.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if h, p := native.counters(); h != heads || p != publishes {
			t.Fatal("recovered success continued polling native storage")
		}
	})
	t.Run("canceled directory request recovers and retires", func(t *testing.T) {
		path := "Health/empty"
		r := jsonRequest("/api/fs/mkdir", `{"path":"`+path+`"}`)
		requestCtx, stopRequest := context.WithCancel(ctx)
		defer stopRequest()
		native.mu.Lock()
		native.afterFolder = stopRequest
		native.mu.Unlock()
		if err = server.handleMkdir(httptest.NewRecorder(), r.WithContext(requestCtx)); err == nil {
			t.Fatal("canceled directory request unexpectedly finished")
		}
		native.mu.Lock()
		native.afterFolder = nil
		native.mu.Unlock()
		if countWatches(path) != 1 {
			t.Fatal("directory marker missing recovery intent")
		}
		restarted := New(cfg, db, native, nil)
		worker := restarted.uploadIndex.(*uploadindex.Service)
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if row, _ := restarted.nodes.Live(ctx, path, owner); row == nil || !row.IsDir {
			t.Fatal("directory marker not recovered")
		}
		if countWatches(path) != 0 {
			t.Fatal("recovered directory retained a permanent watch")
		}
		heads, _ := native.counters()
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if h, _ := native.counters(); h != heads {
			t.Fatal("retired directory still polled")
		}
	})
	t.Run("active move and copy reserve both path subtrees", func(t *testing.T) {
		var otherOwner string
		if err := db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('other-job-owner@example.test','') returning id::text`).Scan(&otherOwner); err != nil {
			t.Fatal(err)
		}
		jobID := "browser-upload-guard-job"
		defer func() {
			if _, err := db.Pool().Exec(ctx, `delete from fs_jobs where id=$1`, jobID); err != nil {
				t.Error(err)
			}
		}()
		source, destination := "Health/job/source%_", "Health/job/destination"
		if err := server.nodes.EnsureParents(ctx, source+"/nested/file.json", owner); err != nil {
			t.Fatal(err)
		}
		if err := server.nodes.EnsureParents(ctx, destination+"/nested/file.json", owner); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Pool().Exec(ctx, `insert into fs_jobs(id,user_id,op,from_path,to_path,status)
			values($1,$2::uuid,'move',$3,$4,'pending')`, jobID, owner, source, destination); err != nil {
			t.Fatal(err)
		}
		attempt := func(path string) error {
			body, err := json.Marshal(map[string]any{"path": path, "size": 3})
			if err != nil {
				t.Fatal(err)
			}
			return server.handleUploadSign(httptest.NewRecorder(), jsonRequest("/api/fs/upload/sign", string(body)))
		}
		for _, status := range []string{"pending", "running"} {
			if _, err := db.Pool().Exec(ctx, `update fs_jobs set status=$2,op=case when $2='running' then 'copy' else 'move' end where id=$1`, jobID, status); err != nil {
				t.Fatal(err)
			}
			for _, target := range []string{source, destination, source + "/nested/file.json", destination + "/nested/file.json", "Health/job", "Health"} {
				assertStatus(attempt(target), 409)
				assertStatus(server.checkBrowserUploadConflict(ctx, owner, target, false), 409)
			}
		}
		// A similar prefix containing wildcard-like filename characters must not
		// overlap, and a different owner's active job must not leak its namespace.
		for _, unrelated := range []string{"Health/job/sourceAB/file.json", "Health/job/source%_suffix/file.json", "Health/job/destination-sibling/file.json"} {
			if err := server.checkBrowserUploadConflict(ctx, owner, unrelated, true); err != nil {
				t.Fatalf("unrelated path blocked: %s: %v", unrelated, err)
			}
		}
		if err := server.checkBrowserUploadConflict(ctx, otherOwner, source+"/file.json", true); err != nil {
			t.Fatal("another owner's job blocked this owner", err)
		}
		unrelatedID := sign("Health/job-unrelated.json", 3)
		binary(unrelatedID, "abc")
		for _, status := range []string{"done", "failed", "cancelled"} {
			if _, err := db.Pool().Exec(ctx, `update fs_jobs set status=$2 where id=$1`, jobID, status); err != nil {
				t.Fatal(err)
			}
			if err := server.checkBrowserUploadConflict(ctx, owner, source+"/nested/file.json", true); err != nil {
				t.Fatalf("terminal %s job retained guard: %v", status, err)
			}
		}
		finishedID := sign(destination+"/nested/file.json", 3)
		binary(finishedID, "abc")
	})
	t.Run("journal failure refuses upload", func(t *testing.T) {
		if _, err = db.Pool().Exec(ctx, `drop table browser_uploads`); err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		if err = server.handleUploadSign(w, jsonRequest("/api/fs/upload/sign", `{"path":"Health/blocked.json","size":0}`)); err == nil {
			t.Fatal("sign succeeded without durable upload registration")
		}
		if w.Body.Len() != 0 {
			t.Fatal("upload ID or credentials exposed despite failed registration")
		}
	})
}
