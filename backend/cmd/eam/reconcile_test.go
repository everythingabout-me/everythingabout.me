package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

func TestReconcileRejectsUnsafeScopeBeforeConfiguration(t *testing.T) {
	for _, args := range [][]string{nil, {"--owner", " "}, {"--owner", "someone@example.test", "--timeout", "0s"}, {"--owner", "someone@example.test", "--max-entries", "-1"}, {"--owner", "someone@example.test", "--prune"}, {"--owner", "someone@example.test", "unexpected"}} {
		if err := runReconcile(args); err == nil {
			t.Fatalf("unsafe options accepted: %v", args)
		}
	}
}

type untouchedScanDB struct{ begins int }

func (d *untouchedScanDB) BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error) {
	d.begins++
	return nil, errors.New("database must not be reached")
}
func TestInvalidScanCannotStartDatabaseTransaction(t *testing.T) {
	for _, result := range []*treescan.Result{nil, {}} {
		db := &untouchedScanDB{}
		_, err := nodes.ApplyScan(context.Background(), db, "11111111-1111-1111-1111-111111111111", result, nodes.ScanApplyOptions{})
		if err == nil || db.begins != 0 {
			t.Fatalf("invalid scan reached database: %v, begins=%d", err, db.begins)
		}
	}
	db := &untouchedScanDB{}
	_, err := nodes.ApplyScan(context.Background(), db, "", nil, nodes.ScanApplyOptions{})
	if err == nil || db.begins != 0 {
		t.Fatal("missing owner reached database")
	}
}

type singleScanTree struct{ entry storage.Entry }

func (t singleScanTree) Stat(context.Context, storage.Path) (storage.Entry, error) {
	return storage.Entry{Kind: storage.Folder}, nil
}
func (t singleScanTree) List(context.Context, storage.Path, storage.PageRequest) (storage.Page, error) {
	return storage.Page{Entries: []storage.Entry{t.entry}}, nil
}
func (t singleScanTree) Open(context.Context, storage.Path, storage.ReadOptions) (storage.Read, error) {
	panic("index scan must never open native content")
}

