package nodes

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
	"everythingabout.me/backend/internal/treescan"
)

func TestIndexedBrowsingPaginationScopeAndHiddenRoots(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	owner := actionOwner(t, pool)
	other := actionOwner(t, pool)
	testURL, err := url.Parse(testpg.Require(t))
	if err != nil {
		t.Fatal(err)
	}
	testURL.Path = "/" + pool.Config().ConnConfig.Database
	db, err := store.Open(ctx, testURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := New(db, nil) // No storage service is available to these reads.
	for _, entry := range []struct {
		path   string
		folder bool
	}{
		{"Folder", true}, {"Folder/nested.txt", false}, {"Folder/hidden", true}, {"Folder/hidden/secret.txt", false},
		{"A.txt", false}, {"a.txt", false}, {"cafe\u0301.txt", false}, {"literal%_.txt", false},
	} {
		indexActionEntry(t, pool, owner, entry.path, entry.folder)
	}
	indexActionEntry(t, pool, other, "other-only.txt", false)
	namespace := fileRootNamespace(owner)
	root, err := service.FileBrowseMetadata(ctx, owner.OwnerID, namespace, "")
	if err != nil || root == nil || !root.Row.IsDir || len(root.Membership) != 1 || root.Membership[0] != "membership:5" {
		t.Fatalf("root %+v error %v", root, err)
	}
	var got []string
	var after *FileBrowseKey
	for {
		page, err := service.FileBrowsePage(ctx, owner.OwnerID, namespace, FileBrowseQuery{List: true, Limit: 2, After: after})
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, record := range page {
			got = append(got, record.Row.Path)
		}
		last := page[len(page)-1].Row
		kind := 1
		if last.IsDir {
			kind = 0
		}
		after = &FileBrowseKey{Kind: kind, Name: last.Name, Path: last.Path}
		if len(got) > 10 {
			t.Fatal("pagination did not advance")
		}
	}
	if want := []string{"Folder", "A.txt", "a.txt", "cafe\u0301.txt", "literal%_.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("paths %q want %q", got, want)
	}
	nfc, err := service.FileBrowseMetadata(ctx, owner.OwnerID, namespace, "café.txt")
	if err != nil || nfc == nil || nfc.Row.Path != "cafe\u0301.txt" {
		t.Fatalf("native spelling %+v %v", nfc, err)
	}
	hidden := []string{"Folder/hidden"}
	folder, err := service.FileBrowseMetadata(ctx, owner.OwnerID, namespace, "Folder", hidden...)
	if err != nil || folder == nil || !reflect.DeepEqual(folder.Membership, []string{"membership:2"}) {
		t.Fatalf("visible membership %+v %v", folder, err)
	}
	missing, err := service.FileBrowseMetadata(ctx, owner.OwnerID, namespace, "Folder/hidden/secret.txt", hidden...)
	if err != nil || missing != nil {
		t.Fatalf("hidden metadata %+v %v", missing, err)
	}
	page, err := service.FileBrowsePage(ctx, owner.OwnerID, namespace, FileBrowseQuery{Path: "Folder", Keyword: "TXT", Kind: "file", Limit: 10, HiddenRoots: hidden})
	if err != nil || len(page) != 1 || page[0].Row.Path != "Folder/nested.txt" {
		t.Fatalf("scoped search %+v %v", page, err)
	}
	page, err = service.FileBrowsePage(ctx, owner.OwnerID, namespace, FileBrowseQuery{Keyword: "%_", Limit: 10})
	if err != nil || len(page) != 1 || page[0].Row.Path != "literal%_.txt" {
		t.Fatalf("literal wildcard search %+v %v", page, err)
	}
	page, err = service.FileBrowsePage(ctx, owner.OwnerID, namespace, FileBrowseQuery{Keyword: "/Folder", Limit: 10, HiddenRoots: hidden})
	if err != nil || len(page) != 2 {
		t.Fatalf("absolute path search %+v %v", page, err)
	}
	bytes, files, err := service.FileBrowseUsage(ctx, owner.OwnerID, "Folder", hidden...)
	if err != nil || bytes != 3 || files != 1 {
		t.Fatalf("usage %d %d %v", bytes, files, err)
	}
}

func TestIndexedMutationResponsesMatchBrowsing(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)
	namespace := fileRootNamespace(root)
	assertResponse := func(raw json.RawMessage, path string) {
		t.Helper()
		record, err := fileBrowseMetadata(ctx, pool, root.OwnerID, namespace, path, nil)
		if err != nil || record == nil {
			t.Fatalf("browse %+v %v", record, err)
		}
		metadata, err := IndexedMetadata(namespace, *record)
		if err != nil {
			t.Fatal(err)
		}
		expected, _ := json.Marshal(metadata)
		if string(raw) != string(expected) {
			t.Fatalf("settlement differs from browse:\n%s\n%s", raw, expected)
		}
	}
	mkdir := actionEffect(0, "mkdir", "", "folder", storage.Folder)
	assertResponse(runActionCommit(t, pool, actionOperation(root, "mkdir", "", "folder", mkdir)), "folder")
	intent, _ := json.Marshal(storage.WriteIntent{Target: "folder/a.txt", Size: 3})
	entry := storage.Entry{Path: "folder/a.txt", Kind: storage.File, Size: 3, Revision: []byte("native-v1")}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := CommitStagedFile(ctx, tx, fileops.Operation{Root: root, Intent: intent}, &storage.MutationResult{Entry: &entry})
	if err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertResponse(raw, "folder/a.txt")
	source := entry
	copy := actionEffect(0, "copy", "folder/a.txt", "folder/b.txt", storage.File)
	copy.SourceEntry = &source
	copy.SourceRevision = source.Revision
	assertResponse(runActionCommit(t, pool, actionOperation(root, "copy", "folder/a.txt", "folder/b.txt", copy)), "folder/b.txt")
	move := actionEffect(0, "move", "folder/a.txt", "folder/c.txt", storage.File)
	move.SourceEntry = &source
	move.SourceRevision = source.Revision
	assertResponse(runActionCommit(t, pool, actionOperation(root, "move", "folder/a.txt", "folder/c.txt", move)), "folder/c.txt")
}

