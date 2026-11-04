package nodes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

func scanChangesInventory(t *testing.T, root fileops.Root, records string) *treescan.Result {
	t.Helper()
	prefix := paths.OwnerPrefix(root.OwnerID)
	input := "key,size,etag\n" + strings.ReplaceAll(records, "ROOT/", prefix)
	result, err := treescan.Inventory(context.Background(), strings.NewReader(input), prefix, treescan.Options{Names: storage.NameRules{CaseSensitive: root.CaseSensitive}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestScanChangesCreatedUpdatedNoopAndDryRun(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)
	root.CaseSensitive = false
	initial := scanChangesInventory(t, root, "ROOT/Docs/photo.jpg,3,etag-one\n")
	opts := ScanApplyOptions{ChangeRoot: &root}
	report, err := ApplyScan(ctx, pool, root.OwnerID, initial, opts)
	if err != nil || report.Inserted != 2 {
		t.Fatalf("initial scan: %+v, %v", report, err)
	}
	head, err := filechanges.ReadHead(ctx, pool, root.OwnerID)
	if err != nil || head.Position != 2 || head.RootNamespace != fileRootNamespace(root) {
		t.Fatalf("initial change head or binding: %+v, %v", head, err)
	}
	changes, err := filechanges.Read(ctx, pool, root.OwnerID, 0, head.Position, 10)
	if err != nil || len(changes) != 2 {
		t.Fatalf("initial changes: %+v, %v", changes, err)
	}
	var fileID, folderID string
	for _, change := range changes {
		if change.Kind != "created" || change.Origin != "external" || change.Actor.Kind != "external" || change.Actor.PrincipalID != "" || change.OperationID != "" || change.RequestID != "" || change.Before != nil || change.After == nil || change.After.ChangeToken != change.ID || change.After.LastChangedAt == nil {
			t.Fatalf("invalid external attribution/snapshot: %+v", change)
		}
		if len(change.AffectedFolders) != len(change.AfterAncestors) {
			t.Fatalf("created change omitted ancestor patches: %+v", change)
		}
		seen := map[string]bool{}
		for _, folder := range change.AffectedFolders {
			if seen[folder.ID] || folder.Kind != "folder" || folder.ChangeToken != change.ID || folder.LastChangedAt == nil || !folder.LastChangedAt.Equal(change.ObservedAt) {
				t.Fatalf("invalid created ancestor patch: %+v", folder)
			}
			seen[folder.ID] = true
		}
		if change.After.Kind == "file" {
			fileID = change.After.ID
		} else {
			folderID = change.After.ID
		}
		marker, err := filechanges.ReadMarker(ctx, pool, root.OwnerID, change.After.ID)
		if err != nil || marker.Position < change.Position {
			t.Fatalf("entry marker: %+v, %v", marker, err)
		}
	}
	if fileID == "" || folderID == "" {
		t.Fatal("scan did not emit both explicit file and implicit folder")
	}
	var version, rootID string
	if err := pool.QueryRow(ctx, `select name_comparison_version,id::text from file_identity_roots where owner_id=$1::uuid`, root.OwnerID).Scan(&version, &rootID); err != nil || version != "nfc-fold-v1" {
		t.Fatalf("scan root rules: %q, %v", version, err)
	}
	for _, id := range []string{fileID, folderID, rootID} {
		marker, err := filechanges.ReadMarker(ctx, pool, root.OwnerID, id)
		if err != nil || marker.Position != head.Position {
			t.Fatalf("recursive marker %s: %+v, %v", id, marker, err)
		}
	}
	// Content evidence belongs to the bound API namespace. A changed external
	// observation clears it instead of attaching an earlier revision to new bytes.
	if _, err := pool.Exec(ctx, `insert into file_content_generations(owner_id,root_namespace,path,node_id,native_fingerprint,content_revision)
 values($1::uuid,$2,'Docs/photo.jpg',$3::uuid,'native-one','revision-one')`, root.OwnerID, fileRootNamespace(root), fileID); err != nil {
		t.Fatal(err)
	}
	report, err = ApplyScan(ctx, pool, root.OwnerID, initial, opts)
	if err != nil || report.Unchanged != 2 {
		t.Fatalf("repeat scan: %+v, %v", report, err)
	}
	unchanged, err := filechanges.ReadHead(ctx, pool, root.OwnerID)
	if err != nil || unchanged.Position != head.Position {
		t.Fatalf("repeat scan emitted changes: %+v, %v", unchanged, err)
	}
	changed := scanChangesInventory(t, root, "ROOT/Docs/photo.jpg,8,etag-two\n")
	report, err = ApplyScan(ctx, pool, root.OwnerID, changed, ScanApplyOptions{DryRun: true, ChangeRoot: &root})
	if err != nil || report.Updated != 1 || report.Unchanged != 1 {
		t.Fatalf("dry run: %+v, %v", report, err)
	}
	var size int64
	if err := pool.QueryRow(ctx, `select size from file_entries where id=$1::uuid`, fileID).Scan(&size); err != nil || size != 3 {
		t.Fatalf("dry run changed index: %d, %v", size, err)
	}
	// An earlier service clock may be ahead of this worker. Saved snapshots and
	// recursive markers must agree on the clamped service observation time.
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `update file_change_markers set last_changed_at=$3 where owner_id=$1::uuid and entry_id=$2::uuid`, root.OwnerID, rootID, future); err != nil {
		t.Fatal(err)
	}
	report, err = ApplyScan(ctx, pool, root.OwnerID, changed, opts)
	if err != nil || report.Updated != 1 || report.Unchanged != 1 {
		t.Fatalf("updated scan: %+v, %v", report, err)
	}
	changes, err = filechanges.Read(ctx, pool, root.OwnerID, head.Position, head.Position+1, 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("updated change: %+v, %v", changes, err)
	}
	change := changes[0]
	if change.Kind != "updated" || change.Before == nil || change.After == nil || change.Before.ID != fileID || change.After.ID != fileID || change.Before.Size == nil || *change.Before.Size != 3 || change.After.Size == nil || *change.After.Size != 8 || change.Before.ContentRevision == nil || *change.Before.ContentRevision != "revision-one" || change.After.ContentRevision != nil || len(change.BeforeAncestors) != 2 || len(change.AfterAncestors) != 2 {
		t.Fatalf("updated transition lost metadata or ancestry: %+v", change)
	}
	if change.After.ChangeToken != change.ID || change.After.LastChangedAt == nil || !change.After.LastChangedAt.Equal(future) || !change.ObservedAt.Equal(future) {
		t.Fatalf("updated snapshot marker or clamped time mismatch: %+v", change)
	}
	if len(change.AffectedFolders) != 2 {
		t.Fatalf("updated change omitted parent/root patches: %+v", change.AffectedFolders)
	}
	patched := map[string]bool{}
	for _, folder := range change.AffectedFolders {
		if patched[folder.ID] || (folder.ID != folderID && folder.ID != rootID) || folder.Kind != "folder" || folder.ChangeToken != change.ID || folder.LastChangedAt == nil || !folder.LastChangedAt.Equal(future) {
			t.Fatalf("updated ancestor patch mismatch: %+v", folder)
		}
		patched[folder.ID] = true
		record, err := fileBrowseMetadata(ctx, pool, root.OwnerID, fileRootNamespace(root), folder.Path.Relative(), nil)
		if err != nil || record == nil {
			t.Fatalf("current ancestor metadata: %+v, %v", record, err)
		}
		current, err := IndexedMetadata(fileRootNamespace(root), *record)
		if err != nil || current.ID != folder.ID || current.Revision != folder.Revision || current.ChangeToken != folder.ChangeToken || current.LastChangedAt == nil || !current.LastChangedAt.Equal(*folder.LastChangedAt) {
			t.Fatalf("retained ancestor patch differs from committed metadata: saved=%+v current=%+v error=%v", folder, current, err)
		}
	}
	for _, id := range []string{fileID, folderID, rootID} {
		marker, err := filechanges.ReadMarker(ctx, pool, root.OwnerID, id)
		if err != nil || marker.Token != change.ID || marker.Position != head.Position+1 || !marker.LastChangedAt.Equal(future) {
			t.Fatalf("updated recursive marker %s: %+v, %v", id, marker, err)
		}
	}
	var retained int
	if err := pool.QueryRow(ctx, `select count(*) from file_content_generations where owner_id=$1::uuid`, root.OwnerID).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("external content evidence retained: %d, %v", retained, err)
	}
	if report, err := ApplyScan(ctx, pool, root.OwnerID, changed, opts); err != nil || report.Unchanged != 2 {
		t.Fatalf("repeat updated scan emitted another transition: %+v, %v", report, err)
	}
	// An empty inventory supplies no evidence for deletion or external rename.
	empty := scanChangesInventory(t, root, "")
	if report, err := ApplyScan(ctx, pool, root.OwnerID, empty, opts); err != nil || report.Scanned != 0 {
		t.Fatalf("empty no-prune import: %+v, %v", report, err)
	}
	current, err := filechanges.ReadHead(ctx, pool, root.OwnerID)
	if err != nil || current.Position != head.Position+1 {
		t.Fatalf("empty import emitted deletion facts: %+v, %v", current, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from file_entries where owner_id=$1::uuid`, root.OwnerID).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("empty import pruned retained index: %d, %v", retained, err)
	}
	changedRoot := root
	changedRoot.Generation++
	changedRoot.CaseSensitive = true
	rebound := scanChangesInventory(t, root, "ROOT/Docs/photo.jpg,12,etag-three\nROOT/new.txt,1,new\n")
	if _, err := ApplyScan(ctx, pool, root.OwnerID, rebound, ScanApplyOptions{ChangeRoot: &changedRoot}); !errors.Is(err, filechanges.ErrRootChanged) {
		t.Fatalf("changed namespace must require explicit reset: %v", err)
	}
	unchangedBinding, err := filechanges.ReadHead(ctx, pool, root.OwnerID)
	if err != nil || unchangedBinding.Position != current.Position || unchangedBinding.RootEpoch != current.RootEpoch || unchangedBinding.RootNamespace != current.RootNamespace {
		t.Fatalf("rejected root changed stream state: %+v, %v", unchangedBinding, err)
	}
	if err := pool.QueryRow(ctx, `select name_comparison_version from file_identity_roots where owner_id=$1::uuid`, root.OwnerID).Scan(&version); err != nil || version != "nfc-fold-v1" {
		t.Fatalf("rejected root changed name comparison: %q, %v", version, err)
	}
	if err := pool.QueryRow(ctx, `select size from file_entries where id=$1::uuid`, fileID).Scan(&size); err != nil || size != 8 {
		t.Fatalf("rejected root changed existing entry: %d, %v", size, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from file_entries where owner_id=$1::uuid`, root.OwnerID).Scan(&retained); err != nil || retained != 2 {
		t.Fatalf("rejected root inserted an entry: %d, %v", retained, err)
	}
}

func TestScanChangesRollbackAndOptionalBinding(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)
	result := scanChangesInventory(t, root, "ROOT/Docs/accepted.txt,3,one\nROOT/Docs/reject.txt,4,two\n")
	// Reject the last feed append after earlier entries were appended, proving
	// that index writes, earlier feed rows, head and markers all roll back.
	if _, err := pool.Exec(ctx, `alter table file_changes add constraint reject_scan_change check(after_snapshot->>'name'<>'reject.txt')`); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyScan(ctx, pool, root.OwnerID, result, ScanApplyOptions{ChangeRoot: &root}); err == nil {
		t.Fatal("scan succeeded despite rejected change append")
	}
	for _, table := range []string{"nodes", "file_changes", "file_change_effects", "file_change_heads", "file_change_markers", "file_identity_roots"} {
		var count int
		if err := pool.QueryRow(ctx, fmt.Sprintf("select count(*) from %s where owner_id=$1::uuid", table), root.OwnerID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback retained %s: %d, %v", table, count, err)
		}
	}
	if _, err := pool.Exec(ctx, `alter table file_changes drop constraint reject_scan_change`); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyScan(ctx, pool, root.OwnerID, result, ScanApplyOptions{DryRun: true, ChangeRoot: &root}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from file_change_heads where owner_id=$1::uuid`, root.OwnerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry-run allocated change head: %d, %v", count, err)
	}
	foreign := root
	foreign.OwnerID = changeID()
	if _, err := ApplyScan(ctx, pool, root.OwnerID, result, ScanApplyOptions{ChangeRoot: &foreign}); err == nil {
		t.Fatal("foreign root accepted")
	}
	// Index-only fixtures may omit a configured namespace; production CLI never
	// does. This intentionally does not claim changes in an invented namespace.
	report, err := ApplyScan(ctx, pool, root.OwnerID, result, ScanApplyOptions{})
	if err != nil || report.Inserted != 3 {
		t.Fatalf("index-only import: %+v, %v", report, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from file_changes where owner_id=$1::uuid`, root.OwnerID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unbound import invented changes: %d, %v", count, err)
	}
}
