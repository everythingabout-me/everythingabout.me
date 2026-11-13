package fileops

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestOperationJournalPredicatesReadColumns is the mechanism that keeps the column split
// from reverting. sql/047 promoted every file_operations field a SQL predicate read into a
// column, because a predicate that names a JSON key compares a *string* to a literal: a Go
// field rename then compiled, passed every test, and silently stopped matching, so every
// suspended operation became unrecoverable without an error anywhere. A column name is
// checked by the compiler and by the migration, so a rename fails loudly instead.
//
// The guard is deliberately textual rather than a SQL parser: every historical predicate was
// spelled with the arrow operators on a bare `snapshot` column, and forbidding that spelling
// catches the regression the day it is typed. `before_snapshot`/`after_snapshot` (the change
// feed) and a function argument named `snapshot` are not the journal column, and the word
// boundary excludes them.
func TestOperationJournalPredicatesReadColumns(t *testing.T) {
	journalRead := regexp.MustCompile(`\bsnapshot\s*->`)
	check := func(path, source string) {
		t.Helper()
		for number, line := range strings.Split(source, "\n") {
			// `Entries` is the one key of the document that has no promoted column: it is the
			// per-entry list itself, which Release A copies into file_operation_entries. The
			// backfill (sql/050) and the shadow-agreement test must read the whole list, so
			// that exact key is allowed; any other arrow read stays banned, because a
			// predicate over the entries' own fields (number, state, ...) still spells an
			// arrow out of the document and does not match this exact token.
			line = strings.ReplaceAll(line, "snapshot->'Entries'", "Entries(list)")
			line = strings.ReplaceAll(line, "snapshot -> 'Entries'", "Entries(list)")
			if journalRead.MatchString(line) {
				t.Errorf("%s:%d reads a journal field out of JSON: %s\n\tquery the promoted column sql/047 added instead",
					path, number+1, strings.TrimSpace(line))
			}
		}
	}
	// The backend is where queries live. Test sources are included on purpose: a fixture that
	// writes only the document produces the exact row shape this change exists to eliminate.
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		check(path, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A new migration can reintroduce the shape too. Everything up to and including 047 is
	// history: 047 is the backfill that reads the old documents once, and 041-043 are the
	// superseded indexes it replaces.
	migrations, err := filepath.Glob(filepath.Join("..", "..", "..", "sql", "*.sql"))
	if err != nil || len(migrations) < 17 {
		t.Fatalf("migration discovery: %d %v", len(migrations), err)
	}
	for _, path := range migrations {
		if filepath.Base(path) <= "047_file_operations_columns.sql" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		check(path, string(data))
	}
}
