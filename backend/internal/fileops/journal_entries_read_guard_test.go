package fileops

// Release B guard for docs/design/journal-entries.md §2.3/§2.7.
//
// Release A's guard (TestJournalEntriesTableIsWriteOnly) existed to keep the table
// write-only until the flip. Release B inverts the requirement: the table *is* the entry
// record, and the failure the guard must now catch is the one the design calls the
// riskiest part — the settlement load being hoisted out of the transaction that holds the
// parent row lock.
//
// The guard is deliberately textual, in the style of TestOperationJournalPredicatesReadColumns:
//
//   - Every statement that names file_operation_entries stays in postgres_entries.go, so a
//     reader cannot grow a second, divergent spelling somewhere else.
//   - loadEntries (the one ordered manifest load) is called from Settle in
//     postgres_transitions.go and from nowhere else in production code. postgres.go is
//     where Get/readOperation live, so a call there is exactly the "load it with the
//     operation" refactor the design forbids.
//   - Claim upgrades a 'document' row (the only normal-operation layout flip), and writers
//     produce 'table' rows.
//
// Test sources are exempt: they hold the table to its contract, and a legacy-fixture test
// has to build the old document shape to prove the upgrade.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const entriesSQLOwner = "postgres_entries.go"

func TestEntriesTableReadsStayInSettlement(t *testing.T) {
	tableName := regexp.MustCompile(`\bfile_operation_entries\b`)
	tableRead := regexp.MustCompile(`(?i)\b(from|join)\s+file_operation_entries\b`)
	manifestLoad := regexp.MustCompile(`\bloadEntries\(`)
	root := filepath.Join("..", "..")
	scanned := 0
	var transitions, postgres string
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
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		scanned++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		source := string(data)
		switch filepath.Base(path) {
		case "postgres_transitions.go":
			transitions = source
		case "postgres.go":
			postgres = source
		}
		for number, line := range strings.Split(source, "\n") {
			// Comment lines explain the contract and do not execute; a commented-out query
			// is not a reader.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			base := filepath.Base(path)
			if (tableName.MatchString(line) || tableRead.MatchString(line)) && base != entriesSQLOwner {
				t.Errorf("%s:%d names file_operation_entries outside %s: %s\n\tsql that touches the entry table lives in one file so the settlement load cannot grow a second spelling",
					path, number+1, entriesSQLOwner, strings.TrimSpace(line))
			}
			if manifestLoad.MatchString(line) && base != entriesSQLOwner && base != "postgres_transitions.go" {
				t.Errorf("%s:%d loads the whole manifest outside settlement: %s\n\tRelease B's one ordered load belongs in Settle's transaction (§2.3); a per-transition or per-Get caller must use the one-row accessor instead",
					path, number+1, strings.TrimSpace(line))
			}
			if base == "postgres.go" && manifestLoad.MatchString(line) {
				t.Errorf("%s:%d loads the manifest in the file that implements Get/readOperation: %s\n\tthis is the refactor §2.7 forbids: an operation read would carry a list that the settlement then trusts without holding the parent row lock",
					path, number+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 || transitions == "" || postgres == "" {
		t.Fatalf("only %d production Go files scanned (transitions=%d bytes, postgres=%d bytes); the walk is not finding the backend",
			scanned, len(transitions), len(postgres))
	}

	// The ordered load must be inside Settle, which already holds the parent row lock from
	// readOperation ... FOR UPDATE. This is the guard's real assertion: not that the SQL
	// exists, but where it runs.
	settleBody := functionBody(t, transitions, "func (p *Postgres) Settle(")
	if !manifestLoad.MatchString(settleBody) {
		t.Fatal("Settle does not load the manifest entries inside its transaction; the design's riskiest invariant is gone (§2.3)")
	}
	// The load must sit inside the `leased` callback, which is what takes the parent row
	// lock (readOperation ... FOR UPDATE) before the callback runs.
	locked := strings.Index(settleBody, "p.leased(")
	load := strings.Index(settleBody, "loadEntries(")
	if locked < 0 || load < locked {
		t.Fatal("Settle loads the manifest entries outside its leased transaction; the load must run after the parent row lock (§2.3)")
	}

	// Claim is the one normal-operation layout flip, so an operation written before this
	// release can still settle.
	claimBody := functionBody(t, postgres, "func (p *Postgres) Claim(")
	if !strings.Contains(claimBody, "upgradeEntries(") {
		t.Fatal("Claim does not upgrade a 'document' row; a pre-Release-B operation could never settle (§2.5)")
	}
	// New rows are written as table-backed, explicitly (the column default stays
	// 'document' as a safety net, so a forgotten writer produces a readable document).
	if !strings.Contains(postgres, `args["entries_layout"] = string(op.EntriesLayout)`) || !strings.Contains(postgres, "EntriesLayout: EntriesLayoutTable") {
		t.Fatal("admission no longer writes new operations with entries_layout='table'")
	}
	if _, err := os.Stat(entriesSQLOwner); err != nil {
		t.Fatalf("entry SQL owner %s is missing: %v", entriesSQLOwner, err)
	}
}

// functionBody returns the source of one Go function: from its declaration to the next
// top-level func. The guard only needs a coarse body, not a parser.
func functionBody(t *testing.T, source, declaration string) string {
	t.Helper()
	start := strings.Index(source, declaration)
	if start < 0 {
		t.Fatalf("%q not found", declaration)
	}
	rest := source[start+len(declaration):]
	if next := strings.Index(rest, "\nfunc "); next >= 0 {
		rest = rest[:next]
	}
	return rest
}