// Explicit opt-in only. This test never loads application config or .env and
// creates an isolated schema in the supplied disposable Postgres database.
func TestReconcilePostgresIdentityDryRunAndAtomicity(t *testing.T) {
	url := os.Getenv("EAM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("EAM_TEST_DATABASE_URL is unset; disposable Postgres integration was not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("eam_scan_test_%x", random[:])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(context.Background(), "drop schema "+quoted+" cascade"); err != nil {
			t.Errorf("remove disposable schema: %v", err)
		}
	}()
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Relevant real schema constraints: private UUID primary keys, owner/path and
	// key uniqueness, arbitrary application metadata and FK-linked event media.
	_, err = pool.Exec(ctx, `create table users(id uuid primary key);
  create table nodes(
    id uuid primary key default gen_random_uuid(), owner_id uuid not null references users(id),
    key text not null unique,path text not null,parent text not null,name text not null,
    is_dir boolean not null default false,size bigint not null default 0,
    mime text,etag text,storage_class text,source_modified_at timestamptz,
    meta jsonb not null default '{}'::jsonb,
    created_at timestamptz not null default now(),updated_at timestamptz not null default now(),
    unique(owner_id,path));
  create table event_media(event_id uuid primary key,node_id uuid not null references nodes(id) on delete cascade);
  create table fs_jobs(user_id uuid not null,op text not null,status text not null,from_path text not null);
   create table file_content_generations(owner_id uuid not null,root_namespace text not null,path text not null,native_fingerprint bytea not null,content_revision text not null,primary key(owner_id,root_namespace,path));
  insert into users values ('11111111-1111-1111-1111-111111111111'),('22222222-2222-2222-2222-222222222222');
  insert into nodes(id,owner_id,key,path,parent,name,is_dir,size,mime,etag,storage_class,source_modified_at,meta,created_at,updated_at) values
   ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','11111111-1111-1111-1111-111111111111','drive/11111111-1111-1111-1111-111111111111/ folder /a ',' folder /a ',' folder ','a ',false,3,'text/plain','prior-etag','STANDARD','2020-01-02T03:04:05Z','{"annotation":"keep"}','2019-01-01T00:00:00Z','2020-01-01T00:00:00Z'),
   ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb','11111111-1111-1111-1111-111111111111','drive/11111111-1111-1111-1111-111111111111/index-only','index-only','','index-only',false,9,null,null,null,null,'{}','2019-01-01T00:00:00Z','2020-01-01T00:00:00Z'),
   ('cccccccc-cccc-cccc-cccc-cccccccccccc','22222222-2222-2222-2222-222222222222','drive/22222222-2222-2222-2222-222222222222/index-only','index-only','','index-only',false,10,null,null,null,null,'{}','2019-01-01T00:00:00Z','2020-01-01T00:00:00Z');
  insert into event_media values('dddddddd-dddd-dddd-dddd-dddddddddddd','aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa');`)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the focused scan fixture, then apply the real identity projection and
	// change-marker dependencies used by the current nodes repository. Identity
	// backfill intentionally synthesizes the known logical ancestor before scanning.
	for _, migration := range []string{"026_file_identity.sql", "027_canonical_file_tree.sql", "028_file_changes.sql", "031_effective_change_markers.sql"} {
		data, err := os.ReadFile("../../../sql/" + migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(data)); err != nil {
			t.Fatalf("migration %s: %v", migration, err)
		}
	}
	owner := "11111111-1111-1111-1111-111111111111"
	prefix := "drive/" + owner + "/"
	scan := func(body string) *treescan.Result {
		t.Helper()
		r, err := treescan.Inventory(ctx, strings.NewReader("key,size,lastmodifieddate\n"+body), prefix, treescan.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	result := scan(prefix + " folder /a ,3,2020-01-02T03:04:05Z\n" + prefix + "empty/,0,\n")
	snapshot := func() []string {
		t.Helper()
		rows, err := pool.Query(ctx, `select row_to_json(n)::text from nodes n order by owner_id,path`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var s []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				t.Fatal(err)
			}
			s = append(s, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snapshot()
	guard, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Exec(ctx, `select pg_advisory_lock(hashtextextended($1,0))`, "browser-write:"+owner); err != nil {
		guard.Release()
		t.Fatal(err)
	}
	_, lockedErr := nodes.ApplyScan(ctx, pool, owner, result, nodes.ScanApplyOptions{})
	_, unlockErr := guard.Exec(ctx, `select pg_advisory_unlock(hashtextextended($1,0))`, "browser-write:"+owner)
	guard.Release()
	if lockedErr == nil || !strings.Contains(lockedErr.Error(), "another file change") || unlockErr != nil {
		t.Fatalf("namespace lock was not respected: %v, unlock: %v", lockedErr, unlockErr)
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("blocked scan changed the index")
	}
	scanConn, finishScan, err := nodes.LockScanNamespace(ctx, pool, owner)
	if err != nil {
		t.Fatal(err)
	}
	_, reentrantErr := nodes.ApplyScan(ctx, scanConn, owner, result, nodes.ScanApplyOptions{DryRun: true})
	finishScan()
	if reentrantErr != nil {
		t.Fatalf("whole-scan lock blocked its own import: %v", reentrantErr)
	}
	// Failed deletion remains reserved too. Native spellings compare in NFC,
	// while matching must respect complete segments and the owner's namespace.
	if _, err := pool.Exec(ctx, `insert into fs_jobs values($1::uuid,'delete','failed','café')`, owner); err != nil {
		t.Fatal(err)
	}
	blocked := scan(prefix + "a-safe,0,\n" + prefix + "cafe\u0301/file,1,\n")
	for _, dryRun := range []bool{false, true} {
		if _, err := nodes.ApplyScan(ctx, pool, owner, blocked, nodes.ScanApplyOptions{DryRun: dryRun}); err == nil || !strings.Contains(err.Error(), "pending permanent deletion") {
			t.Fatalf("pending deletion imported (dry-run=%v): %v", dryRun, err)
		}
		if !reflect.DeepEqual(before, snapshot()) {
			t.Fatal("pending deletion rejection partially wrote")
		}
	}
	allowed := scan(prefix + "café-other,0,\n")
	if _, err := nodes.ApplyScan(ctx, pool, owner, allowed, nodes.ScanApplyOptions{DryRun: true}); err != nil {
		t.Fatalf("similarly named sibling was blocked: %v", err)
	}
	if _, err := pool.Exec(ctx, `update fs_jobs set from_path='parent/gone'`); err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.ApplyScan(ctx, pool, owner, scan(prefix+"parent/,0,\n"), nodes.ScanApplyOptions{DryRun: true}); err != nil {
		t.Fatalf("ancestor folder was blocked: %v", err)
	}
	if _, err := pool.Exec(ctx, `delete from fs_jobs`); err != nil {
		t.Fatal(err)
	}
	otherOwner := "22222222-2222-2222-2222-222222222222"
	if _, err := pool.Exec(ctx, `insert into fs_jobs values($1::uuid,'delete','failed',' folder '),($2::uuid,'delete','done',' folder ')`, otherOwner, owner); err != nil {
		t.Fatal(err)
	}
	report, err := nodes.ApplyScan(ctx, pool, owner, result, nodes.ScanApplyOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.Inserted != 1 || report.Updated != 0 || report.Unchanged != 2 || !reflect.DeepEqual(before, snapshot()) {
		t.Fatalf("dry-run wrote or miscounted: %#v", report)
	}
	report, err = nodes.ApplyScan(ctx, pool, owner, result, nodes.ScanApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Inserted != 1 || report.Updated != 0 || report.Unchanged != 2 {
		t.Fatalf("initial import: %#v", report)
	}
	after := snapshot()
	report, err = nodes.ApplyScan(ctx, pool, owner, result, nodes.ScanApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Inserted != 0 || report.Updated != 0 || report.Unchanged != 3 || !reflect.DeepEqual(after, snapshot()) {
		t.Fatalf("repeated import changed index: %#v", report)
	}
	var nodeID, meta, mime, etag, class string
	var updated time.Time
	err = pool.QueryRow(ctx, `select n.id::text,n.meta::text,n.mime,n.etag,n.storage_class,n.updated_at from nodes n join event_media m on m.node_id=n.id`).Scan(&nodeID, &meta, &mime, &etag, &class, &updated)
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" || !strings.Contains(meta, "keep") || mime != "text/plain" || etag != "prior-etag" || class != "STANDARD" || updated.Year() != 2020 {
		t.Fatalf("identity/metadata changed: %s %s %s %s %s %s", nodeID, meta, mime, etag, class, updated)
	}
	// An actual metadata change updates the same UUID while retaining event refs.
	changed := scan(prefix + " folder /a ,8,2021-01-02T03:04:05Z\n")
	report, err = nodes.ApplyScan(ctx, pool, owner, changed, nodes.ScanApplyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Updated != 1 {
		t.Fatalf("source change not imported: %#v", report)
	}
	var size int64
	if err := pool.QueryRow(ctx, `select size from nodes where id=$1::uuid`, nodeID).Scan(&size); err != nil || size != 8 {
		t.Fatalf("same node not updated: %d %v", size, err)
	}
	// Preflight must fail before inserting an earlier valid item; retained rows
	// include index-only entries, their implicit parents and the other owner.
	before = snapshot()
	conflict := scan(prefix + "a-new,0,\n" + prefix + "index-only/,0,\n")
	if _, err := nodes.ApplyScan(ctx, pool, owner, conflict, nodes.ScanApplyOptions{}); err == nil {
		t.Fatal("type conflict accepted")
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("failed import partially wrote")
	}
	// Force a later SQL constraint failure after the first insert, checking rollback.
	if _, err := pool.Exec(ctx, `alter table nodes add constraint reject_test_path check (path <> 'z-rejected')`); err != nil {
		t.Fatal(err)
	}
	before = snapshot()
	rejected := scan(prefix + "a-new,0,\n" + prefix + "z-rejected,0,\n")
	if _, err := nodes.ApplyScan(ctx, pool, owner, rejected, nodes.ScanApplyOptions{}); err == nil {
		t.Fatal("expected SQL rejection")
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("transaction failure leaked an earlier insert")
	}
	nativeScan, err := treescan.Scan(ctx, singleScanTree{entry: storage.Entry{Path: "native-ref", Kind: storage.File, NativeRef: []byte("opaque-provider-locator"), Version: []byte("opaque-native-version"), Size: 1}}, treescan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nodes.ApplyScan(ctx, pool, owner, nativeScan, nodes.ScanApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var mappedKey string
	if err := pool.QueryRow(ctx, `select key from nodes where owner_id=$1::uuid and path='native-ref'`, owner).Scan(&mappedKey); err != nil || mappedKey != prefix+"native-ref" {
		t.Fatalf("native reference used as private key: %q %v", mappedKey, err)
	}
	var refs int
	if err := pool.QueryRow(ctx, `select count(*) from event_media`).Scan(&refs); err != nil || refs != 1 {
		t.Fatalf("event refs changed: %d %v", refs, err)
	}
}
