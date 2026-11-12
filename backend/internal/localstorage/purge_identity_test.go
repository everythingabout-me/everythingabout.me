//go:build darwin || linux

package localstorage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// ledgerRecords counts the settled action intents this driver has written. A retried
// purge must reuse its intent, so this number must not grow on the retry.
func ledgerRecords(t *testing.T, w *MutationDriver, cfg storage.Config) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cfg.SystemRoot, w.namespace))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "action-") && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count
}

func purgeFolder(t *testing.T, w *MutationDriver, cfg storage.Config, mkdirID, removeID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := w.Mkdir(ctx, storage.Mutation{ID: mkdirID, Path: "Folder", Conditions: storage.Conditions{Absent: true}}, stageOK); err != nil {
		t.Fatalf("mkdir %s: %v", mkdirID, err)
	}
	revision := mutationStat(t, w, "Folder").Revision
	if _, err := w.Remove(ctx, storage.Mutation{ID: removeID, Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: revision}}, stageOK); err != nil {
		t.Fatalf("remove %s: %v", removeID, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "Folder")); !os.IsNotExist(err) {
		t.Fatalf("remove %s left the folder: %v", removeID, err)
	}
}

// A purge intent names the accepted deletion, not the path. The Local driver journals
// every removal, and the ledger refuses a second operation under one id, so deleting a
// folder, re-creating it and deleting it again must produce two intents. A path-keyed id
// made the second purge answer intent_conflict while the re-created folder stayed on disk.
func TestLocalPurgeIntentNamesTheOperation(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	// The generic purge carries no operation identity to journal. Local must take the
	// caller's named removal instead; claiming storage.Purger is how a path-keyed id
	// would come back.
	if _, ok := interface{}(w).(storage.Purger); ok {
		t.Fatal("Local driver claims storage.Purger; a purge would have to name itself by path")
	}
	purgeFolder(t, w, cfg, "op-1/mkdir", "op-1/purge/Folder")
	settled := ledgerRecords(t, w, cfg)
	purgeFolder(t, w, cfg, "op-2/mkdir", "op-2/purge/Folder")
	if got := ledgerRecords(t, w, cfg); got != settled+2 {
		t.Fatalf("delete/create/delete wrote %d intents, want %d", got, settled+2)
	}
}

// Re-running the same purge of the same operation must report the applied result without
// reserving a second intent. This is the retry a lost reply or a restarted worker
// produces, and a second intent would make the ledger's own identity ambiguous.
func TestLocalPurgeRetryReusesOneIntent(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	ctx := context.Background()
	mkdir := storage.Mutation{ID: "op-1/mkdir", Path: "Folder", Conditions: storage.Conditions{Absent: true}}
	if _, err := w.Mkdir(ctx, mkdir, stageOK); err != nil {
		t.Fatal(err)
	}
	revision := mutationStat(t, w, "Folder").Revision
	remove := storage.Mutation{ID: "op-1/purge/Folder", Path: "Folder", Conditions: storage.Conditions{ExpectedRevision: revision}}
	if _, err := w.Remove(ctx, remove, stageOK); err != nil {
		t.Fatal(err)
	}
	settled := ledgerRecords(t, w, cfg)
	if _, err := w.Remove(ctx, remove, stageOK); err != nil {
		t.Fatalf("retried purge: %v", err)
	}
	if got := ledgerRecords(t, w, cfg); got != settled {
		t.Fatalf("retried purge added %d intents, want the same %d", got-settled, settled)
	}
	if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "Folder")); !os.IsNotExist(err) {
		t.Fatalf("retried purge left the folder: %v", err)
	}
}
