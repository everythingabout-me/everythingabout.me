package fileops

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertJournalRow writes a row the way beginTx does: the document and the promoted columns
// from one Operation, through the same statement. Tests that need a specific journal shape
// use this instead of a hand-written insert, so they cannot accidentally build the
// document-only row that sql/047 exists to eliminate.
func insertJournalRow(t *testing.T, pool *pgxpool.Pool, op Operation) {
	t.Helper()
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	args := operationArgs(op, b)
	args["owner_id"] = op.Root.OwnerID
	args["principal_id"] = op.PrincipalID
	args["action"] = op.Kind
	args["idempotency_key"] = nil
	args["fingerprint"] = "prune-test"
	args["entry_count"] = len(op.Entries)
	if _, err := pool.Exec(context.Background(), insertOperationSQL, args); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresPruneExpiredDeletesOnlyUnreferencedSettledOperations pins the rule the prune
// documents, because it is the one command here that deletes rows: every clause that keeps a
// row must be exercised, or a future simplification silently drops recovery evidence.
func TestPostgresPruneExpiredDeletesOnlyUnreferencedSettledOperations(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 1000, 1000))
	now := time.Now().UTC().Truncate(time.Microsecond)
	settled := Operation{
		Attribution: a, Root: r, PrincipalID: a.ID, Kind: "move", State: Succeeded, Phase: Settled,
		CreatedAt: now.Add(-2 * time.Hour), FinishedAt: ptrTime(now.Add(-time.Hour)), RetainUntil: now.Add(-time.Hour),
	}
	insert := func(mutate func(*Operation)) string {
		t.Helper()
		op := settled
		op.ID = newID()
		if mutate != nil {
			mutate(&op)
		}
		insertJournalRow(t, pool, op)
		return op.ID
	}
	expired := insert(nil)
	future := insert(func(op *Operation) { op.RetainUntil = now.Add(time.Hour) })
	guarded := insert(nil)
	if _, err := pool.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree)
 values($1::uuid,$2::uuid,0,'kept','kept',false)`, guarded, r.OwnerID); err != nil {
		t.Fatal(err)
	}
	factored := insert(nil)
	if _, err := pool.Exec(ctx, `insert into file_change_effects(owner_id,effect_key,event_id,position,fingerprint)
 values($1::uuid,$2,gen_random_uuid(),1,'fixture')`, r.OwnerID, "prune-"+factored); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into file_changes(owner_id,position,effect_key,kind,coverage,actor,origin,operation_id)
 values($1::uuid,1,$2,'invalidated','entry','{}'::jsonb,'api',$3)`, r.OwnerID, "prune-"+factored, factored); err != nil {
		t.Fatal(err)
	}
	claimed := insert(nil)
	if _, err := pool.Exec(ctx, `insert into file_credit_claims(owner_id,comparison_path,version,operation_id)
 values($1::uuid,$2,decode('00','hex'),$3::uuid)`, r.OwnerID, "claim-"+claimed, claimed); err != nil {
		t.Fatal(err)
	}
	open := insert(func(op *Operation) { op.Kind = "upload_commit" })
	if _, err := pool.Exec(ctx, `insert into file_uploads(operation_id,owner_id,principal_id,client_fingerprint,comparison_path,
 part_size,part_count,state,created_at,expires_at)
 values($1::uuid,$2::uuid,'owner:test','fixture','upload/open',1,1,'open',$3,$3)`, open, r.OwnerID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	live := insert(func(op *Operation) { op.State = Running; op.Phase = EffectStarted; op.FinishedAt = nil })
	diverged := insert(nil)
	// The columns say failed while the document says succeeded: the row is repaired from the
	// document and kept this run, never deleted on the strength of a disagreeing column.
	if _, err := pool.Exec(ctx, `update file_operations set state='failed' where id=$1::uuid`, diverged); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{expired, guarded} {
		if _, err := pool.Exec(ctx, `insert into file_operation_checkpoints(operation_id,owner_id,entry_number,lease_epoch,checkpoint)
 values($1::uuid,$2::uuid,0,1,'x')`, id, r.OwnerID); err != nil {
			t.Fatal(err)
		}
	}

	dry, err := p.PruneExpired(ctx, r.OwnerID, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Deleted != 1 || dry.Repaired != 1 || dry.Skipped != 1 {
		t.Fatalf("dry run = %+v, want 1 deletable, 1 repaired and 1 skipped", dry)
	}
	still, err := p.PruneExpired(ctx, r.OwnerID, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if still.Deleted != 1 || still.Repaired != 1 || still.Skipped != 1 {
		t.Fatalf("run = %+v, want 1 deleted, 1 repaired and 1 skipped", still)
	}
	for name, id := range map[string]string{
		"retained future": future, "guarded": guarded, "change fact": factored,
		"credit claim": claimed, "open upload": open, "non-terminal": live, "repaired": diverged,
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `select exists(select 1 from file_operations where id=$1::uuid)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("prune deleted a %s operation", name)
		}
	}
	var exists bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from file_operations where id=$1::uuid)`, expired).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("prune kept an unreferenced settled operation")
	}
	// Child rows of a deleted operation go with it; the retained one keeps its own.
	for _, tc := range []struct {
		id   string
		want bool
	}{{expired, false}, {guarded, true}} {
		var checkpoint bool
		if err := pool.QueryRow(ctx, `select exists(select 1 from file_operation_checkpoints where operation_id=$1::uuid)`, tc.id).Scan(&checkpoint); err != nil {
			t.Fatal(err)
		}
		if checkpoint != tc.want {
			t.Errorf("checkpoint for %s exists=%v, want %v", tc.id, checkpoint, tc.want)
		}
	}
	var repaired State
	if err := pool.QueryRow(ctx, `select state from file_operations where id=$1::uuid`, diverged).Scan(&repaired); err != nil {
		t.Fatal(err)
	}
	if repaired != Succeeded {
		t.Fatalf("repaired row state = %q, want the document's %q", repaired, Succeeded)
	}
}

func ptrTime(v time.Time) *time.Time { return &v }
