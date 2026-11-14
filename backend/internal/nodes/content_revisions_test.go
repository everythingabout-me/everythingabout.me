package nodes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/filerevision"
	"everythingabout.me/backend/internal/storage"
)

func TestContentGenerationTransfersAndInvalidation(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)
	source := storage.Entry{Path: "from/a.txt", Kind: storage.File, Size: 3, Revision: []byte("captured-version"), NativeRef: []byte("source-inode"), ContentIdentity: []byte("source-content")}
	indexActionEntry(t, pool, root, "from", true)
	id := indexActionEntry(t, pool, root, string(source.Path), false)
	first := filerevision.Content(fileRootNamespace(root), source)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = saveContentRevision(ctx, tx, root, source, first, nil); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	copied := actionEffect(1, "copy", string(source.Path), "to/a.txt", storage.File)
	copied.SourceEntry = &source
	removed := actionEffect(2, "delete", string(source.Path), "", storage.File)
	removed.SourceEntry = &source
	runActionCommit(t, pool, actionOperation(root, "move", "from", "to", actionEffect(0, "mkdir", "", "to", storage.Folder), copied, removed, actionEffect(3, "delete", "from", "", storage.Folder)))
	var targetResult storage.MutationResult
	if err = json.Unmarshal(copied.Result, &targetResult); err != nil {
		t.Fatal(err)
	}
	target := *targetResult.Entry
	got, err := lookupContentRevision(ctx, pool, root.OwnerID, fileRootNamespace(root), target)
	if err != nil || got == nil || *got != *first {
		t.Fatalf("moved descendant generation=%v want=%v error=%v", got, first, err)
	}
	assertActionID(t, pool, root, string(target.Path), id)
	var count int
	if err = pool.QueryRow(ctx, `select count(*) from file_content_generations where owner_id=$1::uuid and path=$2`, root.OwnerID, string(source.Path)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("source generation remains: %d %v", count, err)
	}
	// A second move transfers an existing mapped generation rather than recomputing
	// the intermediate native revision from its new inode.
	second := actionEffect(0, "copy", string(target.Path), "again.txt", storage.File)
	second.SourceEntry = &target
	second.SourceRevision = target.Revision
	secondDelete := actionEffect(1, "delete", string(target.Path), "", storage.File)
	secondDelete.SourceEntry = &target
	secondDelete.SourceRevision = target.Revision
	raw := runActionCommit(t, pool, actionOperation(root, "move", string(target.Path), "again.txt", second, secondDelete))
	var m fc.Metadata
	if err = json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ContentRevision == nil || *m.ContentRevision != *first {
		t.Fatalf("second move lost generation: %s", raw)
	}
	var secondResult storage.MutationResult
	json.Unmarshal(second.Result, &secondResult)
	current := *secondResult.Entry
	changed := current
	changed.Revision = []byte("external-replacement")
	got, err = lookupContentRevision(ctx, pool, root.OwnerID, fileRootNamespace(root), changed)
	if err != nil || got == nil || *got == *first || *got != *filerevision.Content(fileRootNamespace(root), changed) {
		t.Fatalf("external replacement inherited generation: %v %v", got, err)
	}
	got, err = lookupContentRevision(ctx, pool, root.OwnerID, "another-root", current)
	if err != nil || got == nil || *got != *filerevision.Content("another-root", current) {
		t.Fatalf("root namespace leaked: %v %v", got, err)
	}
	other := actionOwner(t, pool)
	got, err = lookupContentRevision(ctx, pool, other.OwnerID, fileRootNamespace(root), current)
	if err != nil || got == nil || *got == *first {
		t.Fatalf("owner generation leaked: %v %v", got, err)
	}
	copyOnly := actionEffect(0, "copy", string(current.Path), "independent.txt", storage.File)
	copyOnly.SourceEntry = &current
	copyOnly.SourceRevision = current.Revision
	copyRaw := runActionCommit(t, pool, actionOperation(root, "copy", string(current.Path), "independent.txt", copyOnly))
	var independent fc.Metadata
	json.Unmarshal(copyRaw, &independent)
	if independent.ContentRevision == nil || *independent.ContentRevision == *first {
		t.Fatalf("copy inherited move generation: %s", copyRaw)
	}
}

func TestFinalFolderObservationAndNoopMetadata(t *testing.T) {
	pool := actionTestDatabase(t)
	ctx := context.Background()
	root := actionOwner(t, pool)
	mkdir := actionEffect(0, "mkdir", "", "folder", storage.Folder)
	child := actionEffect(1, "copy", "source.txt", "folder/a.txt", storage.File)
	var created storage.MutationResult
	json.Unmarshal(mkdir.Result, &created)
	final := *created.Entry
	final.ModifiedAt = final.ModifiedAt.Add(time.Second)
	observation := fileops.ActionObservation{Entry: final, Children: []storage.Entry{{Path: "folder/a.txt", Kind: storage.File}}}
	evidence, _ := json.Marshal(observation)
	native := &storage.MutationResult{Entry: &final, Evidence: evidence}
	commit := func(op fileops.Operation, native *storage.MutationResult) json.RawMessage {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		raw, err := CommitFileAction(ctx, tx, op, native)
		if err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	raw := commit(actionOperation(root, "copy", "source", "folder", mkdir, child), native)
	var m fc.Metadata
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.SourceModifiedAt == nil || !m.SourceModifiedAt.Equal(final.ModifiedAt.Truncate(time.Millisecond)) {
		t.Fatalf("response used stale mkdir observation: %s", raw)
	}
	record, err := fileBrowseMetadata(ctx, pool, root.OwnerID, fileRootNamespace(root), "folder", nil)
	if err != nil || record == nil {
		t.Fatalf("folder membership %+v %v", record, err)
	}
	expected, err := IndexedMetadata(fileRootNamespace(root), *record)
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != expected.Revision || m.ChangeToken != expected.ChangeToken {
		t.Fatalf("final folder metadata differs from browsing: response=%s expected=%+v", raw, expected)
	}
	// Existing mkdir observes a native folder; no index-only namespace effect is
	// needed and repeated observations preserve application timestamps/revision.
	op := actionOperation(root, "mkdir", "", "folder")
	first := commit(op, native)
	second := commit(op, native)
	if string(first) != string(second) {
		t.Fatalf("verified noop changed metadata:\n%s\n%s", first, second)
	}
	var indexed time.Time
	if err := pool.QueryRow(ctx, `select source_modified_at from nodes where owner_id=$1::uuid and path='folder'`, root.OwnerID).Scan(&indexed); err != nil || !indexed.Equal(final.ModifiedAt) {
		t.Fatalf("final folder index mtime: %s %v", indexed, err)
	}
	// Malformed final children must atomically fail settlement.
	observation.Children = []storage.Entry{{Path: "outside.txt", Kind: storage.File}}
	invalid, _ := json.Marshal(observation)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = CommitFileAction(ctx, tx, op, &storage.MutationResult{Entry: &final, Evidence: invalid}); !fileops.IsCode(err, "invalid_native_result") {
		t.Fatalf("outside child accepted: %v", err)
	}
}
