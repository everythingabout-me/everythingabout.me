package nodes

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestBrowseExpectedIdentityRejectsRecreatedFolder(t *testing.T) {
	db, ctx := nodesTestDB(t)
	pool := db.Pool()
	service := New(db, nil)
	owner := actionOwner(t, pool)
	original := indexActionEntry(t, pool, owner, "folder", true)
	indexActionEntry(t, pool, owner, "folder/old.txt", false)
	query := FileBrowseQuery{Path: "folder", ExpectedID: original, List: true, Limit: 10}
	page, err := service.FileBrowsePage(ctx, owner.OwnerID, "root", query)
	if err != nil || len(page) != 1 {
		t.Fatalf("original page %+v %v", page, err)
	}
	if _, err = pool.Exec(ctx, `delete from nodes where owner_id=$1::uuid`, owner.OwnerID); err != nil {
		t.Fatal(err)
	}
	replacement := indexActionEntry(t, pool, owner, "folder", true)
	indexActionEntry(t, pool, owner, "folder/new.txt", false)
	for _, list := range []bool{true, false} {
		query.List = list
		page, err = service.FileBrowsePage(ctx, owner.OwnerID, "root", query)
		if err != nil || len(page) != 0 {
			t.Fatalf("replacement leaked: list=%v page=%+v err=%v", list, page, err)
		}
	}
	query.ExpectedID = replacement
	query.List = true
	page, err = service.FileBrowsePage(ctx, owner.OwnerID, "root", query)
	if err != nil || len(page) != 1 || page[0].Row.Path != "folder/new.txt" {
		t.Fatalf("replacement page %+v %v", page, err)
	}
	root, _, err := service.FileIdentity(ctx, owner.OwnerID, "")
	if err != nil {
		t.Fatal(err)
	}
	query = FileBrowseQuery{ExpectedID: root, List: true, Limit: 10}
	page, err = service.FileBrowsePage(ctx, owner.OwnerID, "root", query)
	if err != nil || len(page) != 1 {
		t.Fatalf("root page %+v %v", page, err)
	}
	query.ExpectedID = original
	page, err = service.FileBrowsePage(ctx, owner.OwnerID, "root", query)
	if err != nil || len(page) != 0 {
		t.Fatalf("foreign root binding %+v %v", page, err)
	}
}

func TestFileIdentityBusyUsesOwnerNativeRulesAndDurableGuards(t *testing.T) {
	db, ctx := nodesTestDB(t)
	pool := db.Pool()
	service := New(db, nil)
	owner, other := actionOwner(t, pool), actionOwner(t, pool)
	var operation string
	snapshot, _ := json.Marshal(fileops.Operation{Root: fileops.Root{OwnerID: owner.OwnerID, CaseSensitive: false}})
	// FileIdentityBusy compares paths through the operation's case rule, which since
	// sql/047 is a column; write it from the same Root so the fixture keeps testing
	// case-insensitive comparison.
	if err := pool.QueryRow(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot,root_case_sensitive) values(gen_random_uuid(),$1::uuid,'test','move','fixture',$2,false) returning id::text`, owner.OwnerID, snapshot).Scan(&operation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree) values($1::uuid,$2::uuid,0,'Straße','strasse',true),($1::uuid,$2::uuid,1,'Exact','exact',false)`, operation, owner.OwnerID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ owner, path, want string }{
		{owner.OwnerID, "STRASSE/child.txt", operation}, {owner.OwnerID, "straße", operation},
		{owner.OwnerID, "EXACT", operation}, {owner.OwnerID, "Exact/child.txt", ""},
		{owner.OwnerID, "Straße-other/child.txt", ""}, {other.OwnerID, "Straße/child.txt", ""},
	} {
		got, err := service.FileIdentityBusy(ctx, tc.owner, tc.path)
		if err != nil || got != tc.want {
			t.Fatalf("busy(%q,%q)=%q want=%q err=%v", tc.owner, tc.path, got, tc.want, err)
		}
	}
	if _, err := pool.Exec(ctx, `delete from file_operation_guards where operation_id=$1::uuid`, operation); err != nil {
		t.Fatal(err)
	}
	if got, err := service.FileIdentityBusy(ctx, owner.OwnerID, "Straße/child.txt"); err != nil || got != "" {
		t.Fatalf("released guard %q %v", got, err)
	}
}

func TestNewFileRetiresStaleFolderDescendants(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	for _, kind := range []string{"create", "copy", "move"} {
		t.Run(kind, func(t *testing.T) {
			owner := actionOwner(t, pool)
			stale := indexActionEntry(t, pool, owner, "target%_", true)
			ghost := indexActionEntry(t, pool, owner, "target%_/ghost.txt", false)
			keep := indexActionEntry(t, pool, owner, "targetXX/keep.txt", false)
			source := indexActionEntry(t, pool, owner, "source.txt", false)
			event := attachActionEvent(t, pool, owner, source)
			if _, err := pool.Exec(ctx, `insert into file_content_generations(owner_id,root_namespace,path,native_fingerprint,content_revision) values($1::uuid,$2,'target%_/ghost.txt','x','old')`, owner.OwnerID, fileRootNamespace(owner)); err != nil {
				t.Fatal(err)
			}
			var raw json.RawMessage
			if kind == "create" {
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx)
				intent, _ := json.Marshal(storage.WriteIntent{Target: "target%_", Size: 3, Conditions: storage.Conditions{Absent: true}})
				raw, err = CommitStagedFile(ctx, tx, fileops.Operation{Root: owner, Intent: intent}, &storage.MutationResult{Entry: &storage.Entry{Path: "target%_", Kind: storage.File, Size: 3}})
				if err != nil {
					t.Fatal(err)
				}
				if err = tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				raw = runActionCommit(t, pool, actionOperation(owner, kind, "source.txt", "target%_", actionEffect(0, kind, "source.txt", "target%_", storage.File)))
			}
			var id string
			if err := pool.QueryRow(ctx, `select id::text from nodes where owner_id=$1::uuid and path='target%_'`, owner.OwnerID).Scan(&id); err != nil || id == stale || id == ghost {
				t.Fatalf("target %q %s err=%v", id, raw, err)
			}
			if kind == "move" && id != source {
				t.Fatalf("source identity lost %s", raw)
			}
			assertActionID(t, pool, owner, "target%_/ghost.txt", "")
			assertActionID(t, pool, owner, "targetXX/keep.txt", keep)
			var count int
			if err := pool.QueryRow(ctx, `select count(*) from file_content_generations where owner_id=$1::uuid and path='target%_/ghost.txt'`, owner.OwnerID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale generation %d %v", count, err)
			}
			var linked string
			if err := pool.QueryRow(ctx, `select node_id::text from event_media where event_id=$1::uuid`, event).Scan(&linked); err != nil || linked != source {
				t.Fatalf("source reference lost %q %v", linked, err)
			}
			migration, err := os.ReadFile("../../../sql/026_file_identity.sql")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(migration)); err != nil {
				t.Fatalf("migration after replacement: %v", err)
			}
		})
	}
}
