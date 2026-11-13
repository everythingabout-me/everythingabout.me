package nodes

import (
	"context"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tryOwnerSettlement performs an index settlement under the owner admission lock
// every fileops transaction takes. A scan holds that lock for its whole native
// walk and import, so a settlement attempted during a scan must not be admitted.
func tryOwnerSettlement(t *testing.T, pool *pgxpool.Pool, owner string, settle func(context.Context, pgx.Tx) error) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var admitted bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock(`+fileops.OwnerLockKey+`)`, owner).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	if !admitted {
		return false
	}
	if err := settle(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return true
}

func settleOwner(t *testing.T, pool *pgxpool.Pool, owner string, settle func(context.Context, pgx.Tx) error) {
	t.Helper()
	if !tryOwnerSettlement(t, pool, owner, settle) {
		t.Fatal("the settlement was still refused after the scan released its lock")
	}
}

// holdScanLock keeps a failed assertion from leaking the scan's session locks.
func holdScanLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, owner string) (*pgx.Conn, func()) {
	t.Helper()
	conn, unlock, err := LockScanOwner(ctx, pool, owner)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		unlock()
	}
	t.Cleanup(release)
	return conn, release
}

// TestScanOwnerLockSerializesSettlements pins the two consequences of a
// scan and a live settlement sharing the owner admission lock: an upload that
// settled after the native walk looked absent to Sync and was pruned, and a
// deletion that settled there had already removed the rows the stale walk
// reported, so the import re-inserted them. LockScanOwner holds the owner
// lock across the walk and the import, so neither settlement can land in between.
func TestScanOwnerLockSerializesSettlements(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)

	insertFile := func(path string) func(context.Context, pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,meta)
 values($1::uuid,$2,$3,$4,$5,false,3,'{}')`, root.OwnerID, paths.KeyFor(path, root.OwnerID), path, paths.ParentOf(path), paths.NameOf(path))
			return err
		}
	}

	t.Run("upload settled during the scan is not pruned", func(t *testing.T) {
		initial := scanChangesInventory(t, root, "ROOT/before.txt,3,etag-before\n")
		if _, err := ApplyScan(ctx, pool, root.OwnerID, initial, ScanApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		conn, release := holdScanLock(t, ctx, pool, root.OwnerID)
		// The walk saw only before.txt. The upload below is the write that landed
		// after that walk and before the import.
		settledDuringScan := tryOwnerSettlement(t, pool, root.OwnerID, insertFile("during.txt"))
		if _, err := ApplyScan(ctx, conn, root.OwnerID, initial, ScanApplyOptions{Sync: true}); err != nil {
			t.Fatal(err)
		}
		release()
		if !settledDuringScan {
			settleOwner(t, pool, root.OwnerID, insertFile("during.txt"))
		}
		var exists bool
		if err := pool.QueryRow(ctx, `select exists(select 1 from file_entries where owner_id=$1::uuid and path=$2)`, root.OwnerID, "during.txt").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("the sync import pruned an upload that settled during the scan (settled=%t)", settledDuringScan)
		}
	})

	t.Run("delete settled during the scan is not resurrected", func(t *testing.T) {
		initial := scanChangesInventory(t, root, "ROOT/Photos/a.jpg,3,etag-photo\n")
		if _, err := ApplyScan(ctx, pool, root.OwnerID, initial, ScanApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		conn, release := holdScanLock(t, ctx, pool, root.OwnerID)
		deleteTree := func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `delete from nodes where owner_id=$1::uuid and (path=$2 or path like $3)`, root.OwnerID, "Photos", "Photos/%")
			return err
		}
		// The walk still reports Photos/a.jpg because the native purge has not run.
		settledDuringScan := tryOwnerSettlement(t, pool, root.OwnerID, deleteTree)
		if _, err := ApplyScan(ctx, conn, root.OwnerID, initial, ScanApplyOptions{Sync: true}); err != nil {
			t.Fatal(err)
		}
		release()
		if !settledDuringScan {
			settleOwner(t, pool, root.OwnerID, deleteTree)
		}
		var exists bool
		if err := pool.QueryRow(ctx, `select exists(select 1 from file_entries where owner_id=$1::uuid and path=$2)`, root.OwnerID, "Photos/a.jpg").Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("the sync import resurrected a subtree deleted during the scan (settled=%t)", settledDuringScan)
		}
	})
}
