//go:build darwin || linux

package localstorage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func reassertCheckpoint(context.Context, []byte) error { return nil }

// A local folder's revision is its directory identity, so a reassert replaces the inode;
// rename over an empty directory is the one native effect that can do that without a
// window where the path is missing for a reader of the new content.
func TestReassertReplacesLocalFolderIdentity(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	ctx := context.Background()
	if _, err := w.Mkdir(ctx, storage.Mutation{ID: "create", Path: "Folder", Conditions: storage.Conditions{Absent: true}}, reassertCheckpoint); err != nil {
		t.Fatal(err)
	}
	before := mutationStat(t, w, "Folder")
	beforeInfo, err := os.Stat(filepath.Join(cfg.ContentRoot, "Folder"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := w.Reassert(ctx, storage.Mutation{ID: "reassert", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: before.Revision}}, reassertCheckpoint)
	if err != nil || after.Entry == nil {
		t.Fatalf("reassert %+v %v", after, err)
	}
	if string(after.Entry.Revision) == string(before.Revision) || string(after.Entry.Revision) == "" {
		t.Fatalf("reassert did not move the directory identity: %q -> %q", before.Revision, after.Entry.Revision)
	}
	afterInfo, err := os.Stat(filepath.Join(cfg.ContentRoot, "Folder"))
	if err != nil || os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("reassert kept the old directory inode: %v", err)
	}
	if _, err = w.Reassert(ctx, storage.Mutation{ID: "stale", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: before.Revision}}, reassertCheckpoint); !storage.IsCode(err, "changed") {
		t.Fatalf("stale expected identity accepted: %v", err)
	}
}

// Reasserting a marker must never delete user content. A folder that gained a child keeps
// its inode and its child; the caller treats not_empty as already protected.
func TestReassertRefusesNonEmptyFolder(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	ctx := context.Background()
	if _, err := w.Mkdir(ctx, storage.Mutation{ID: "create", Path: "Folder", Conditions: storage.Conditions{Absent: true}}, reassertCheckpoint); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(cfg.ContentRoot, "Folder", "child.txt")
	if err := os.WriteFile(child, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	before := mutationStat(t, w, "Folder")
	beforeInfo, err := os.Stat(filepath.Join(cfg.ContentRoot, "Folder"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Reassert(ctx, storage.Mutation{ID: "reassert", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: before.Revision}}, reassertCheckpoint); !storage.IsCode(err, "not_empty") {
		t.Fatalf("non-empty folder reasserted: %v", err)
	}
	data, err := os.ReadFile(child)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("reassert touched user content: %q %v", data, err)
	}
	afterInfo, err := os.Stat(filepath.Join(cfg.ContentRoot, "Folder"))
	if err != nil || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("refused reassert replaced the directory")
	}
}

// The purge may already have removed the marker when the create reaches the fence; the
// create must still end with a native folder.
func TestReassertCreatesMissingFolder(t *testing.T) {
	w, _, _ := mutationFixture(t)
	ctx := context.Background()
	created, err := w.Reassert(ctx, storage.Mutation{ID: "after-purge", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: []byte("native-version:gone")}}, reassertCheckpoint)
	if err != nil || created.Entry == nil || created.Entry.Kind != storage.Folder {
		t.Fatalf("absent-marker reassert did not create a folder: %+v %v", created, err)
	}
	if _, err = w.Stat(ctx, "Folder"); err != nil {
		t.Fatalf("marker missing after reassert: %v", err)
	}
}
