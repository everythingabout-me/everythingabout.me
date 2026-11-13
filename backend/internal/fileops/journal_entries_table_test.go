package fileops

// Release B of docs/design/journal-entries.md: the rows in file_operation_entries are the
// operation's per-entry record, the document carries none, and an operation written before
// the flip is upgraded on Claim. These tests drive the real Prepare + Run path (only the
// native tree is in memory) so the transitions under test are the production ones.

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// documentCarriesEntries reports whether the operation document still spells an Entries
// key. `?` is the jsonb key-existence operator, which is what "carries none" means after
// the flip; the document is not parsed here.
func documentCarriesEntries(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(),
		`select snapshot ? 'Entries' from file_operations where id=$1`, id).Scan(&present); err != nil {
		t.Fatalf("read document shape of %s: %v", id, err)
	}
	return present
}

func countEntryRows(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from file_operation_entries where operation_id=$1`, id).Scan(&count); err != nil {
		t.Fatalf("count entries of %s: %v", id, err)
	}
	return count
}

func readLayout(t *testing.T, pool *pgxpool.Pool, id string) (string, int) {
	t.Helper()
	var layout string
	var count int
	if err := pool.QueryRow(context.Background(),
		`select entries_layout, entry_count from file_operations where id=$1`, id).Scan(&layout, &count); err != nil {
		t.Fatalf("read layout of %s: %v", id, err)
	}
	return layout, count
}

// TestTableBackedOperationKeepsEntriesOutOfTheDocument is the flip itself: after Prepare
// the document has no Entries key, entry_count and the rows agree, and a Get returns no
// list (a reader that needs one asks). After Run every row is applied and the document
// still carries none, which is what makes the per-transition cost one row instead of the
// whole manifest.
func TestTableBackedOperationKeepsEntriesOutOfTheDocument(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	n := costTree(r, 4)
	a := actionCoordinator(t, p, nil)

	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare: replay=%v err=%v", replay, err)
	}
	if op.EntriesLayout != EntriesLayoutTable {
		t.Fatalf("a new operation was written with entries_layout=%q", op.EntriesLayout)
	}
	if op.EntryCount == 0 {
		t.Fatal("the frozen manifest has no entry count")
	}
	if len(op.Entries) != 0 {
		t.Fatalf("Get returned %d entries for a table-backed operation; a read must not load the manifest", len(op.Entries))
	}
	if layout, count := readLayout(t, pool, op.ID); layout != "table" || count != op.EntryCount {
		t.Fatalf("parent row is layout=%q count=%d, want table/%d", layout, count, op.EntryCount)
	}
	if count := countEntryRows(t, pool, op.ID); count != op.EntryCount {
		t.Fatalf("the table holds %d rows, entry_count says %d", count, op.EntryCount)
	}
	if documentCarriesEntries(t, pool, op.ID) {
		t.Fatal("the frozen document still carries an Entries key; the list lives in the table now")
	}
	entries, err := p.Entries(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != op.EntryCount {
		t.Fatalf("loaded %d of %d entries", len(entries), op.EntryCount)
	}
	for _, entry := range entries {
		if entry.State != "pending" {
			t.Fatalf("a freshly frozen entry is %q, want pending", entry.State)
		}
	}

	moved, err := a.Run(ctx, r, actor, op.ID, n)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if moved.State != Succeeded {
		t.Fatalf("move settled %s: %s", moved.State, moved.Problem)
	}
	if documentCarriesEntries(t, pool, op.ID) {
		t.Fatal("settlement put the entry list back into the document")
	}
	if layout, count := readLayout(t, pool, op.ID); layout != "table" || count != op.EntryCount {
		t.Fatalf("after settlement parent is layout=%q count=%d, want table/%d", layout, count, op.EntryCount)
	}
	entries, err = p.Entries(ctx, moved)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.State != "applied" {
			t.Fatalf("settled entry %d is %q, want applied", entry.Number, entry.State)
		}
	}
	completed, err := p.EntryProgress(ctx, moved)
	if err != nil || completed != len(entries) {
		t.Fatalf("progress = %d/%d, %v", completed, len(entries), err)
	}
}

// TestBeginEntryRowAndDocumentWriteCommitTogether proves the direction a crash would hurt:
// the entry row update and the parent document write are one transaction, so a failure of
// the document write rolls the row back too. The failure is injected with a trigger on
// file_operations, so the code under test is the real leased path (parent row lock,
// BeginEntry row update, saveOperation).
func TestBeginEntryRowAndDocumentWriteCommitTogether(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	n := costTree(r, 2)
	a := actionCoordinator(t, p, nil)

	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare: replay=%v err=%v", replay, err)
	}
	claimed := mustClaim(t, p, op, actor)

	failing := fmt.Sprintf(`create or replace function journal_atomicity_fail() returns trigger
		language plpgsql as $$
		begin
			if new.id = '%s'::uuid then
				raise exception 'injected document write failure';
			end if;
			return new;
		end $$`, op.ID)
	if _, err := pool.Exec(ctx, failing); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `create trigger journal_atomicity_fail before update on file_operations
		for each row execute function journal_atomicity_fail()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `drop trigger if exists journal_atomicity_fail on file_operations`)
		_, _ = pool.Exec(context.Background(), `drop function if exists journal_atomicity_fail()`)
	}()

	if err := p.BeginEntry(ctx, claimed.Lease, 0); err == nil {
		t.Fatal("BeginEntry succeeded although the document write was made to fail")
	}
	var rowState string
	if err := pool.QueryRow(ctx, `select state from file_operation_entries where operation_id=$1 and number=0`, op.ID).Scan(&rowState); err != nil {
		t.Fatal(err)
	}
	if rowState != "pending" {
		t.Fatalf("after the rolled-back transition the entry row is %q; the row write must commit with the document or not at all", rowState)
	}

	// The lease was not consumed by the rolled-back transaction: the operation is still usable.
	if _, err := pool.Exec(ctx, `drop trigger if exists journal_atomicity_fail on file_operations`); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginEntry(ctx, claimed.Lease, 0); err != nil {
		t.Fatalf("BeginEntry after the failed attempt: %v", err)
	}
	if err := pool.QueryRow(ctx, `select state from file_operation_entries where operation_id=$1 and number=0`, op.ID).Scan(&rowState); err != nil {
		t.Fatal(err)
	}
	if rowState != "started" {
		t.Fatalf("after the retried transition the entry row is %q, want started", rowState)
	}
}

