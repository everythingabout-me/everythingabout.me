package fileops

// Shadow verification for Release A of docs/design/journal-entries.md.
//
// The contract these tests defend: while a row is entries_layout='document', the operation
// document is authoritative and complete, and file_operation_entries is a copy that must match
// it field for field. Every shadow write happens in the transaction that writes the document
// (postgres_entries.go says why), so the two cannot disagree through a crash or a failed
// statement — and these tests prove it on the real Prepare + Run path, not on a hand-written
// fixture row.

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// shadowVerifySQL rebuilds every entry row as the document's entry object and asks SQL whether
// the two are `is distinct from` each other. jsonb equality compares every field, including the
// nested source entry and result documents, without the test having to decode them.
//
// It runs against the document, which is the authority for a 'document' row. Every column of
// the table is present in the reconstruction, so a missing or renamed column shows up as a
// difference; the fallback reproduces the document's own spelling of "no entries" (JSON null
// for a nil slice, an empty array for an empty one) so a row with no table rows compares equal
// to an operation with no entries.
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
              -- for every frozen manifest entry (validateFrozenAction builds each entry with
              -- just Absent/ExpectedRevision, and a manifest that set it could never equal
              -- that expected set). The document still spells it, so the reconstruction
              -- must spell it too.
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
// operation document, or "" when the shadow is exact.
func shadowMismatch(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var entriesDiffer bool
	var entryCount, tableRows, foreignRows int
	err := pool.QueryRow(context.Background(), shadowVerifySQL, id).
		Scan(&entriesDiffer, &entryCount, &tableRows, &foreignRows)
	if err != nil {
		t.Fatalf("read shadow of operation %s: %v", id, err)
	}
	switch {
	case foreignRows != 0:
		return fmt.Sprintf("%d entry row(s) name a different owner", foreignRows)
	case entriesDiffer:
		return fmt.Sprintf("table entries differ from snapshot->'Entries' (%d rows)", tableRows)
	case entryCount != tableRows:
		return fmt.Sprintf("entry_count=%d but the table holds %d rows", entryCount, tableRows)
	}
	return ""
}

// TestShadowEntriesMatchDocument drives a real move through the production path and compares
// the shadow with the document after the manifest freezes and after settlement. It is the
// detector for a write path that mutates an entry without shadowing it: the two moments cover
// all five transitions (Claim, BeginEntry, the started checkpoint, SaveEntry applied, Release),
// because the comparison after Run sees the state each one left behind.
func TestShadowEntriesMatchDocument(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, costBaseline())
	n := costTree(r, 4)
	a := actionCoordinator(t, p, nil)

	op, replay, err := a.Prepare(ctx, moveCostRequest(r, actor), n)
	if err != nil || replay {
		t.Fatalf("prepare: replay=%v err=%v", replay, err)
	}
	if len(op.Entries) == 0 {
		t.Fatal("the frozen manifest is empty; the fixture did not build a move")
	}
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("after freeze: %s", got)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from file_operation_entries where operation_id=$1`, op.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(op.Entries) {
		t.Fatalf("freeze shadowed %d of %d entries", count, len(op.Entries))
	}

	moved, err := a.Run(ctx, r, actor, op.ID, n)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if moved.State != Succeeded {
		t.Fatalf("move settled %s: %s", moved.State, moved.Problem)
	}
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("after settlement: %s", got)
	}

	// The detector must be able to fail: corrupt one shadowed row and require a mismatch.
	if _, err := pool.Exec(ctx, `update file_operation_entries set state='started'
		where operation_id=$1 and number=0 and state='applied'`, op.ID); err != nil {
		t.Fatal(err)
	}
	if got := shadowMismatch(t, pool, op.ID); got == "" {
		t.Fatal("a corrupted shadow row was not detected")
	}
}

// TestShadowEntryAndDocumentWriteCommitTogether proves the pairing in the direction a crash
// would hurt: the shadow write is issued first, then the document update, and if the document
// update fails the whole transaction must roll back so the table does not run ahead.
//
// The failure is injected with a trigger on file_operations, so the code under test is the real
// `leased` path (parent row lock, BeginEntry, shadow write, saveOperation). A shadow write on a
// second connection, or one committed before the document, would survive the rollback and be
// visible as a 'started' row while the document still says 'pending'.
func TestShadowEntryAndDocumentWriteCommitTogether(t *testing.T) {
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

	// Fail every update of this operation's document row.
	failing := fmt.Sprintf(`create or replace function shadow_atomicity_fail() returns trigger
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
	if _, err := pool.Exec(ctx, `create trigger shadow_atomicity_fail before update on file_operations
		for each row execute function shadow_atomicity_fail()`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `drop trigger if exists shadow_atomicity_fail on file_operations`)
		_, _ = pool.Exec(context.Background(), `drop function if exists shadow_atomicity_fail()`)
	}()

	if err := p.BeginEntry(ctx, claimed.Lease, 0); err == nil {
		t.Fatal("BeginEntry succeeded although the document write was made to fail")
	}
	// The injected failure must not have left the table ahead of the document.
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("a failed document write left the shadow ahead: %s", got)
	}
	var docState, rowState string
	if err := pool.QueryRow(ctx, `select snapshot->'Entries'->0->>'State' from file_operations where id=$1`, op.ID).Scan(&docState); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select state from file_operation_entries where operation_id=$1 and number=0`, op.ID).Scan(&rowState); err != nil {
		t.Fatal(err)
	}
	if docState != "pending" || rowState != "pending" {
		t.Fatalf("after the rolled-back transition: document entry is %q, shadow row is %q; both must still be pending", docState, rowState)
	}

	// The lease was not consumed by the rolled-back transaction: the operation is still usable.
	if _, err := pool.Exec(ctx, `drop trigger if exists shadow_atomicity_fail on file_operations`); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginEntry(ctx, claimed.Lease, 0); err != nil {
		t.Fatalf("BeginEntry after the failed attempt: %v", err)
	}
	if got := shadowMismatch(t, pool, op.ID); got != "" {
		t.Fatalf("after the retried transition: %s", got)
	}
}
