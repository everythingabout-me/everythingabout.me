package nodes

// Probes for the strict ID→path resolution the upload settlement depends on. They use
// the real index and the audited broken-chain state (a moved ancestor row removed while
// the child keeps its cached path), because the binding they guard is what used to let a
// pending replacement or upload settle at a location the entry had left.

import (
	"encoding/json"
	"errors"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestUploadBindingRejectsBrokenAncestry(t *testing.T) {
	db, ctx := nodesTestDB(t)
	pool := db.Pool()
	root := actionOwner(t, pool)
	folder := indexActionEntry(t, pool, root, "Photos", true)
	photo := indexActionEntry(t, pool, root, "Photos/x.jpg", false)
	_ = indexActionEntry(t, pool, root, "Media", true)

	// A tree move relinks only the moved row; the child keeps its pre-move cached path.
	if _, err := pool.Exec(ctx, `update nodes set key=$3,path='Media/Photos',parent='Media',name='Photos'
		where owner_id=$1::uuid and id=$2::uuid`, root.OwnerID, folder, "drive/"+root.OwnerID+"/Media/Photos/"); err != nil {
		t.Fatalf("tree move: %v", err)
	}
	// Removing the moved row is index damage, not a delete of the child: the child still
	// exists, but no ancestry resolves it (docs/design/file-identity.md, write-path audit).
	if _, err := pool.Exec(ctx, `delete from nodes where owner_id=$1::uuid and id=$2::uuid`, root.OwnerID, folder); err != nil {
		t.Fatalf("break ancestry: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)

	// The exported transaction read is the same query a change read uses.
	if _, found, err := FileResolveIDOn(ctx, tx, root.OwnerID, photo); found || !errors.Is(err, ErrIdentityUnresolved) {
		t.Fatalf("ID→path on a transaction: found=%v error=%v, want ErrIdentityUnresolved", found, err)
	}

	// The upload/replacement binding captured "Photos/x.jpg" before the move. The cache
	// still spells exactly that, so a fallback would pass this check and let the settlement
	// publish there; identity says the entry is somewhere else, so it must refuse.
	intent, err := json.Marshal(storage.WriteIntent{Target: "Photos/x.jpg", Parents: true, EntryID: photo})
	if err != nil {
		t.Fatal(err)
	}
	if err = CheckUploadBinding(ctx, tx, fileops.Operation{Root: root, Intent: intent}); !fileops.IsCode(err, "target_changed") {
		t.Fatalf("upload binding accepted a broken chain: %v", err)
	}
}
