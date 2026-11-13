package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrateTestPool creates a private scratch database on an explicitly supplied disposable
// server. Like the other Postgres integration tests, it skips without EAM_TEST_DATABASE_URL
// and never loads application configuration.
func migrateTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_migrate_")
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// copyMigrations puts the repository's migrations in a private directory so a test can add
// a later or failing file without touching sql/.
func copyMigrations(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir("../../../sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../../../sql", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runTestMigrate(t *testing.T, pool *pgxpool.Pool, dir string) (string, error) {
	t.Helper()
	files, err := migrationFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var out bytes.Buffer
	err = runMigrationSession(ctx, conn, dir, files, &out)
	return out.String(), err
}

func countMigrations(t *testing.T, pool *pgxpool.Pool) (baseline, executed int) {
	t.Helper()
	ctx := context.Background()
	if err := pool.QueryRow(ctx, `select count(*) from schema_migrations where method='baseline'`).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from schema_migrations where method='executed'`).Scan(&executed); err != nil {
		t.Fatal(err)
	}
	return baseline, executed
}

func TestMigrateAppliesOnceSkipsAndRollsBackFailures(t *testing.T) {
	pool := migrateTestPool(t)
	dir := t.TempDir()
	copyMigrations(t, dir)
	files, err := migrationFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	output, err := runTestMigrate(t, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Match the count fragment, not the summary's singular/plural wording: the history rows
	// below are the contract.
	if !strings.Contains(output, fmt.Sprintf("applied %d ", len(files))) {
		t.Fatalf("first run did not apply every migration:\n%s", output)
	}
	baseline, executed := countMigrations(t, pool)
	if baseline != 0 || executed != len(files) {
		t.Fatalf("first run recorded baseline=%d executed=%d, want baseline=0 executed=%d", baseline, executed, len(files))
	}

	output, err = runTestMigrate(t, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "nothing to apply") {
		t.Fatalf("second run was not a no-op:\n%s", output)
	}
	if baseline, executed = countMigrations(t, pool); baseline != 0 || executed != len(files) {
		t.Fatalf("second run changed history: baseline=%d executed=%d", baseline, executed)
	}

	// A file added later applies on the next run.
	extra := filepath.Join(dir, "045_migrate_test_extra.sql")
	if err := os.WriteFile(extra, []byte("create table migrate_test_extra (id int primary key);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err = runTestMigrate(t, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "045_migrate_test_extra.sql OK") {
		t.Fatalf("later migration was not applied:\n%s", output)
	}

	// A failing file aborts, leaves its statements rolled back, and is not recorded.
	failing := filepath.Join(dir, "046_migrate_test_fail.sql")
	if err := os.WriteFile(failing, []byte("create table migrate_test_must_roll_back (id int);\nselect * from migrate_test_missing_relation;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err = runTestMigrate(t, pool, dir)
	if err == nil {
		t.Fatalf("failing migration was accepted:\n%s", output)
	}
	if !strings.Contains(err.Error(), "046_migrate_test_fail.sql") {
		t.Fatalf("failure does not name the file: %v", err)
	}
	var recorded, rolledBack bool
	if err := pool.QueryRow(context.Background(), `select exists(select 1 from schema_migrations where name='046_migrate_test_fail.sql')`).Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `select to_regclass('migrate_test_must_roll_back') is not null`).Scan(&rolledBack); err != nil {
		t.Fatal(err)
	}
	if recorded {
		t.Fatal("the failed file was recorded as applied")
	}
	if rolledBack {
		t.Fatal("the failed file's statements were committed")
	}
}

func TestMigrateBootstrapsAPreVersioningDatabase(t *testing.T) {
	pool := migrateTestPool(t)
	ctx := context.Background()
	// A pre-versioning database already has the core schema. Reproduce the one fact the
	// rule keys on without replaying 43 files: the plan is to prove 001-043 are adopted,
	// not executed (in particular 035 must not run and create storage_connections).
	if _, err := pool.Exec(ctx, `create table nodes (id int primary key)`); err != nil {
		t.Fatal(err)
	}
	// Files after 044 still execute here (the rule cannot tell a history-less full
	// schema from a history-less partial one), so the stub must carry the relations
	// those files extend. 045 needs the timeline tables, 047 promotes columns on the
	// operation journal and indexes the change feed by the operation that produced a
	// fact. Add another relation when a later file references a pre-044 table: a stub
	// that lags the migration it exercises fails as a missing relation, which is a
	// fixture error, not a bootstrap-rule error.
	for _, statement := range []string{
		`create table users (id uuid primary key)`,
		`create table timeline_assets (user_id uuid not null, node_id uuid not null)`,
		`create table timeline_jobs (id bigserial primary key, user_id uuid not null, kind text not null, status text not null default 'pending')`,
		`create table file_operations (id uuid primary key, owner_id uuid not null, action text not null, snapshot jsonb not null)`,
		`create table file_changes (owner_id uuid not null, operation_id text not null default '')`,
		`create table file_operation_guards (comparison_path text not null)`,
		`create table file_uploads (comparison_path text not null)`,
		`create table file_credit_claims (owner_id uuid not null, comparison_path text not null, version bytea not null, operation_id uuid not null)`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	copyMigrations(t, dir)
	files, err := migrationFiles(dir)
	if err != nil {
		t.Fatal(err)
	}

	output, err := runTestMigrate(t, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	baseline, executed := countMigrations(t, pool)
	wantBaseline := len(bootstrapRange(files))
	if baseline != wantBaseline || executed != len(files)-wantBaseline {
		t.Fatalf("bootstrap recorded baseline=%d executed=%d, want baseline=%d executed=%d\n%s", baseline, executed, wantBaseline, len(files)-wantBaseline, output)
	}
	if !strings.Contains(output, "bootstrap:") {
		t.Fatalf("bootstrap reason was not reported:\n%s", output)
	}
	var storageConnections bool
	if err := pool.QueryRow(ctx, `select to_regclass('storage_connections') is not null`).Scan(&storageConnections); err != nil {
		t.Fatal(err)
	}
	if storageConnections {
		t.Fatal("bootstrap executed 035: storage_connections must not exist")
	}
	var reason string
	if err := pool.QueryRow(ctx, `select reason from schema_migrations where method='baseline' limit 1`).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reason, "adopted without execution") {
		t.Fatalf("baseline row does not record why: %q", reason)
	}

	output, err = runTestMigrate(t, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "nothing to apply") {
		t.Fatalf("second bootstrap run was not a no-op:\n%s", output)
	}

	// Files from 044 on were never replayed by the old runner, so they must still apply.
	extra := filepath.Join(dir, "045_migrate_test_extra.sql")
	if err := os.WriteFile(extra, []byte("create table migrate_test_extra (id int primary key);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err = runTestMigrate(t, pool, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "045_migrate_test_extra.sql OK") {
		t.Fatalf("later migration was not applied after bootstrap:\n%s", output)
	}
}

// TestMigrateBackfillsOperationsThatAlreadyHaveDocuments runs the journal schema changes
// against a database that already holds operations.
//
// The other migration tests start from an empty database, so a backfill never sees a row.
// That is exactly how a backfill that assumed `snapshot->'Entries'` is always an array
// reached main: a document written by a nil Go slice stores JSON null, and
// `jsonb_array_length` on it aborted the whole run on a database that had data.
func TestMigrateBackfillsOperationsThatAlreadyHaveDocuments(t *testing.T) {
	pool := migrateTestPool(t)
	ctx := context.Background()

	before := t.TempDir()
	entries, err := os.ReadDir("../../../sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") || name > "046_" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("../../../sql", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(before, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runTestMigrate(t, pool, before); err != nil {
		t.Fatalf("migrate to 046: %v", err)
	}

	owner := "11111111-1111-4111-8111-111111111111"
	if _, err := pool.Exec(ctx, `insert into users(id,email,created_at) values($1::uuid,$2,now())`,
		owner, "backfill@example.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Seed real Operation documents rather than hand-written JSON: the backfill reads the
	// document's own field names, and a fixture that invents a shape tests nothing.
	mkOp := func(id string, entries []fileops.EntryEffect) []byte {
		op := fileops.Operation{
			ID:          id,
			Root:        fileops.Root{OwnerID: owner, ConnectionID: "fixture", Generation: 1, ContentRoot: "/content", SystemRoot: "/system", CaseSensitive: true},
			PrincipalID: "owner-test",
			Kind:        "copy",
			State:       fileops.Queued,
			Phase:       fileops.Admitted,
			Fingerprint: "fingerprint-" + id,
			CreatedAt:   time.Now().UTC(),
			RetainUntil: time.Now().UTC().Add(time.Hour),
			Entries:     entries,
		}
		data, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("marshal operation: %v", err)
		}
		return data
	}
	// A nil slice marshals to JSON null: that is the shape that broke the first backfill.
	empty := mkOp("22222222-2222-4222-8222-222222222222", nil)
	list := mkOp("33333333-3333-4333-8333-333333333333", []fileops.EntryEffect{
		{ManifestEntry: fileops.ManifestEntry{Number: 1, Action: "copy", Source: "/a", Target: "/b", Size: 3}, State: "pending"},
		{ManifestEntry: fileops.ManifestEntry{Number: 2, Action: "copy", Source: "/c", Target: "/d", Size: 4}, State: "applied"},
	})
	for _, row := range []struct {
		id  string
		doc []byte
	}{{"22222222-2222-4222-8222-222222222222", empty}, {"33333333-3333-4333-8333-333333333333", list}} {
		if _, err := pool.Exec(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot)
			values ($1::uuid,$2::uuid,'owner-test','copy',$3,$4::jsonb)`, row.id, owner, "f-"+row.id, row.doc); err != nil {
			t.Fatalf("seed operation %s: %v", row.id, err)
		}
	}

	after := t.TempDir()
	copyMigrations(t, after)
	if _, err := runTestMigrate(t, pool, after); err != nil {
		t.Fatalf("migrate with existing rows: %v", err)
	}

	var emptyCount, listCount int
	if err := pool.QueryRow(ctx, `select entry_count from file_operations where id='22222222-2222-4222-8222-222222222222'`).Scan(&emptyCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select entry_count from file_operations where id='33333333-3333-4333-8333-333333333333'`).Scan(&listCount); err != nil {
		t.Fatal(err)
	}
	if emptyCount != 0 || listCount != 2 {
		t.Fatalf("entry_count = %d/%d, want 0/2", emptyCount, listCount)
	}
	var rows int
	if err := pool.QueryRow(ctx, `select count(*) from file_operation_entries where operation_id='33333333-3333-4333-8333-333333333333'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("backfilled %d entries, want 2", rows)
	}
	var layout string
	if err := pool.QueryRow(ctx, `select entries_layout from file_operations where id='33333333-3333-4333-8333-333333333333'`).Scan(&layout); err != nil {
		t.Fatal(err)
	}
	if layout != "document" {
		t.Fatalf("backfill must leave the row document-backed, got %q", layout)
	}
}
