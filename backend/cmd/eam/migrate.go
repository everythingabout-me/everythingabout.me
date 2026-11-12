package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/store"
)

const (
	// versionTableMigration creates schema_migrations. It is also the bootstrap boundary:
	// every file that sorts before it had already been replayed by the pre-versioning
	// runner and is adopted, never executed, when the version table is introduced into an
	// existing database. The 0NN prefix makes filename order the migration order, so the
	// same string comparison decides both execution order and the boundary.
	versionTableMigration = "044_schema_migrations.sql"

	// bootstrapCoreTable is the table from the very first migration (001_init.sql). If it
	// exists while schema_migrations is empty, the schema predates version tracking.
	bootstrapCoreTable = "nodes"

	versionTable = "schema_migrations"

	// The session-level advisory lock serializes migrate runs across processes. The timeout
	// bounds the wait for the lock (and for any lock a migration statement takes), never the
	// work itself: a migration is allowed to run for as long as its statements need.
	migrateLockKey     = "eam-migrate-v1"
	migrateLockTimeout = 10 * time.Second
)

// versionTableDDL mirrors sql/044_schema_migrations.sql. The runner must create the table
// before it can record anything, including the row for 044 itself, so the two definitions
// have to stay identical; TestVersionTableDDLMatchesMigration enforces that.
const versionTableDDL = `create table if not exists schema_migrations (
  name       text primary key,
  applied_at timestamptz not null default now(),
  method     text not null check (method in ('executed', 'baseline')),
  reason     text,
  check (method <> 'baseline' or reason is not null)
)`

// bootstrapReason is stored on every adopted row. It is the answer to the question a later
// reader will ask first: "was this file run against this database, or inferred?"
const bootstrapReason = "adopted without execution: it predates schema_migrations, which was introduced after the old runner had replayed every sql/*.sql on every deploy"

type appliedMigration struct {
	method    string
	appliedAt time.Time
}

// findSQLDir works from either the repo root or the backend/ directory.
func findSQLDir() (string, error) {
	if dir := os.Getenv("EAM_SQL_DIR"); dir != "" {
		return dir, nil
	}
	for _, candidate := range []string{"sql", filepath.Join("..", "sql"), filepath.Join("..", "..", "sql")} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("sql/ directory not found; set EAM_SQL_DIR")
}

// migrationFiles returns the .sql files in dir in filename order. The version-table
// migration must be present: without it there is no boundary between "already replayed by
// the old runner" and "new", so the runner could neither bootstrap nor prove that its
// history is complete.
func migrationFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no .sql files in %s", dir)
	}
	for _, file := range files {
		if file == versionTableMigration {
			return files, nil
		}
	}
	return nil, fmt.Errorf("%s is missing from %s: eam migrate needs it to record which files have been applied "+
		"(EAM_SQL_DIR must name the migrations directory)", versionTableMigration, dir)
}

func runMigrate(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("migrate takes no arguments: %v", args)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := findSQLDir()
	if err != nil {
		return err
	}
	files, err := migrationFiles(dir)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	// The advisory lock is session-scoped, so it needs one dedicated connection for the
	// whole run. The pool holds 40; a migrate run uses exactly this one.
	conn, err := db.Pool().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("failed to acquire a database connection: %w", err)
	}
	defer conn.Release()

	return runMigrationSession(ctx, conn, dir, files, os.Stdout)
}

