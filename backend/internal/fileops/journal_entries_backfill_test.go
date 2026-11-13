package fileops

// The backfill (sql/050) is the data half of the two-release migration: it copies every
// retained operation's document entries into file_operation_entries and sets entry_count,
// without touching the documents or flipping entries_layout. Release B still keeps it,
// because a database upgraded from Release A may hold rows this binary never claimed:
// terminal ones that will be pruned with their document, and live ones that Release B's
// Claim-time upgrade would otherwise have to repair on the next attempt.
//
// These tests hold the file to the properties the release depends on: it is idempotent,
// it can be interrupted without half-writing a row, and what it writes matches the document
// field for field.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
// pre-Release-B binary leaves: the document carries every entry, the table has none, and
// entry_count is still its default. That is the row the backfill and Release B's Claim-time
// upgrade exist for. The document's spelling is produced by marshalling the same
// []EntryEffect the transition wrote, so the fixture cannot drift from the real shape.
func legacyOperation(t *testing.T, pool *pgxpool.Pool, p *Postgres, r Root, actor Actor, files int) Operation {
	t.Helper()
	ctx := context.Background()
	n := costTree(r, files)
	a := actionCoordinator(t, p, nil)
	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare: replay=%v err=%v", replay, err)
	}
	entries, err := p.Entries(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the frozen manifest is empty")
	}
	document, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `delete from file_operation_entries where operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update file_operations
		set entry_count=0, entries_layout='document', snapshot=jsonb_set(snapshot,'{Entries}',$2::jsonb)
		where id=$1`, op.ID, string(document)); err != nil {
		t.Fatal(err)
	}
	return op
}

// shadowVerifySQL rebuilds every entry row as the document's entry object and asks SQL whether
// the two are `is distinct from` each other. jsonb equality compares every field, including the
// nested source entry and result documents, without the test having to decode them.
//
// It runs against the document, which is the authority while a row is 'document'. Every column
// of the table is present in the reconstruction, so a missing or renamed column shows up as a
// difference; the fallback reproduces the document's own spelling of "no entries" so a row with
// no table rows compares equal to an operation with no entries.
//
// Binary columns are spelled as base64 because that is how the Go document spells []byte.
// Postgres's encode(..., 'base64') wraps at 76 characters and Go's does not, so the line breaks
// are stripped before comparing: the bytes are the same either way, and a comparison that
// differed only in line wrapping would report a disagreement that is not one.
const shadowVerifySQL = `
select coalesce(
         (select jsonb_agg(jsonb_build_object(
              'Number', e.number,
              'Action', e.action,
              'Source', e.source_path,
              'Target', e.target_path,
              'SourceRevision', case when e.source_revision is null then null
                                     else translate(encode(e.source_revision, 'base64'), E'\n', '') end,
              'SourceEntry', e.source_entry,
              -- ImplicitFolder has no column: it is only valid for a Remove and is false
              -- for every frozen manifest entry. The document still spells it, so the
              -- reconstruction must spell it too.
              'TargetConditions', jsonb_build_object(
                  'Absent', e.target_absent,
                  'ExpectedRevision', case when e.target_revision is null then null
                                           else translate(encode(e.target_revision, 'base64'), E'\n', '') end,
                  'ImplicitFolder', false),
              'Size', e.size,
              'State', e.state,
              'Checkpoint', case when e.checkpoint is null then null
                                 else translate(encode(e.checkpoint, 'base64'), E'\n', '') end,
              'Evidence', case when e.evidence is null then null
                               else translate(encode(e.evidence, 'base64'), E'\n', '') end,
              'Result', e.result) order by e.number)
            from file_operation_entries e where e.operation_id = o.id),
         case when jsonb_typeof(o.snapshot->'Entries') = 'array' then '[]'::jsonb
              else 'null'::jsonb end)
       is distinct from coalesce(o.snapshot->'Entries', 'null'::jsonb) as entries_differ,
       o.entry_count,
       (select count(*) from file_operation_entries e where e.operation_id = o.id) as table_rows,
       (select count(*) from file_operation_entries e
         where e.operation_id = o.id and e.owner_id <> o.owner_id) as foreign_rows
  from file_operations o where o.id = $1`

// shadowMismatch returns a description of the first disagreement between the table and the
// operation document, or "" when the copy is exact.
func shadowMismatch(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var entriesDiffer bool
	var entryCount, tableRows, foreignRows int
	err := pool.QueryRow(context.Background(), shadowVerifySQL, id).
		Scan(&entriesDiffer, &entryCount, &tableRows, &foreignRows)
	if err != nil {
		t.Fatalf("read entries of operation %s: %v", id, err)
	}
	switch {
	case foreignRows != 0:
		return "entries name a different owner"
	case entriesDiffer:
		return "table entries differ from the document's Entries"
	case entryCount != tableRows:
		return "entry_count disagrees with the table row count"
	}
	return ""
}

// TestJournalEntriesBackfill fills a legacy row, checks the result field for field against
// the operation document, and checks that a second run changes nothing.
func TestJournalEntriesBackfill(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	op := legacyOperation(t, pool, p, r, actor, 3)
	want := op.EntryTotal()
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
	if count := countEntryRows(t, pool, op.ID); count != want {
		t.Fatalf("backfill moved %d of %d entries", count, want)
	}
	// The backfill is still read-only with respect to documents and never flips the layout:
	// that is what lets an interrupted upgrade be re-run before Release B's writers reach
	// the row.
	var after, layout string
	if err := pool.QueryRow(ctx, `select snapshot::text, entries_layout from file_operations where id=$1`, op.ID).Scan(&after, &layout); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("the backfill rewrote the operation document; it must leave documents untouched")
	}
	if layout != "document" {
		t.Fatalf("the backfill set entries_layout=%q; only Claim flips it during normal operation", layout)
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
		t.Fatalf("the second run rewrote an entry row (%s -> %s); on conflict do nothing must make it a no-op", firstStamp, secondStamp)
	}
}

// TestJournalEntriesBackfillInterruption runs the backfill inside a transaction and aborts it.
// The migration runner applies each file in one transaction (sql/044), so this is exactly what
// an interrupted `eam migrate` leaves: no row and no entry_count survived, and the next run
// starts from the same state. Nothing is half-written.
func TestJournalEntriesBackfillInterruption(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	op := legacyOperation(t, pool, p, r, actor, 2)
	want := op.EntryTotal()
	backfill := readBackfillSQL(t)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, backfill); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("backfill inside the transaction: %v", err)
	}
	if inside := countEntryRowsTx(t, tx, op.ID); inside != want {
		_ = tx.Rollback(ctx)
		t.Fatalf("inside the transaction the backfill moved %d of %d entries", inside, want)
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
		t.Fatalf("after the aborted run: %d entry row(s), entry_count=%d; an interrupted migration must leave both at zero", rows, entryCount)
	}
	// And the restart works: the same file, run again, completes.
	if _, err := pool.Exec(ctx, backfill); err != nil {
		t.Fatalf("restarted backfill: %v", err)
	}
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("after the restarted backfill: %s", got)
	}
}

func countEntryRowsTx(t *testing.T, tx pgx.Tx, id string) int {
	t.Helper()
	var count int
	if err := tx.QueryRow(context.Background(),
		`select count(*) from file_operation_entries where operation_id=$1`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
