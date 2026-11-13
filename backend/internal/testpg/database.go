// Scratch databases for Postgres-backed tests.
//
// Every such test needs a private database: it applies every migration twice,
// seeds its own rows, and must never see another test's schema. CreateDatabase
// is the only implementation of that create/drop pair, so the cleanup policy
// lives in one place instead of being copied (and drifting) in every package.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// createTimeout bounds creating a scratch database. Creating one is a
	// single catalog write on an otherwise idle server; this only has to cover
	// a loaded test machine, not provider work.
	createTimeout = 30 * time.Second
	// cleanupTimeout bounds the drop, including every retry. It is deliberately
	// longer than the five-second defer this helper replaces: when the whole
	// suite runs in parallel, DROP DATABASE waits behind the other packages'
	// catalog writes, and a five-second budget turned that queueing into a
	// false failure at deferred cleanup. Sixty seconds is six times the old
	// budget while still bounding a genuinely stuck drop.
	cleanupTimeout = 60 * time.Second
	// cleanupRetryDelay is the pause between drop attempts. DROP DATABASE ...
	// WITH (FORCE) terminates existing sessions, but a session that arrives
	// between the termination and the drop still makes it fail with
	// object_in_use; the retry covers that race.
	cleanupRetryDelay = 500 * time.Millisecond
)

// Database is a scratch database on the server named by EAM_TEST_DATABASE_URL.
//
// Name is the exact database name, kept visible so a caller's own diagnostic
// and the cleanup failure message can both identify a leftover. URL connects
// to the same server with Name selected and the inherited search_path removed
// (a search_path pointing at a schema that does not exist yet would break the
// first migrations).
type Database struct {
	Name string
	URL  string

	address string
}

// CreateDatabase creates a randomly named, private database on the disposable
// PostgreSQL service and registers its removal with t.Cleanup.
//
// The name is prefix plus sixteen random hex characters; the caller's prefix
// should describe the package (for example "eam_fileops_") so a leftover is
// recognizable in `select datname from pg_database where datname like 'eam%'`.
//
// Cleanup contract: after the test finishes it closes nothing on the caller's
// behalf, but it drops the database with a bounded deadline of sixty seconds,
// retrying while the server still reports the database in use. If the drop
// cannot finish, the test is NOT marked failed: its assertions already ran,
// and a cleanup-only failure is an infrastructure problem, not evidence about
// the code under test; failing here would turn a passing run red for a reason
// unrelated to what it covered. It is not silent either: the helper logs the
// database name, the number of attempts and the last error, prints the exact
// `drop database ... with (force)` command, and appends the name and error to
// EAM_TEST_LEAK_LOG, which the Makefile prints after a run. That last part
// matters because `go test` discards a passing package's output, so a t.Logf
// alone would not reach the terminal of the gate that is supposed to catch it.
func CreateDatabase(t testing.TB, prefix string) *Database {
	t.Helper()
	address := Require(t)

	ctx, cancel := context.WithTimeout(context.Background(), createTimeout)
	defer cancel()

	// Build the isolated URL before touching the server so a malformed
	// EAM_TEST_DATABASE_URL fails without creating a database nobody can reach.
	name := prefix + randomSuffix()
	isolated := isolatedURL(t, address, name)

	admin, err := pgx.Connect(ctx, address)
	if err != nil {
		t.Fatalf("testpg: connect to the disposable server named by %s: %v", DatabaseURLVariable, err)
	}
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{name}.Sanitize()); err != nil {
		_ = admin.Close(context.Background())
		t.Fatalf("testpg: create scratch database %q on %s: %v", name, DatabaseURLVariable, err)
	}
	// The cleanup connection is opened on demand, so this one has no further
	// role. A close error here cannot strand the scratch database.
	_ = admin.Close(context.Background())

	database := &Database{Name: name, URL: isolated, address: address}
	t.Cleanup(func() { database.drop(t) })
	return database
}

// drop removes the scratch database. It never calls t.Fatal or t.Error: see
// the cleanup contract on CreateDatabase.
func (d *Database) drop(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	quoted := pgx.Identifier{d.Name}.Sanitize()
	attempts := 0
	var lastErr error
	for {
		attempts++
		lastErr = d.dropOnce(ctx, quoted)
		if lastErr == nil {
			return
		}
		if ctx.Err() != nil || !retryableDropError(lastErr) {
			break
		}
		timer := time.NewTimer(cleanupRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
			continue
		}
		break
	}
	t.Logf("testpg: scratch database %s was NOT dropped after %d attempt(s) within %s; it is left behind. "+
		"Last error: %v. Remove it with: drop database %s with (force);",
		quoted, attempts, cleanupTimeout, lastErr, quoted)
	recordLeftover(d.Name, lastErr)
}

// recordLeftover appends the failure to EAM_TEST_LEAK_LOG so the Makefile can
// print it after the run even though the passing test's own output is hidden.
// Like recordSkip it is best-effort: failing to write a diagnostic must not
// turn cleanup into a test failure.
func recordLeftover(name string, cause error) {
	path := os.Getenv(LeftoverLogVariable)
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\t%s\n", name, strings.ReplaceAll(cause.Error(), "\n", " "))
}

// dropOnce opens a fresh connection for one drop attempt. A fresh connection
// keeps a retry possible after a previous attempt's connection was closed by a
// cancelled context, and avoids holding an idle administrator connection for
// the whole test.
func (d *Database) dropOnce(ctx context.Context, quoted string) error {
	admin, err := pgx.Connect(ctx, d.address)
	if err != nil {
		return err
	}
	_, dropErr := admin.Exec(ctx, "drop database "+quoted+" with (force)")
	// The drop result is what this helper reports; a failed close cannot leave
	// the scratch database behind by itself.
	_ = admin.Close(context.Background())
	if dropErr != nil && databaseDoesNotExist(dropErr) {
		// A previous attempt committed but its reply was lost, e.g. the context
		// expired just as the server dropped the database. The goal is reached.
		return nil
	}
	return dropErr
}

// retryableDropError reports whether another drop attempt can succeed. An
// object_in_use refusal is the race WITH (FORCE) leaves open, and connection
// failures are worth retrying while the deadline lasts. Authentication and
// privilege errors are terminal: retrying cannot fix them.
func retryableDropError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "3D000": // invalid_catalog_name: already dropped
			return false
		case "42501", "28P01", "28000", "42602": // privilege / authentication / invalid name
			return false
		}
	}
	return true
}

func databaseDoesNotExist(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "3D000"
}

// isolatedURL returns address with the database replaced by name.
func isolatedURL(t testing.TB, address, name string) string {
	t.Helper()
	parsed, err := url.Parse(address)
	if err != nil {
		t.Fatalf("%s must be a postgres:// URL naming a disposable server: %v", DatabaseURLVariable, err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		t.Fatalf("%s must be a postgres:// or postgresql:// URL naming a disposable server, not %q", DatabaseURLVariable, parsed.Scheme)
	}
	parsed.Path = "/" + name
	parsed.RawPath = ""
	query := parsed.Query()
	query.Del("search_path")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func randomSuffix() string {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand.Read never fails on supported platforms, and there is no
		// meaningful fallback for a uniqueness requirement; fail loudly rather
		// than create two tests' databases with the same name.
		panic(fmt.Sprintf("testpg: random database suffix: %v", err))
	}
	return hex.EncodeToString(random[:])
}
