// Package testpg is the single gate for tests that need a disposable
// PostgreSQL service.
//
// Those tests create and drop their own databases, so they must never run
// against whatever DATABASE_URL happens to point at. They opt in through
// EAM_TEST_DATABASE_URL and skip without it. That skip used to be silent: a
// read-only-transaction bug in ListUploads shipped because its tests skipped
// without anyone noticing. Two mechanisms make the skip visible:
//
//   - Every skip is appended to the file named by EAM_TEST_SKIP_LOG, and the
//     Makefile turns that file into a count printed after the run. The count is
//     a file rather than stdout because `go test` discards a passing package's
//     output unless -v is set, so a stdout-only warning would itself be easy
//     to miss.
//   - EAM_TEST_REQUIRE_DATABASE_URL=1 turns a missing URL into a test failure.
//     make test-pg and CI set it, so a required run cannot pass by skipping.
//
// Scratch-database cleanup follows the same rule for the same reason: a drop
// that cannot finish is appended to EAM_TEST_LEAK_LOG and the Makefile prints
// it, because a t.Logf warning on a passing test never reaches the terminal.
package testpg

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	// DatabaseURLVariable names a disposable PostgreSQL service with CREATEDB.
	// The tests apply every migration to random databases they create and drop;
	// pointing this at a live database would destroy or pollute it.
	DatabaseURLVariable = "EAM_TEST_DATABASE_URL"
	// RequireVariable turns a missing EAM_TEST_DATABASE_URL into a failure
	// instead of a skip.
	RequireVariable = "EAM_TEST_REQUIRE_DATABASE_URL"
	// SkipLogVariable names the file that receives one line per skipped test:
	// the source file, a tab, then the test name.
	SkipLogVariable = "EAM_TEST_SKIP_LOG"
	// LeftoverLogVariable names the file that receives one line per scratch
	// database that could not be dropped: the database name, a tab, then the
	// last error. The Makefile prints its contents, so a leak is not silent even
	// though the test itself stays green (see CreateDatabase's cleanup contract).
	LeftoverLogVariable = "EAM_TEST_LEAK_LOG"
)

// Require returns the disposable PostgreSQL URL or stops the test.
//
// Without a URL the test skips and the skip is counted, unless
// EAM_TEST_REQUIRE_DATABASE_URL=1, in which case it fails: a release gate must
// not go green by running nothing.
func Require(t testing.TB) string {
	t.Helper()
	if url := os.Getenv(DatabaseURLVariable); url != "" {
		return url
	}
	recordSkip(t)
	if os.Getenv(RequireVariable) == "1" {
		t.Fatalf("%s is not set: the Postgres-backed suite is required (%s=1) and cannot skip. "+
			"Start a disposable PostgreSQL with CREATEDB, set %s, and run `make test-pg`.",
			DatabaseURLVariable, RequireVariable, DatabaseURLVariable)
	}
	t.Skipf("%s is unset; Postgres-backed test skipped. Run `make test-pg` with a disposable PostgreSQL URL to execute it.", DatabaseURLVariable)
	return ""
}

// recordSkip appends the calling test's file and name to the skip log. It is
// best-effort: a counting problem must never turn a skip into a failure.
//
// The entry point is Require, but tests usually reach it through
// CreateDatabase. Walk past this package's own frames so the log names the
// test's file, not database.go, which is what makes the count useful when
// investigating a skip.
func recordSkip(t testing.TB) {
	path := os.Getenv(SkipLogVariable)
	if path == "" {
		return
	}
	file := "unknown"
	for skip := 2; skip < 8; skip++ {
		_, caller, _, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		if strings.Contains(filepath.ToSlash(caller), "/internal/testpg/") {
			continue
		}
		file = caller
		break
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\t%s\n", file, t.Name())
}
