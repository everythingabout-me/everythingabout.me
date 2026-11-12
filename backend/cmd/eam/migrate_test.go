package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationFilesSortsAndIgnoresOtherEntries(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("043_file_operations_reclaim_index.sql", "select 1;")
	write("001_init.sql", "select 1;")
	write(versionTableMigration, "select 1;")
	write("notes.md", "not a migration")
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("nested", "002_nested.sql"), "select 1;")

	files, err := migrationFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"001_init.sql", "043_file_operations_reclaim_index.sql", versionTableMigration}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("migration order = %v, want %v", files, want)
	}
}

func TestMigrationFilesRequiresVersionTableMigration(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "001_init.sql"), []byte("select 1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := migrationFiles(dir); err == nil || !strings.Contains(err.Error(), versionTableMigration) {
		t.Fatalf("missing %s must be reported by name, got %v", versionTableMigration, err)
	}

	empty := t.TempDir()
	if _, err := migrationFiles(empty); err == nil {
		t.Fatal("an empty directory must be rejected")
	}
}

func TestBootstrapRangeStopsAtVersionTableMigration(t *testing.T) {
	files := []string{
		"001_init.sql",
		"035_storage_connections.sql",
		"043_file_operations_reclaim_index.sql",
		versionTableMigration,
		"045_later.sql",
	}
	want := []string{
		"001_init.sql",
		"035_storage_connections.sql",
		"043_file_operations_reclaim_index.sql",
	}
	if got := bootstrapRange(files); !reflect.DeepEqual(got, want) {
		t.Fatalf("bootstrap range = %v, want %v", got, want)
	}

	onlyNew := []string{versionTableMigration, "045_later.sql"}
	if got := bootstrapRange(onlyNew); len(got) != 0 {
		t.Fatalf("bootstrap range = %v, want none", got)
	}
}

func TestShouldBootstrapRequiresEmptyHistoryAndExistingSchema(t *testing.T) {
	for _, tc := range []struct {
		name     string
		empty    bool
		core     bool
		expected bool
	}{
		{name: "fresh database", empty: true, core: false, expected: false},
		{name: "pre-versioning database", empty: true, core: true, expected: true},
		{name: "versioned database", empty: false, core: true, expected: false},
		{name: "versioned empty schema", empty: false, core: false, expected: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldBootstrap(tc.empty, tc.core); got != tc.expected {
				t.Fatalf("shouldBootstrap(%v, %v) = %v, want %v", tc.empty, tc.core, got, tc.expected)
			}
		})
	}
}

func TestLockTimeoutRecognition(t *testing.T) {
	timeout := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	if !isLockTimeout(timeout) {
		t.Fatal("55P03 must be recognized as a lock timeout")
	}
	if !isLockTimeout(fmt.Errorf("statement failed: %w", timeout)) {
		t.Fatal("a wrapped 55P03 must be recognized as a lock timeout")
	}
	if isLockTimeout(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("a unique violation is not a lock timeout")
	}
	if isLockTimeout(errors.New("dial error")) {
		t.Fatal("a non-Postgres error is not a lock timeout")
	}
	if isLockTimeout(nil) {
		t.Fatal("nil is not a lock timeout")
	}
}

// TestVersionTableDDLMatchesMigration keeps the runner's copy of the definition from
// drifting away from the migration a reader consults. The runner must create the table
// before the loop reaches 044, so the two definitions have to be the same table.
func TestVersionTableDDLMatchesMigration(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "sql", versionTableMigration))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), versionTableDDL) {
		t.Fatalf("sql/%s no longer contains the runner's schema_migrations definition:\n%s", versionTableMigration, versionTableDDL)
	}
}
