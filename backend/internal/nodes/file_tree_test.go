package nodes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestCanonicalTreeMovePreservesThirtyThousandDescendants(t *testing.T) {
	db, ctx := nodesTestDB(t)
	pool := db.Pool()
	service := New(db, nil)
	root := actionOwner(t, pool)
	folder := indexActionEntry(t, pool, root, "before", true)
	// Populate a large established index directly; the move must not update any
	// descendant, generation or event reference, irrespective of total tree size.
	if _, err := pool.Exec(ctx, `alter table nodes disable trigger user`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,parent_id,comparison_name)
 select $1::uuid,'drive/'||$1||'/before/'||name,'before/'||name,'before',name,false,3,$2::uuid,name
 from (select 'file-'||lpad(i::text,5,'0')||'.txt' as name from generate_series(1,30000) i) fixture`, root.OwnerID, folder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `alter table nodes enable trigger user`); err != nil {
		t.Fatal(err)
	}
	leafID, _, err := service.FileIdentity(ctx, root.OwnerID, "before/file-00001.txt")
	if err != nil {
		t.Fatal(err)
	}
	event := attachActionEvent(t, pool, root, leafID)
	nativeLeaf := storage.Entry{Path: "before/file-00001.txt", Kind: storage.File, Size: 3, Revision: []byte("native-version"), ContentIdentity: []byte("native-file-identity")}
	originalRevision := "cr_preserved_from_earlier_move"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = saveContentRevision(ctx, tx, root, nativeLeaf, &originalRevision); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := service.FileBrowseMetadata(ctx, root.OwnerID, fileRootNamespace(root), "before")
	if err != nil {
		t.Fatal(err)
	}
	// Any attempted descendant UPDATE/DELETE fails immediately. Cache columns of
	// those rows intentionally remain unchanged after the ancestor moves.
	_, err = pool.Exec(ctx, `create function reject_descendant_tree_write() returns trigger language plpgsql as $$ begin
 if OLD.owner_id='`+root.OwnerID+`'::uuid and OLD.id<>'`+folder+`'::uuid then raise exception 'unexpected descendant write'; end if;
 if TG_OP='DELETE' then return OLD; end if; return NEW; end $$;
 create trigger reject_descendant_tree_write before update or delete on nodes for each row execute function reject_descendant_tree_write()`)
	if err != nil {
		t.Fatal(err)
	}
	effect := actionEffect(0, "move", "before", "after", storage.Folder)
	var nativeMove storage.MutationResult
	if err := json.Unmarshal(effect.Result, &nativeMove); err != nil {
		t.Fatal(err)
	}
	source := *nativeMove.Entry
	source.Path = "before"
	effect.SourceRevision = source.Revision
	effect.SourceEntry = &source
	raw := runActionCommit(t, pool, actionOperation(root, "move", "before", "after", effect))
	var result struct{ ID, Path string }
	if err = json.Unmarshal(raw, &result); err != nil || result.ID != folder || result.Path != "/after" {
		t.Fatalf("move result %s %v", raw, err)
	}
	if _, err = pool.Exec(ctx, `drop trigger reject_descendant_tree_write on nodes;drop function reject_descendant_tree_write()`); err != nil {
		t.Fatal(err)
	}
	after, err := service.FileBrowseMetadata(ctx, root.OwnerID, fileRootNamespace(root), "after")
	if err != nil || after == nil {
		t.Fatalf("after %+v %v", after, err)
	}
	if len(after.Membership) != 1 || after.Membership[0] != before.Membership[0] {
		t.Fatalf("membership changed %+v => %+v", before, after)
	}
	resolved, found, err := service.FileResolveID(ctx, root.OwnerID, leafID)
	if err != nil || !found || resolved != "after/file-00001.txt" {
		t.Fatalf("descendant ID resolved %q %v %v", resolved, found, err)
	}
	if old, err := service.FileBrowseMetadata(ctx, root.OwnerID, fileRootNamespace(root), "before/file-00001.txt"); err != nil || old != nil {
		t.Fatalf("old canonical path visible %+v %v", old, err)
	}
	nativeLeaf.Path = "after/file-00001.txt"
	revision, err := service.LookupContentRevision(ctx, root.OwnerID, fileRootNamespace(root), nativeLeaf)
	if err != nil || revision == nil || *revision != originalRevision {
		t.Fatalf("content revision %v %v", revision, err)
	}
	var linked, cachePath string
	if err = pool.QueryRow(ctx, `select node_id::text from event_media where event_id=$1::uuid`, event).Scan(&linked); err != nil || linked != leafID {
		t.Fatalf("event %q %v", linked, err)
	}
	if err = pool.QueryRow(ctx, `select path from nodes where id=$1::uuid`, leafID).Scan(&cachePath); err != nil || cachePath != "before/file-00001.txt" {
		t.Fatalf("descendant cache rewritten %q %v", cachePath, err)
	}
	// Recreate at the old location. Only the conflicting old cache is invalidated;
	// the moved descendant's ID, bytes evidence and revision remain independent.
	replacement := indexActionEntry(t, pool, root, "before/file-00001.txt", false)
	if replacement == leafID {
		t.Fatal("old path recreation reused moved identity")
	}
	resolved, found, err = service.FileResolveID(ctx, root.OwnerID, leafID)
	if err != nil || !found || resolved != "after/file-00001.txt" {
		t.Fatalf("recreation retargeted moved ID %q %v %v", resolved, found, err)
	}
	// Legacy UPSERT must refresh that same current canonical node, not allocate an
	// independent identity merely because its old advisory cache was invalidated.
	if _, err = service.UpsertFile(ctx, UpsertInput{OwnerID: root.OwnerID, Path: "after/file-00001.txt", Size: 3}); err != nil {
		t.Fatal(err)
	}
	id, _, err := service.FileIdentity(ctx, root.OwnerID, "after/file-00001.txt")
	if err != nil || id != leafID {
		t.Fatalf("legacy upsert identity %q %v", id, err)
	}
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, migration := range migrations {
			data, err := os.ReadFile(migration)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(data)); err != nil {
				t.Fatalf("replay %d %s: %v", pass, filepath.Base(migration), err)
			}
		}
	}
	resolved, found, err = service.FileResolveID(ctx, root.OwnerID, leafID)
	if err != nil || !found || resolved != "after/file-00001.txt" {
		t.Fatalf("migration undid canonical move %q %v %v", resolved, found, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `select count(*) from file_entries where owner_id=$1::uuid and parent_id=$2::uuid`, root.OwnerID, folder).Scan(&count); err != nil || count != 30000 {
		t.Fatalf("descendant count %d %v", count, err)
	}
}
