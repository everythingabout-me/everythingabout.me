package nodes

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5/pgconn"
)

type observedStorage struct {
	storage.Storage
	mu      sync.Mutex
	objects map[string]storage.ObjectInfo
	err     error
	onHead  func(string)
	removed chan struct{}
}

func (s *observedStorage) Head(_ context.Context, key string) (*storage.ObjectInfo, error) {
	s.mu.Lock()
	value, exists := s.objects[key]
	err, hook := s.err, s.onHead
	s.mu.Unlock()
	if hook != nil {
		hook(key)
	}
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return &value, nil
}
func (s *observedStorage) Remove(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	if s.removed != nil {
		select {
		case s.removed <- struct{}{}:
		default:
		}
	}
	return nil
}
func (s *observedStorage) put(info storage.ObjectInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[info.Key] = info
}

func observationTestDB(t *testing.T) (*store.DB, context.Context) {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_observation_")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
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
	return db, ctx
}

func TestUploadObservationPostgresRecovery(t *testing.T) {
	db, ctx := observationTestDB(t)
	var owner, other string
	for i, id := range []*string{&owner, &other} {
		if err := db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("observer-%d@example.test", i)).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	objects := &observedStorage{objects: map[string]storage.ObjectInfo{}}
	service := New(db, objects)
	path := "Health/apple-health/2021/07/16/steps.json"
	key := paths.KeyFor(path, owner)
	nativeTime := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	info := storage.ObjectInfo{Key: key, Size: 3, ETag: "first", ContentType: "application/json", LastModified: nativeTime}
	objects.put(info)
	if changed, err := service.ObserveUpload(ctx, owner, path, false); err != nil || !changed {
		t.Fatalf("lost confirm recovery changed=%v err=%v", changed, err)
	}
	row, err := service.Live(ctx, path, owner)
	if err != nil || row == nil {
		t.Fatal("recovery did not index file", err)
	}
	if row.Size != 3 || stringValue(row.Mime) != "application/json" {
		t.Fatalf("wrong recovered facts: %+v", row)
	}
	parent, err := service.Live(ctx, paths.ParentOf(path), owner)
	if err != nil || parent == nil || !parent.IsDir {
		t.Fatal("missing implicit native parent", err)
	}
	if otherRow, err := service.Live(ctx, path, other); err != nil || otherRow != nil {
		t.Fatal("observation leaked across owners", err)
	}
	original := time.Date(2015, 9, 26, 12, 0, 0, 0, time.UTC)
	acceptedMime := "application/example"
	_, err = service.UpsertFile(ctx, UpsertInput{OwnerID: owner, Path: path, Size: 3, Mime: &acceptedMime, ETag: &info.ETag, SourceModifiedAt: &original, Meta: map[string]any{"sha256": "accepted-checksum", "note": "keep annotation"}})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := service.Live(ctx, path, owner)
	if changed, err := service.ObserveUpload(ctx, owner, path, false); err != nil || changed {
		t.Fatalf("unchanged observation changed=%v err=%v", changed, err)
	}
	after, _ := service.Live(ctx, path, owner)
	if after.UpdatedAt != before.UpdatedAt || stringValue(after.SourceModifiedAt) != stringValue(before.SourceModifiedAt) || stringValue(after.Mime) != acceptedMime || string(after.Meta) != string(before.Meta) {
		t.Fatal("unchanged observation discarded accepted metadata or churned timestamp")
	}
	info.ETag = "replacement"
	info.LastModified = nativeTime.Add(time.Minute)
	info.ContentType = "text/plain"
	objects.put(info)
	if changed, err := service.ObserveUpload(ctx, owner, path, false); err != nil || !changed {
		t.Fatalf("replacement observation changed=%v err=%v", changed, err)
	}
	after, _ = service.Live(ctx, path, owner)
	if after.ID != before.ID || strings.Contains(string(after.Meta), "sha256") || !strings.Contains(string(after.Meta), "keep annotation") || stringValue(after.Mime) != "text/plain" {
		t.Fatal("replacement lost annotations/identity or retained stale checksum")
	}
	objects.mu.Lock()
	objects.err = errors.New("temporary storage failure")
	objects.mu.Unlock()
	if _, err := service.ObserveUpload(ctx, owner, path, false); err == nil {
		t.Fatal("storage error treated as successful observation")
	}
	objects.mu.Lock()
	objects.err = nil
	objects.mu.Unlock()
	missing := "Health/not-uploaded.json"
	if changed, err := service.ObserveUpload(ctx, owner, missing, false); err != nil || changed {
		t.Fatal("missing upload became a file", err)
	}
	if row, _ := service.Live(ctx, missing, owner); row != nil {
		t.Fatal("phantom file indexed")
	}
	for _, folder := range []bool{false, true} {
		p := "Health/zero"
		k := paths.KeyFor(p, owner)
		if folder {
			p = "Health/empty-folder"
			k = paths.DirKeyFor(p, owner)
		}
		objects.put(storage.ObjectInfo{Key: k, Size: 0, ETag: "empty", LastModified: nativeTime})
		if changed, err := service.ObserveUpload(ctx, owner, p, folder); err != nil || !changed {
			t.Fatal("zero-byte/marker recovery failed", err)
		}
		row, _ := service.Live(ctx, p, owner)
		if row == nil || row.IsDir != folder {
			t.Fatal("zero-byte file confused with folder marker")
		}
	}
	// A stale HEAD must not recreate a file after the application's deletion.
	headRead := make(chan struct{})
	releaseHead := make(chan struct{})
	objects.removed = make(chan struct{}, 1)
	var once sync.Once
	objects.mu.Lock()
	objects.onHead = func(k string) {
		if k == key {
			once.Do(func() { close(headRead); <-releaseHead })
		}
	}
	objects.mu.Unlock()
	observed := make(chan error, 1)
	go func() { _, err := service.ObserveUpload(ctx, owner, path, false); observed <- err }()
	select {
	case <-headRead:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Only the observer's captured HEAD waits; deletion's HEAD sees current data.
	objects.mu.Lock()
	objects.onHead = nil
	objects.mu.Unlock()
	deleted := make(chan error, 1)
	go func() { _, err := service.Purge(ctx, path, owner); deleted <- err }()
	select {
	case <-objects.removed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(releaseHead)
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if row, _ := service.Live(ctx, path, owner); row != nil {
		t.Fatal("stale HEAD resurrected deleted index row")
	}
	if changed, err := service.ObserveUpload(ctx, owner, path, false); err != nil || changed {
		t.Fatal("missing deleted file was recreated", err)
	}
	// A real late native PUT is new storage evidence and must be indexed again.
	objects.put(info)
	if changed, err := service.ObserveUpload(ctx, owner, path, false); err != nil || !changed {
		t.Fatal("late native object was not recovered", err)
	}
}

func TestConfirmUploadPostgresMetadataAndDeleteRace(t *testing.T) {
	db, ctx := observationTestDB(t)
	var owner string
	if err := db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('confirm-race@example.test','') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	objects := &observedStorage{objects: map[string]storage.ObjectInfo{}}
	service := New(db, objects)
	path := "Health/data.json"
	key := paths.KeyFor(path, owner)
	nativeTime := time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC)
	info := storage.ObjectInfo{Key: key, Size: 12, ETag: "first", StorageClass: "STANDARD", LastModified: nativeTime}
	objects.put(info)
	acceptedTime := time.Date(2015, 9, 26, 12, 0, 0, 0, time.UTC)
	acceptedMime := "application/json"
	row, err := service.ConfirmUpload(ctx, UpsertInput{
		OwnerID: owner, Path: path, Size: 999, Mime: &acceptedMime, SourceModifiedAt: &acceptedTime,
		Meta: map[string]any{"sha256": "first-checksum", "mime": acceptedMime, "annotation": "keep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.Size != info.Size || stringValue(row.ETag) != info.ETag || stringValue(row.StorageClass) != info.StorageClass || stringValue(row.SourceModifiedAt) != acceptedTime.Format("2006-01-02T15:04:05.000Z") {
		t.Fatalf("confirmation did not combine native facts and accepted metadata: %+v", row)
	}
	if parent, _ := service.Live(ctx, "Health", owner); parent == nil || !parent.IsDir {
		t.Fatal("confirmation did not atomically create its parent")
	}
	info.ETag = "replacement"
	info.LastModified = nativeTime.Add(time.Minute)
	objects.put(info)
	row, err = service.ConfirmUpload(ctx, UpsertInput{OwnerID: owner, Path: path, Mime: &acceptedMime})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(row.Meta), "sha256") || !strings.Contains(string(row.Meta), "keep") || stringValue(row.SourceModifiedAt) != info.LastModified.Format("2006-01-02T15:04:05.000Z") {
		t.Fatal("replacement retained a stale checksum, lost annotations or missed native timestamp fallback")
	}
	if _, err := service.ConfirmUpload(ctx, UpsertInput{OwnerID: owner, Path: "Missing/file.json"}); err == nil {
		t.Fatal("confirmation created a file without a complete native object")
	}
	if parent, _ := service.Live(ctx, "Missing", owner); parent != nil {
		t.Fatal("failed confirmation leaked parent writes")
	}
	if _, err := service.ConfirmUpload(ctx, UpsertInput{OwnerID: owner, Path: "Health"}); err == nil {
		t.Fatal("confirmation overwrote an indexed directory")
	}

	// Capture HEAD, then remove the native object. The SQL deletion must wait
	// for confirmation's lock, or invalidate its serializable snapshot.
	headRead := make(chan struct{})
	releaseHead := make(chan struct{})
	objects.removed = make(chan struct{}, 1)
	objects.mu.Lock()
	objects.onHead = func(k string) {
		if k == key {
			close(headRead)
			<-releaseHead
		}
	}
	objects.mu.Unlock()
	confirmed := make(chan error, 1)
	go func() {
		_, err := service.ConfirmUpload(ctx, UpsertInput{OwnerID: owner, Path: path, Mime: &acceptedMime})
		confirmed <- err
	}()
	select {
	case <-headRead:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	objects.mu.Lock()
	objects.onHead = nil
	objects.mu.Unlock()
	deleted := make(chan error, 1)
	go func() { _, err := service.Purge(ctx, path, owner); deleted <- err }()
	select {
	case <-objects.removed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	deletionFinished := false
	var deletionError error
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
waitForDelete:
	for {
		select {
		case deletionError = <-deleted:
			deletionFinished = true
			break waitForDelete
		case <-ticker.C:
			var blocked bool
			if err := db.Pool().QueryRow(ctx, `select exists(select 1 from pg_stat_activity
				where datname=current_database() and query like 'delete from nodes%' and wait_event_type='Lock')`).Scan(&blocked); err != nil {
				t.Fatal(err)
			}
			if blocked {
				break waitForDelete
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(releaseHead)
	if err := <-confirmed; err != nil {
		var postgresError *pgconn.PgError
		if !errors.As(err, &postgresError) || postgresError.Code != "40001" {
			t.Fatal(err)
		}
	}
	if !deletionFinished {
		deletionError = <-deleted
	}
	if deletionError != nil {
		t.Fatal(deletionError)
	}
	if row, _ := service.Live(ctx, path, owner); row != nil {
		t.Fatal("stale confirmation resurrected a deleted index row")
	}
}

func TestUnchangedUploadPostgresRepairsMissingParents(t *testing.T) {
	db, ctx := observationTestDB(t)
	var owner string
	if err := db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('orphan-recovery@example.test','') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	objects := &observedStorage{objects: map[string]storage.ObjectInfo{}}
	service := New(db, objects)
	path := "Health/imported/day.json"
	info := storage.ObjectInfo{Key: paths.KeyFor(path, owner), Size: 3, ETag: "same"}
	objects.put(info)
	if _, err := service.ObserveUpload(ctx, owner, path, false); err != nil {
		t.Fatal(err)
	}
	before, _ := service.Live(ctx, path, owner)
	if _, err := db.Pool().Exec(ctx, `delete from nodes where owner_id=$1::uuid and is_dir`, owner); err != nil {
		t.Fatal(err)
	}
	if changed, err := service.ObserveUpload(ctx, owner, path, false); err != nil || !changed {
		t.Fatalf("orphan parent recovery changed=%v err=%v", changed, err)
	}
	after, _ := service.Live(ctx, path, owner)
	if after.ID != before.ID || after.UpdatedAt != before.UpdatedAt {
		t.Fatal("repairing parents churned unchanged file metadata")
	}
	id, parentID, err := service.FileIdentity(ctx, owner, path)
	if err != nil || id != before.ID || parentID == "" {
		t.Fatalf("repaired canonical identity %q/%q error=%v", id, parentID, err)
	}
	for _, parentPath := range paths.AncestorsOf(path) {
		if row, _ := service.Live(ctx, parentPath, owner); row == nil || !row.IsDir {
			t.Fatalf("missing repaired parent %s", parentPath)
		}
	}
}