// TestClaimUpgradesDocumentBackedOperation is the upgrade path: an operation written before
// Release B (or by the previous binary during the deploy window) has its entries in the
// document and no rows. Claim must move them into the table, flip the layout, and strip the
// document, all in its own transaction, so the operation can keep settling.
func TestClaimUpgradesDocumentBackedOperation(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	op := legacyOperation(t, pool, p, r, actor, 3)
	want := op.EntryTotal()
	if layout, _ := readLayout(t, pool, op.ID); layout != "document" {
		t.Fatalf("the legacy fixture is layout=%q, want document", layout)
	}
	if !documentCarriesEntries(t, pool, op.ID) {
		t.Fatal("the legacy fixture must carry its entries in the document")
	}
	if count := countEntryRows(t, pool, op.ID); count != 0 {
		t.Fatalf("the legacy fixture must start without rows, has %d", count)
	}

	claimed := mustClaim(t, p, op, actor)
	if claimed.EntriesLayout != EntriesLayoutTable || claimed.EntryCount != want {
		t.Fatalf("Claim left the operation layout=%q count=%d, want table/%d", claimed.EntriesLayout, claimed.EntryCount, want)
	}
	if len(claimed.Entries) != 0 {
		t.Fatalf("Claim returned %d entries; the upgrade moved them into the table", len(claimed.Entries))
	}
	if layout, count := readLayout(t, pool, op.ID); layout != "table" || count != want {
		t.Fatalf("parent row after upgrade is layout=%q count=%d, want table/%d", layout, count, want)
	}
	if documentCarriesEntries(t, pool, op.ID) {
		t.Fatal("the upgrade left the entry list in the document")
	}
	entries, err := p.Entries(ctx, claimed)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Fatalf("upgrade moved %d of %d entries", len(entries), want)
	}
	// The upgraded row must be transitionable exactly like one this binary wrote.
	if err := p.BeginEntry(ctx, claimed.Lease, 0); err != nil {
		t.Fatalf("BeginEntry on the upgraded operation: %v", err)
	}
}