func TestScanInvalidatesChangedContentGeneration(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)
	indexActionEntry(t, pool, root, "file.txt", false)
	seed := func() {
		t.Helper()
		_, err := pool.Exec(ctx, `insert into file_content_generations(owner_id,root_namespace,path,native_fingerprint,content_revision) values($1::uuid,$2,'file.txt','fingerprint','old-content') on conflict do nothing`, root.OwnerID, fileRootNamespace(root))
		if err != nil {
			t.Fatal(err)
		}
	}
	seed()
	scan, err := treescan.Inventory(ctx, strings.NewReader("key,size,lastmodifieddate\n"+paths.KeyFor("file.txt", root.OwnerID)+",9,2020-01-02T03:04:05Z\n"), "drive/"+root.OwnerID+"/", treescan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyScan(ctx, pool, root.OwnerID, scan, ScanApplyOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, `select count(*) from file_content_generations where owner_id=$1::uuid`, root.OwnerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dry-run invalidated generation %d %v", count, err)
	}
	if _, err = ApplyScan(ctx, pool, root.OwnerID, scan, ScanApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `select count(*) from file_content_generations where owner_id=$1::uuid`, root.OwnerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("changed generation survives %d %v", count, err)
	}
	seed()
	report, err := ApplyScan(ctx, pool, root.OwnerID, scan, ScanApplyOptions{})
	if err != nil || report.Unchanged != 1 {
		t.Fatalf("unchanged scan %+v %v", report, err)
	}
	if err = pool.QueryRow(ctx, `select count(*) from file_content_generations where owner_id=$1::uuid`, root.OwnerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unchanged generation lost %d %v", count, err)
	}
}