// runMigrationSession applies every file in files that schema_migrations does not already
// record, each file and its history row committing together so a failure leaves the file
// unrecorded (and its statements rolled back).
//
// Bootstrap rule: `eam migrate` used to replay every sql/*.sql on every invocation. On the
// first run against such a database schema_migrations is empty (or was created empty by this
// run) while the core `nodes` table from 001_init.sql already exists. Those older files must
// not run again: 035 creates a globally unique index on storage_connections(name), 036 drops
// it and replaces it with per-owner indexes, and replaying 035 fails as soon as two owners
// both have a connection named "nas" -- which is why the version table exists at all. The
// old runner applied all files in one transaction, so a database migrated by a release
// containing 001-043 is complete; one last migrated by an older release could be behind, and
// this history-less rule cannot detect that. The database carries no per-file history to
// recover, so every file that sorts before versionTableMigration is recorded as
// method='baseline' without being executed, with the reason written in `reason`. A genuinely
// empty database has no `nodes` table, so every file executes in order and is recorded as
// method='executed'. Once any row exists, the table is the authority: only names without a
// row run.
func runMigrationSession(ctx context.Context, conn *pgxpool.Conn, dir string, files []string, out io.Writer) error {
	// lock_timeout bounds every lock this session waits for: the advisory lock below and
	// object locks taken by migration DDL. A migration blocked behind a running query
	// therefore fails with a named timeout instead of hanging; the statements themselves
	// are still free to take as long as their work needs.
	if _, err := conn.Exec(ctx, `select set_config('lock_timeout', $1, false)`, migrateLockTimeout.String()); err != nil {
		return fmt.Errorf("could not set lock_timeout to %s for the migration session: %w", migrateLockTimeout, err)
	}

	if _, err := conn.Exec(ctx, `select pg_advisory_lock(hashtextextended($1, 0))`, migrateLockKey); err != nil {
		if isLockTimeout(err) {
			return fmt.Errorf("timed out after %s waiting for the %q session advisory lock: another `eam migrate` "+
				"(or an interrupted one) is running against this database; wait for it to finish and retry: %w", migrateLockTimeout, migrateLockKey, err)
		}
		return fmt.Errorf("could not acquire the %q session advisory lock: %w", migrateLockKey, err)
	}
	defer func() {
		// A failed unlock is not fatal: closing the pooled connection releases a session
		// advisory lock too, but say so because the next run may otherwise wait.
		unlockCtx, cancel := context.WithTimeout(context.Background(), migrateLockTimeout)
		defer cancel()
		if _, unlockErr := conn.Exec(unlockCtx, `select pg_advisory_unlock(hashtextextended($1, 0))`, migrateLockKey); unlockErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not release the %q session advisory lock: %v\n", migrateLockKey, unlockErr)
		}
	}()

	created, err := ensureVersionTable(ctx, conn)
	if err != nil {
		return err
	}
	applied, err := loadAppliedMigrations(ctx, conn)
	if err != nil {
		return err
	}

	var corePresent bool
	if len(applied) == 0 {
		if err := conn.QueryRow(ctx, `select to_regclass($1::text) is not null`, bootstrapCoreTable).Scan(&corePresent); err != nil {
			return fmt.Errorf("could not check for the %q table: %w", bootstrapCoreTable, err)
		}
	}
	bootstrap := shouldBootstrap(len(applied) == 0, corePresent)

	baseline := map[string]bool{}
	if bootstrap {
		for _, file := range bootstrapRange(files) {
			baseline[file] = true
		}
		if len(baseline) > 0 {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return fmt.Errorf("could not start the bootstrap transaction: %w", err)
			}
			for _, file := range files {
				if !baseline[file] {
					continue
				}
				// on conflict keeps this honest if the table already held a row somehow;
				// the table was empty by definition, so it is only defensive.
				if _, err := tx.Exec(ctx, `insert into schema_migrations(name, method, reason) values ($1, 'baseline', $2) on conflict (name) do nothing`, file, bootstrapReason); err != nil {
					_ = tx.Rollback(ctx)
					return fmt.Errorf("could not record %s as adopted during bootstrap: %w", file, err)
				}
			}
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("could not commit the bootstrap records: %w", err)
			}
		}
	}

	var appliedCount, skippedCount, baselineCount int
	for _, file := range files {
		if record, ok := applied[file]; ok {
			fmt.Fprintf(out, "→ %s SKIP (applied %s)\n", file, record.appliedAt.UTC().Format(time.RFC3339))
			skippedCount++
			continue
		}
		if baseline[file] {
			fmt.Fprintf(out, "→ %s BASELINE (adopted as applied; not executed)\n", file)
			baselineCount++
			skippedCount++
			continue
		}

		fmt.Fprintf(out, "→ %s ", file)
		content, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			fmt.Fprintln(out, "FAIL")
			return fmt.Errorf("read %s: %w", filepath.Join(dir, file), err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			fmt.Fprintln(out, "FAIL")
			return fmt.Errorf("%s: could not begin transaction: %w", file, err)
		}
		// with no arguments pgx uses the simple protocol, so several statements run at once
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			_ = tx.Rollback(ctx)
			fmt.Fprintln(out, "FAIL")
			return migrationExecError(file, err)
		}
		if _, err := tx.Exec(ctx, `insert into schema_migrations(name, method) values ($1, 'executed')`, file); err != nil {
			_ = tx.Rollback(ctx)
			fmt.Fprintln(out, "FAIL")
			return migrationExecError(file, fmt.Errorf("could not record the migration, transaction rolled back: %w", err))
		}
		if err := tx.Commit(ctx); err != nil {
			fmt.Fprintln(out, "FAIL")
			return fmt.Errorf("%s: could not commit: %w", file, err)
		}
		fmt.Fprintln(out, "OK")
		appliedCount++
	}

	if bootstrap {
		createdNote := "was empty"
		if created {
			createdNote = "was created empty"
		}
		fmt.Fprintf(out, "\nbootstrap: %s %s while the %q table already existed, so %d migration(s) older than %s "+
			"were recorded as applied (method='baseline') without being executed; the pre-versioning runner "+
			"had already replayed them.\n", versionTable, createdNote, bootstrapCoreTable, baselineCount, versionTableMigration)
	}
	if appliedCount == 0 {
		fmt.Fprintf(out, "\nnothing to apply; %d migration(s) already recorded.\n", skippedCount)
	} else {
		fmt.Fprintf(out, "\napplied %d migration(s); %d already recorded.\n", appliedCount, skippedCount)
	}
	return nil
}

