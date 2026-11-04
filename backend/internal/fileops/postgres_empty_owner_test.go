package fileops

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedEmptyOwner(t *testing.T, pool *pgxpool.Pool) Root {
	t.Helper()
	id := newID()
	if _, err := pool.Exec(context.Background(), `insert into users(id,email,password_hash) values($1,$2,'test-only')`, id, id+"@example.test"); err != nil {
		t.Fatal(err)
	}
	root := testRoot()
	root.OwnerID = id
	return root
}

func TestAdmitEmptyOwnerOnlyForInactiveOwners(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p := NewPostgres(pool)

	owner := seedEmptyOwner(t, pool)
	if err := p.AdmitEmptyOwner(ctx, owner, 4096, 8192); err != nil {
		t.Fatalf("empty owner rejected: %v", err)
	}
	usage, err := p.Usage(ctx, owner, testActor(owner))
	if err != nil {
		t.Fatal(err)
	}
	if usage.LogicalBytes != 0 || usage.PhysicalBytes != 0 || usage.LogicalLimit != 4096 || usage.PhysicalLimit != 8192 {
		t.Fatalf("unexpected admitted usage: %+v", usage)
	}
	// Idempotent, and an existing baseline is never replaced with different limits.
	if err = p.AdmitEmptyOwner(ctx, owner, 1, 1); err != nil {
		t.Fatalf("repeat admission rejected: %v", err)
	}
	if usage, err = p.Usage(ctx, owner, testActor(owner)); err != nil || usage.LogicalLimit != 4096 || usage.PhysicalLimit != 8192 {
		t.Fatalf("repeat admission changed the budget: %+v %v", usage, err)
	}
	rotated := owner
	rotated.Generation++
	requireCode(t, p.AdmitEmptyOwner(ctx, rotated, 4096, 8192), "root_conflict")

	// Any prior operation row proves this owner is not new.
	active := seedEmptyOwner(t, pool)
	if _, err = pool.Exec(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot) values($1,$2,'owner:test','mkdir','fixture','{}')`, newID(), active.OwnerID); err != nil {
		t.Fatal(err)
	}
	requireCode(t, p.AdmitEmptyOwner(ctx, active, 0, 0), "baseline_required")
	has, err := p.HasFileActivity(ctx, active)
	if err != nil || !has {
		t.Fatalf("activity precondition missed the operation row: %v %v", has, err)
	}

	// Invalid limits are rejected before any row is written.
	fresh := seedEmptyOwner(t, pool)
	requireCode(t, p.AdmitEmptyOwner(ctx, fresh, -1, 0), "invalid_request")
	requireCode(t, p.AdmitEmptyOwner(ctx, fresh, 0, -1), "invalid_request")
	var budgets int
	if err = pool.QueryRow(ctx, `select count(*) from file_budgets where owner_id=$1`, fresh.OwnerID).Scan(&budgets); err != nil || budgets != 0 {
		t.Fatalf("invalid admission wrote a budget: %d %v", budgets, err)
	}
	has, err = p.HasFileActivity(ctx, fresh)
	if err != nil || has {
		t.Fatalf("fresh owner reported activity: %v %v", has, err)
	}
}
