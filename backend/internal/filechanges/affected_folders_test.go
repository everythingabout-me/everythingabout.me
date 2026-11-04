package filechanges

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
)

func TestAffectedFolderProjectionAndReplayFingerprint(t *testing.T) {
	folderID := "81995470-212d-4f40-934c-8435dba67a0f"
	privateID := "30711bd2-0bde-47d5-bd01-7792edc01fd1"
	folder := snapshot(folderID, "/Apps/Notes", "folder")
	folder.Revision = "nr_parent_after_membership"
	private := snapshot(privateID, "/Private", "folder")
	c := createChange(testOwner, "patches", "/Apps/Notes/a.txt")
	c.AffectedFolders = []fc.Metadata{*private, *folder, *snapshot(testRoot, "/", "folder")}
	projected, visible := Project(c, "/Apps/Notes", NameKey(true))
	if !visible || len(projected.AffectedFolders) != 1 {
		t.Fatalf("visible patches %+v visible=%v", projected, visible)
	}
	patch := projected.AffectedFolders[0]
	if patch.ID != folder.ID || patch.Revision != folder.Revision || patch.ParentID != nil || patch.ParentPath != "" {
		t.Fatalf("patch lost captured state or concealed its parent incorrectly: %+v", patch)
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "/Private") || strings.Contains(string(encoded), privateID) {
		t.Fatalf("concealed patch leaked: %s", encoded)
	}
	if folder.ParentID == nil || c.AffectedFolders[1].ParentID == nil {
		t.Fatal("projection changed the retained patch")
	}
	first, err := normalizeChange(c)
	if err != nil {
		t.Fatal(err)
	}
	firstHash, err := changeFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(c.AffectedFolders)
	second, err := normalizeChange(c)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := changeFingerprint(second)
	if err != nil || firstHash != secondHash {
		t.Fatal("ancestor collection order changed immutable intent", err)
	}
	second.AffectedFolders[0].Revision = "different captured revision"
	changedHash, err := changeFingerprint(second)
	if err != nil || changedHash == firstHash {
		t.Fatal("changed folder patch was omitted from effect fingerprint", err)
	}
	invalid := c
	invalid.AffectedFolders = []fc.Metadata{*snapshot(testEntry, "/Apps/Notes/a.txt", "file")}
	if _, err := normalizeChange(invalid); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("non-folder patch accepted: %v", err)
	}
}

func TestPostgresAffectedFolderSnapshots(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	folder := snapshot(testUUID(t), "/Apps/Notes", "folder")
	folder.Revision = "nr_captured_membership"
	c := createChange(owner, "captured-folders", "/Apps/Notes/a.txt")
	c.AffectedFolders = []fc.Metadata{*snapshot(testRoot, "/", "folder"), *folder}
	position := appendCommitted(t, pool, c)
	rows, err := ReadScoped(ctx, pool, owner, "/Apps/Notes", true, 0, position, 10)
	if err != nil || len(rows) != 1 || len(rows[0].AffectedFolders) != 2 {
		t.Fatalf("stored folder patches %+v %v", rows, err)
	}
	projection, visible := Project(rows[0], "/Apps/Notes", NameKey(true))
	if !visible || len(projection.AffectedFolders) != 1 || projection.AffectedFolders[0].Revision != folder.Revision {
		t.Fatalf("captured patch projection %+v visible=%v", projection, visible)
	}
	// Reapplying the additive migration must neither reset patches nor their event.
	migration, err := os.ReadFile("../../../sql/028_file_changes.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, string(migration)); err != nil {
		t.Fatal(err)
	}
	slices.Reverse(c.AffectedFolders)
	if replay := appendCommitted(t, pool, c); replay != position {
		t.Fatalf("reordered patch replay allocated %d", replay)
	}
	rows, err = Read(ctx, pool, owner, 0, position, 10)
	if err != nil || len(rows) != 1 || rows[0].AffectedFolders[1].Revision != folder.Revision {
		t.Fatalf("migration/replay changed retained patches %+v %v", rows, err)
	}
	c.AffectedFolders[0].Revision = "newer live membership must not replace the saved patch"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = Append(ctx, tx, c); !errors.Is(err, ErrEffectConflict) {
		t.Fatalf("different retained patch replay accepted: %v", err)
	}
}
