package objectmutation

import (
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// A zero-byte folder marker's ETag is the empty-content MD5 on COS/S3: rewriting the same
// empty bytes cannot move the revision a purge compares. Reassert therefore writes a short
// token into the marker, changing its ETag; the purge's conditional delete with the old
// ETag must then refuse it.
func TestReassertMovesFolderMarkerRevision(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	b.emptyETag = `"d41d8cd98f00b204e9800998ecf8427e"`
	ctx := context.Background()
	if _, err := d.Mkdir(ctx, storage.Mutation{ID: "create", Path: "Folder", Conditions: storage.Conditions{Absent: true}}, cpOK); err != nil {
		t.Fatal(err)
	}
	before, err := d.Stat(ctx, "Folder")
	if err != nil || before.Kind != storage.Folder {
		t.Fatalf("folder stat %+v %v", before, err)
	}
	if string(before.Revision) != "native-etag:\"d41d8cd98f00b204e9800998ecf8427e\"" {
		t.Fatalf("fixture did not model the empty-marker ETag: %q", before.Revision)
	}
	after, err := d.Reassert(ctx, storage.Mutation{ID: "reassert", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: before.Revision}}, cpOK)
	if err != nil || after.Entry == nil {
		t.Fatalf("reassert %+v %v", after, err)
	}
	if string(after.Entry.Revision) == string(before.Revision) {
		t.Fatalf("reassert left the empty-marker ETag unchanged: %q", after.Entry.Revision)
	}
	if after.Entry.Kind != storage.Folder || after.Entry.Size != 0 {
		t.Fatalf("token marker stopped being a folder: %+v", after.Entry)
	}
	// The purge's stale conditional delete must refuse, which the purge maps to adopted.
	if err := d.Purge(ctx, "Folder", storage.Folder, before.Revision); !storage.IsCode(err, "changed") {
		t.Fatalf("stale purge delete was not refused: %v", err)
	}
	_, delErr := b.Delete(ctx, cfg.ContentRoot+"Folder/", `"d41d8cd98f00b204e9800998ecf8427e"`)
	if !storage.IsCode(delErr, "precondition_failed") {
		t.Fatalf("stale conditional delete was not refused: %v", delErr)
	}
	if _, err = d.Stat(ctx, "Folder"); err != nil {
		t.Fatalf("reassert removed the folder: %v", err)
	}
}

func TestReassertConditionsAndAbsentMarker(t *testing.T) {
	d, b, cfg, _ := fixture(t)
	b.emptyETag = `"d41d8cd98f00b204e9800998ecf8427e"`
	ctx := context.Background()
	if _, err := d.Mkdir(ctx, storage.Mutation{ID: "create", Path: "Folder", Conditions: storage.Conditions{Absent: true}}, cpOK); err != nil {
		t.Fatal(err)
	}
	before, err := d.Stat(ctx, "Folder")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Reassert(ctx, storage.Mutation{ID: "stale", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: nativeRevision(`"other"`)}}, cpOK); !storage.IsCode(err, "changed") {
		t.Fatalf("stale expected revision accepted: %v", err)
	}
	// The purge wins: the marker is already gone, and the create must still end native.
	if _, err = b.Delete(ctx, cfg.ContentRoot+"Folder/", `"d41d8cd98f00b204e9800998ecf8427e"`); err != nil {
		t.Fatal(err)
	}
	created, err := d.Reassert(ctx, storage.Mutation{ID: "after-purge", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: before.Revision}}, cpOK)
	if err != nil || created.Entry == nil || created.Entry.Kind != storage.Folder {
		t.Fatalf("absent-marker reassert did not create a folder: %+v %v", created, err)
	}
	if _, err = d.Stat(ctx, "Folder"); err != nil {
		t.Fatalf("marker missing after absent-marker reassert: %v", err)
	}
}