// ensureVersionTable creates schema_migrations when it is missing and reports whether this
// run created it. Creation goes through the runner rather than the loop because the table
// has to exist before any file can be recorded, including versionTableMigration itself.
func ensureVersionTable(ctx context.Context, conn *pgxpool.Conn) (bool, error) {
	var exists bool
	if err := conn.QueryRow(ctx, `select to_regclass($1::text) is not null`, versionTable).Scan(&exists); err != nil {
		return false, fmt.Errorf("could not check for the %s table: %w", versionTable, err)
	}
	if exists {
		return false, nil
	}
	if _, err := conn.Exec(ctx, versionTableDDL); err != nil {
		return false, fmt.Errorf("could not create %s: %w", versionTable, err)
	}
	return true, nil
}

func loadAppliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[string]appliedMigration, error) {
	rows, err := conn.Query(ctx, `select name, method, applied_at from schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", versionTable, err)
	}
	defer rows.Close()
	applied := map[string]appliedMigration{}
	for rows.Next() {
		var name string
		var record appliedMigration
		if err := rows.Scan(&name, &record.method, &record.appliedAt); err != nil {
			return nil, fmt.Errorf("could not read %s: %w", versionTable, err)
		}
		applied[name] = record
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("could not read %s: %w", versionTable, err)
	}
	return applied, nil
}

// shouldBootstrap is the whole bootstrap rule in one place: an empty history plus a schema
// that already exists means the database predates version tracking. The empty table on a
// genuinely new database is not enough, because there is nothing to adopt.
func shouldBootstrap(versionTableEmpty, coreTablePresent bool) bool {
	return versionTableEmpty && coreTablePresent
}

// bootstrapRange returns the files that sort before versionTableMigration. files must be
// sorted by name (migrationFiles guarantees it) so this is exactly the set the old replay
// runner had applied before the version table was introduced.
func bootstrapRange(files []string) []string {
	for i, file := range files {
		if file >= versionTableMigration {
			return files[:i]
		}
	}
	return files
}

// isLockTimeout recognizes Postgres lock_timeout and lock-acquisition cancellation.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

// migrationExecError names the file and, for a lock timeout, the mechanism behind it so a
// blocked deploy says what it was waiting for instead of only "canceling statement".
func migrationExecError(file string, err error) error {
	if isLockTimeout(err) {
		return fmt.Errorf("%s: lock timeout after %s: another session holds a conflicting lock; this run holds "+
			"the %q session advisory lock and sets lock_timeout so a blocked migration fails instead of hanging: %w", file, migrateLockTimeout, migrateLockKey, err)
	}
	return fmt.Errorf("%s: %w", file, err)
}
