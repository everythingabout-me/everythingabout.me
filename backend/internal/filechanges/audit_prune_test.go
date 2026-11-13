package filechanges

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// This file pins the audit retention rule: only rows observed strictly before the
// cutoff leave, a dry run changes nothing, and the owner-scoped reader still returns
// everything the cutoff kept (with its keyset intact).

func insertAuditRow(t *testing.T, pool *pgxpool.Pool, owner, action, outcome string, observed time.Time) string {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	audit := &Audit{
		OwnerID: owner, Action: action, Outcome: outcome, Status: 200,
		Actor: Actor{Kind: "owner"}, Origin: "http", ObservedAt: observed.UTC(),
	}
	if err = RecordAudit(context.Background(), tx, audit); err != nil {
		t.Fatalf("record %s/%s: %v", action, outcome, err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return audit.ID
}

func countOwnerAudit(t *testing.T, pool *pgxpool.Pool, owner string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `select count(*) from file_audit where owner_id=$1::uuid`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPostgresPruneAuditBeforeRemovesOnlyExpired(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	other := newOwner(t, pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-365 * 24 * time.Hour)

	expired := []string{
		insertAuditRow(t, pool, owner, "read", "admitted", cutoff.Add(-72*time.Hour)),
		insertAuditRow(t, pool, owner, "read", "completed", cutoff.Add(-71*time.Hour)),
		insertAuditRow(t, pool, owner, "put", "confirmed", cutoff.Add(-70*time.Hour)),
	}
	boundary := insertAuditRow(t, pool, owner, "put", "admitted", cutoff)
	recent := []string{
		insertAuditRow(t, pool, owner, "put", "completed", cutoff.Add(time.Hour)),
		insertAuditRow(t, pool, owner, "delete", "confirmed", now.Add(-time.Hour)),
	}
	foreign := insertAuditRow(t, pool, other, "read", "admitted", cutoff.Add(-90*24*time.Hour))

	if got := countOwnerAudit(t, pool, owner); got != 6 {
		t.Fatalf("seeded owner rows=%d want 6", got)
	}

	// A dry run reports the same count and changes nothing.
	dry, err := PruneAuditBefore(ctx, pool, owner, cutoff, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry != 3 {
		t.Fatalf("dry-run count=%d want 3", dry)
	}
	if got := countOwnerAudit(t, pool, owner); got != 6 {
		t.Fatalf("dry run deleted rows: owner rows=%d want 6", got)
	}

	deleted, err := PruneAuditBefore(ctx, pool, owner, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted=%d want 3", deleted)
	}
	if got := countOwnerAudit(t, pool, owner); got != 3 {
		t.Fatalf("owner rows after prune=%d want 3", got)
	}
	for _, id := range expired {
		var exists bool
		if err := pool.QueryRow(ctx, `select exists(select 1 from file_audit where id=$1::uuid)`, id).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("expired row %s survived the prune", id)
		}
	}
	// The boundary row is the first retained row: the cutoff is exclusive.
	var exists bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from file_audit where id=$1::uuid)`, boundary).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("row observed exactly at the cutoff was deleted; the cutoff must be exclusive")
	}
	// Another owner's rows are outside this command's scope even when expired.
	if err := pool.QueryRow(ctx, `select exists(select 1 from file_audit where id=$1::uuid)`, foreign).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("prune crossed the owner boundary")
	}

	// The reader still returns what remains, newest first, and its page boundary works.
	first, err := ReadAuditPage(ctx, pool, owner, 2, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != recent[1] || first[1].ID != recent[0] {
		t.Fatalf("first page after prune=%v want %v then %v", auditIDs(first), recent[1], recent[0])
	}
	last := first[len(first)-1]
	second, err := ReadAuditPage(ctx, pool, owner, 2, last.ObservedAt, last.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].ID != boundary {
		t.Fatalf("second page after prune=%v want only the boundary row %s", auditIDs(second), boundary)
	}

	// Re-running the same cutoff is a no-op, not an error.
	again, err := PruneAuditBefore(ctx, pool, owner, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second run deleted %d rows; prune must be idempotent", again)
	}
}

func TestPostgresPruneAuditBeforeWalksEveryBatch(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	cutoff := time.Now().UTC().Add(-365 * 24 * time.Hour)
	// Two full batches plus a remainder, so a prune that stops after the first batch
	// fails instead of silently leaving rows behind.
	const seeds = 2*auditPruneBatch + 5
	if _, err := pool.Exec(ctx, `insert into file_audit(owner_id,action,outcome,status,actor,origin,observed_at,fingerprint)
 select $1::uuid,'read','admitted',200,'{"kind":"owner"}','http', $2::timestamptz - (g||' minutes')::interval, 'seed-'||g
 from generate_series(1,$3) g`, owner, cutoff, seeds); err != nil {
		t.Fatal(err)
	}
	deleted, err := PruneAuditBefore(ctx, pool, owner, cutoff, false)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != seeds {
		t.Fatalf("deleted=%d want %d across batches", deleted, seeds)
	}
	if got := countOwnerAudit(t, pool, owner); got != 0 {
		t.Fatalf("rows left after a full prune=%d", got)
	}
}

func auditIDs(items []Audit) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}
