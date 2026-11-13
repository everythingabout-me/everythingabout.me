package nodes

import (
	"context"
	"testing"

	"everythingabout.me/backend/internal/paths"
)

// ensureUploadParents is the surviving half of the retired upload observer: it
// runs when a parents:true upload settles. These cases pin the repair it has to
// keep doing — a file row whose folder rows were removed must be reattached to a
// rebuilt chain instead of being re-created under a new identity.
func TestEnsureUploadParentsRepairsMissingAncestors(t *testing.T) {
	pool := actionTestDatabase(t)
	root := actionOwner(t, pool)
	ctx := context.Background()
	path := "Health/imported/day.json"
	var id string
	if err := pool.QueryRow(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size)
 values($1::uuid,$2,$3,$4,$5,false,3) returning id::text`,
		root.OwnerID, paths.KeyFor(path, root.OwnerID), path, paths.ParentOf(path), paths.NameOf(path)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// The native file is still there, but every folder row is gone, so the file
	// row's parent link dangles.
	if _, err := pool.Exec(ctx, `delete from nodes where owner_id=$1::uuid and is_dir`, root.OwnerID); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ensureUploadParents(ctx, tx, root.OwnerID, path)
	if err != nil || !changed {
		tx.Rollback(ctx)
		t.Fatalf("repair changed=%v err=%v", changed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, parentPath := range paths.AncestorsOf(path) {
		var folder bool
		if err := pool.QueryRow(ctx, `select is_dir from nodes where owner_id=$1::uuid and path=$2`, root.OwnerID, parentPath).Scan(&folder); err != nil || !folder {
			t.Fatalf("missing repaired parent %q: %v", parentPath, err)
		}
	}
	var repaired string
	if err := pool.QueryRow(ctx, `select id::text from nodes where owner_id=$1::uuid and path=$2`, root.OwnerID, path).Scan(&repaired); err != nil || repaired != id {
		t.Fatalf("repair changed the file identity: %q vs %q, %v", repaired, id, err)
	}
	// A repaired tree is a no-op on the next settlement, so repeated commits do
	// not churn an unchanged file.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err = ensureUploadParents(ctx, tx, root.OwnerID, path)
	if err != nil || changed {
		tx.Rollback(ctx)
		t.Fatalf("second repair changed=%v err=%v", changed, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
