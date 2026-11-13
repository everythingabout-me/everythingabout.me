package fileops

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shadowWriter is the one production file Release A allows to name file_operation_entries.
// Every write to the table goes through it, so the shadow cannot grow a second, divergent
// statement elsewhere.
const shadowWriter = "postgres_entries.go"

// TestJournalEntriesTableIsWriteOnly is the guard that keeps Release A additive.
//
// Release B (docs/design/journal-entries.md §2.6) is the release that lets readers use the
// table: `Get`/settlement/Claim branch on `entries_layout`, and `Settle` loads the entries from
// the table inside the transaction that holds the parent row lock (§2.3). Doing any of that one
// release early would change behaviour with no tested rollback: the document is still the
// authority in Release A, so a reader that preferred the table would be reading a copy that a
// deploy-window row may not have, and one that branched on `entries_layout` would already be
// implementing the flip. Rather than a convention, this test fails the day a production source
// does either:
//
//   - names file_operation_entries anywhere except postgres_entries.go (the shadow writer);
//   - reads it (`from`/`join file_operation_entries`) even there;
//   - mentions entries_layout at all, which no Release A code path needs and every Release B
//     reader will.
//
// Test sources are exempt because they are where the shadow is verified (a test has to read
// the table to compare it with the document) and where the backfill is exercised. sql/ is
// exempt because the backfill and the verification query are SQL by design.
func TestJournalEntriesTableIsWriteOnly(t *testing.T) {
	tableName := regexp.MustCompile(`\bfile_operation_entries\b`)
	tableRead := regexp.MustCompile(`(?i)\b(from|join)\s+file_operation_entries\b`)
	layoutFlag := regexp.MustCompile(`\bentries_layout\b`)
	scanned := 0
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(data)
		for number, line := range strings.Split(source, "\n") {
			// Comment lines are allowed to explain the contract (this release's own comments
			// do); the guard is about code and SQL, so a commented-out query is not a reader.
			// Like TestOperationJournalPredicatesReadColumns this is deliberately textual: the
			// spellings it forbids are exactly the ones a reader or a layout branch is typed
			// as, so the day it is typed, the test fails.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if tableName.MatchString(line) && filepath.Base(path) != shadowWriter {
				t.Errorf("%s:%d names file_operation_entries: %s\n\tRelease A allows the table only as a write in %s, and only if no reader uses it yet",
					path, number+1, strings.TrimSpace(line), shadowWriter)
			}
			if tableRead.MatchString(line) {
				t.Errorf("%s:%d reads file_operation_entries: %s\n\tRelease A has no reader switch; Release B (§2.3/§2.4 of docs/design/journal-entries.md) adds the readers together with the layout branch",
					path, number+1, strings.TrimSpace(line))
			}
			if layoutFlag.MatchString(line) || strings.Contains(line, "EntriesLayout") {
				t.Errorf("%s:%d mentions entries_layout in code: %s\n\tRelease A never branches on the layout; the column stays 'document' until Release B",
					path, number+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("only %d production Go files scanned; the walk is not finding the backend", scanned)
	}
	// The allowlist is only meaningful if the file it names exists; a rename would otherwise
	// silently turn this test into "the table may not be named anywhere".
	if _, err := os.Stat(shadowWriter); err != nil {
		t.Fatalf("shadow writer %s is missing: %v", shadowWriter, err)
	}
}
