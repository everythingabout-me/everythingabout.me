package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storagebackend"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

// This test pins the audit write amplification of an import so a future change that
// silently adds a row per request or per entry fails here instead of surfacing as an
// unexplained table growth months later.
//
// The measured rule, and where each row is written:
//
//   - every authenticated file request: one "admitted" row before the handler runs and
//     one "completed"/"failed"/"denied" row after it (files_boundary.go, via
//     recordFilesAudit, each in its own transaction);
//   - a settled mutation: one "confirmed" row per change fact inside the settlement
//     transaction (nodes.settleFileChanges -> filechanges.RecordAudit);
//   - a sync that removes native-absent entries: one "confirmed"/native_absent row per
//     removed entry inside the import transaction (nodes.removeAbsentEntries). A sync
//     that only ADDS entries writes change facts but no per-entry audit row.
//
// So a one-request-per-file import costs 3 audit rows per file, and a single sync
// request that imports N files costs 2 audit rows total plus N change facts. Those
// numbers are the measurement; the assertions below are what keep them true.
func TestPostgresFileAuditAmplificationPerImport(t *testing.T) {
	const files = 8
	scratch := testpg.CreateDatabase(t, "eam_audit_amplification_")
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) < 20 {
		t.Fatalf("migrations unavailable: %d %v", len(migrations), err)
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
	cfg := &config.Config{AppSecret: "audit-amplification-secret", SessionTTLDays: 1, FileCookieOrigins: []string{"https://files.example"}, Storage: config.Storage{Backend: "local", ConnectionID: "audit-amplification", Generation: 1, CoordinatedWrites: true, MaxSinglePutBytes: 1 << 20, Local: config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir()}}}
	backend, err := storagebackend.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	var owner string
	if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values('audit-amplification@example.test','') returning id::text`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	contentRoot := filepath.Join(cfg.Storage.Local.ContentRoot, owner)
	if err = os.Mkdir(contentRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(cfg.Storage.Local.SystemRoot, owner), 0700); err != nil {
		t.Fatal(err)
	}
	driver, err := backend.Open(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	native, ok := driver.(fileops.NativeActions)
	if !ok {
		driver.Close()
		t.Fatal("local driver does not implement native actions")
	}
	binding := native.StorageBinding()
	root := fileops.Root{OwnerID: binding.OwnerID, ConnectionID: binding.ConnectionID, Generation: binding.Generation, ContentRoot: binding.ContentRoot, SystemRoot: binding.SystemRoot, CaseSensitive: native.StorageNameRules().CaseSensitive}
	driver.Close()
	if err = fileops.NewPostgres(db.Pool()).Initialize(ctx, root, fileops.Baseline{LogicalLimit: 1 << 30, PhysicalLimit: 2 << 30, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"test":"empty owner roots"}`)}); err != nil {
		t.Fatal(err)
	}
	service := auth.New(db, cfg)
	token, _, err := service.CreateToken(ctx, auth.NewTokenInput{UserID: owner, Name: "import", Kind: auth.KindApp, CanRead: true, CanWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	server := New(cfg, db, backend.Legacy(), nil)
	server.SetStorageFactory(backend)
	handler := server.Handler()
	request := func(method, target string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://files.example"+target, bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	auditCounts := func() map[string]int64 {
		t.Helper()
		rows, err := db.Pool().Query(ctx, `select action,outcome,count(*) from file_audit where owner_id=$1::uuid group by 1,2`, owner)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		counts := map[string]int64{}
		for rows.Next() {
			var action, outcome string
			var n int64
			if err := rows.Scan(&action, &outcome, &n); err != nil {
				t.Fatal(err)
			}
			counts[action+"/"+outcome] = n
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return counts
	}
	changeCount := func() int64 {
		t.Helper()
		var n int64
		if err := db.Pool().QueryRow(ctx, `select count(*) from file_changes where owner_id=$1::uuid`, owner).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// One request per file: admission + completion + one confirmed settlement row.
	for i := 0; i < files; i++ {
		path := fmt.Sprintf("/import%03d.txt", i)
		if w := request(http.MethodPut, "/api/files/content?path="+path, []byte("payload")); w.Code != 201 {
			t.Fatalf("PUT %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	got := auditCounts()
	want := map[string]int64{"put/admitted": files, "put/completed": files, "file.created/confirmed": files}
	if len(got) != len(want) {
		t.Fatalf("audit rows after a %d-file PUT import = %v, want %v", files, got, want)
	}
	for key, n := range want {
		if got[key] != n {
			t.Fatalf("audit %s=%d after a %d-file import, want %d (per-file amplification changed)", key, got[key], files, n)
		}
	}
	if n := changeCount(); n != files {
		t.Fatalf("change facts after a %d-file PUT import=%d, want %d", files, n, files)
	}

	// One sync request over native files the index has not seen: two request rows and
	// one change fact per entry, no per-entry audit row.
	for i := 0; i < files; i++ {
		name := fmt.Sprintf("scanned%03d.txt", i)
		if err := os.WriteFile(filepath.Join(contentRoot, name), []byte("payload"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if w := request(http.MethodPost, "/api/files/sync", []byte(`{"dryRun":false}`)); w.Code != 200 {
		t.Fatalf("sync: %d %s", w.Code, w.Body.String())
	}
	afterSync := auditCounts()
	wantSync := map[string]int64{
		"put/admitted": files, "put/completed": files, "file.created/confirmed": files,
		"request/admitted": 1, "request/completed": 1,
	}
	if len(afterSync) != len(wantSync) {
		t.Fatalf("audit rows after a %d-file sync import = %v, want %v", files, afterSync, wantSync)
	}
	for key, n := range wantSync {
		if afterSync[key] != n {
			t.Fatalf("audit %s=%d after a %d-file sync import, want %d", key, afterSync[key], files, n)
		}
	}
	if n := changeCount(); n != 2*files {
		t.Fatalf("change facts after the sync=%d, want %d", n, 2*files)
	}

	// A sync that removes entries audits each removal (the only durable account of
	// what the index dropped) in the same transaction: one confirmed row per removal.
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("scanned%03d.txt", i)
		if err := os.Remove(filepath.Join(contentRoot, name)); err != nil {
			t.Fatal(err)
		}
	}
	if w := request(http.MethodPost, "/api/files/sync", []byte(`{"dryRun":false}`)); w.Code != 200 {
		t.Fatalf("removing sync: %d %s", w.Code, w.Body.String())
	}
	afterRemoval := auditCounts()
	if afterRemoval["file.deleted/confirmed"] != 2 {
		t.Fatalf("removal audit rows=%d, want one per removed entry (2); all=%v", afterRemoval["file.deleted/confirmed"], afterRemoval)
	}
	if afterRemoval["request/completed"] != 2 {
		t.Fatalf("request rows after two syncs=%d, want 2", afterRemoval["request/completed"])
	}
}
