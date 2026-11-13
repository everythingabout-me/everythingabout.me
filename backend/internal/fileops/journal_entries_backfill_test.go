package fileops

// The backfill (sql/050) is the data half of Release A: it copies every retained operation's
// document entries into file_operation_entries and sets entry_count, without touching the
// documents. These tests hold it to the three properties the release depends on: it is
// idempotent, it can be interrupted without half-writing a row, and it leaves a shadow that
// matches the document.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// entriesLayoutDocument is the layout every row has in Release A (sql/049's default). The
// constant is test-only on purpose: no Release A production path may branch on the layout yet.
const entriesLayoutDocument = "document"

func readBackfillSQL(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "sql", "050_file_operation_entries_backfill.sql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// legacyOperation prepares a frozen move and then rewrites the row into the shape a
// pre-Release-A binary leaves: the document carries every entry, the table has none, and
// entry_count is still its default. That is the row the backfill exists for — the same shape a
// release-44 database has after sql/049 runs but before sql/050 does.
func legacyOperation(t *testing.T, pool *pgxpool.Pool, p *Postgres, r Root, actor Actor, files int) Operation {
	t.Helper()
	ctx := context.Background()
	n := costTree(r, files)
	a := actionCoordinator(t, p, nil)
	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare: replay=%v err=%v", replay, err)
	}
	if len(op.Entries) == 0 {
		t.Fatal("the frozen manifest is empty")
	}
	// Remove the shadow this binary wrote at freeze and reset the count: from here the row is
	// indistinguishable from one sql/049 just added columns to.
	if _, err := pool.Exec(ctx, `delete from file_operation_entries where operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update file_operations set entry_count=0 where id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	return op
}

// TestJournalEntriesBackfill fills a legacy row, checks the result field for field against the
// operation document, and checks that a second run changes nothing.
func TestJournalEntriesBackfill(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	op := legacyOperation(t, pool, p, r, actor, 3)
	backfill := readBackfillSQL(t)

	var before string
	if err := pool.QueryRow(ctx, `select snapshot::text from file_operations where id=$1`, op.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, backfill); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("after backfill: %s", got)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from file_operation_entries where operation_id=$1`, op.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(op.Entries) {
		t.Fatalf("backfill moved %d of %d entries", count, len(op.Entries))
	}
	// Release A is rollback-safe because the backfill never rewrites a document and never
	// flips entries_layout.
	var after, layout string
	if err := pool.QueryRow(ctx, `select snapshot::text, entries_layout from file_operations where id=$1`, op.ID).Scan(&after, &layout); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("the backfill rewrote the operation document; Release A must leave documents untouched")
	}
	if layout != entriesLayoutDocument {
		t.Fatalf("the backfill set entries_layout=%q; Release A never flips it", layout)
	}

	// Idempotent: a second run inserts nothing and touches nothing.
	var firstStamp string
	if err := pool.QueryRow(ctx, `select max(updated_at)::text from file_operation_entries where operation_id=$1`, op.ID).Scan(&firstStamp); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, backfill); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	var secondStamp string
	if err := pool.QueryRow(ctx, `select max(updated_at)::text from file_operation_entries where operation_id=$1`, op.ID).Scan(&secondStamp); err != nil {
		t.Fatal(err)
	}
	if secondStamp != firstStamp {
		t.Fatalf("the second run rewrote a shadow row (%s -> %s); on conflict do nothing must make it a no-op", firstStamp, secondStamp)
	}
}

// TestJournalEntriesBackfillInterruption runs the backfill inside a transaction and aborts it.
// The migration runner applies each file in one transaction (sql/044), so this is exactly what
// an interrupted `eam migrate` leaves: the schema from sql/049 is in place, no shadow row and no
// entry_count survived, and the next run starts from the same state. Nothing is half-written.
func TestJournalEntriesBackfillInterruption(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	op := legacyOperation(t, pool, p, r, actor, 2)
	backfill := readBackfillSQL(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, backfill); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("backfill inside the transaction: %v", err)
	}
	var inside int
	if err := tx.QueryRow(ctx, `select count(*) from file_operation_entries where operation_id=$1`, op.ID).Scan(&inside); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if inside != len(op.Entries) {
		_ = tx.Rollback(ctx)
		t.Fatalf("inside the transaction the backfill moved %d of %d entries", inside, len(op.Entries))
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	var rows, entryCount int
	if err := pool.QueryRow(ctx, `select (select count(*) from file_operation_entries where operation_id=$1), entry_count
		from file_operations where id=$1`, op.ID).Scan(&rows, &entryCount); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || entryCount != 0 {
		t.Fatalf("after the aborted run: %d shadow row(s), entry_count=%d; an interrupted migration must leave both at zero", rows, entryCount)
	}
	// And the restart works: the same file, run again, completes.
	if _, err := pool.Exec(ctx, backfill); err != nil {
		t.Fatalf("restarted backfill: %v", err)
	}
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("after the restarted backfill: %s", got)
	}
}
