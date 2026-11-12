package storagebackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

func TestVerifyEmptyOwnerAcceptsSettledLocalLedgers(t *testing.T) {
	b, cfg := coordinatedLocalConfig(t)
	owner := "settled-local-owner"
	addEmptyLocalOwner(t, cfg, owner)
	driver := openOwner(t, b, owner)
	ctx := context.Background()
	writer, ok := driver.(storage.StagedWriter)
	if !ok {
		t.Fatal("coordinated local driver is not a staged writer")
	}
	folders, ok := driver.(storage.FolderWriter)
	if !ok {
		t.Fatal("coordinated local driver is not a folder writer")
	}
	remover, ok := driver.(storage.Remover)
	if !ok {
		t.Fatal("coordinated local driver is not a remover")
	}
	made, err := folders.Mkdir(ctx, storage.Mutation{ID: "folder", Path: "folder", Conditions: storage.Conditions{Absent: true}}, func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = remover.Remove(ctx, storage.Mutation{ID: "remove", Path: "folder", Conditions: storage.Conditions{ExpectedRevision: made.Entry.Revision}}, func(context.Context, []byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The previous lifetime left two settled action records; they prove no effect can
	// still land, so they must not block the only automatic baseline.
	if err = b.VerifyEmptyOwner(ctx, owner, driver); err != nil {
		t.Fatalf("settled action ledgers blocked admission: %v", err)
	}

	intent := storage.WriteIntent{ID: "pending-write", Target: "file", Conditions: storage.Conditions{Absent: true}, Size: 5, MediaType: "text/plain"}
	staged, err := writer.Stage(ctx, intent, strings.NewReader("bytes"), func(context.Context, []byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// Ready staging can still publish, so admission must refuse it.
	if err = b.VerifyEmptyOwner(ctx, owner, driver); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("ready staging admitted: %v", err)
	}
	if err = writer.Discard(ctx, intent, staged.Handle); err != nil {
		t.Fatal(err)
	}
	if err = b.VerifyEmptyOwner(ctx, owner, driver); err != nil {
		t.Fatalf("discarded staging blocked admission: %v", err)
	}

	future := time.Now().Add(storage.IntentRetention + time.Minute)
	count, err := b.PruneSettled(ctx, owner, future, true)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("pruned %d settled records, want the two actions and one write", count)
	}
	if err = b.VerifyEmptyOwner(ctx, owner, driver); err != nil {
		t.Fatalf("pruned namespace blocked admission: %v", err)
	}
}

func TestObjectBaselineAcceptsSettledLedger(t *testing.T) {
	b := objectBaselineBackend(t, func(key string) (objectmutation.Object, error) {
		return objectmutation.Object{Key: key}, nil
	})
	binding, err := b.Binding("owner")
	if err != nil {
		t.Fatal(err)
	}
	namespace := spoolNamespaceForTest(t, binding, b.objectOptions.SpoolRoot)
	write := func(phase string) string {
		name := testKeyFor("write", "pending") + ".json"
		data, err := json.Marshal(map[string]any{"Format": 1, "Binding": filepath.Base(namespace), "Kind": "write", "ID": "pending", "Phase": phase})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(namespace, name), data, 0600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	if name := write("applied"); name == "" {
		t.Fatal("record name empty")
	}
	if err := b.ValidateObjectBaseline(context.Background(), "owner", nil); err != nil {
		t.Fatalf("settled spool blocked baseline: %v", err)
	}
	if name := write("publishing"); name == "" {
		t.Fatal("record name empty")
	}
	if err := b.ValidateObjectBaseline(context.Background(), "owner", nil); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("non-terminal spool admitted: %v", err)
	}
}

func spoolNamespaceForTest(t *testing.T, binding storage.Config, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	binding.Credentials = nil
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	namespace := filepath.Join(root, "object-mutations-v1-"+hex.EncodeToString(hash[:]))
	if err = os.Mkdir(namespace, 0700); err != nil {
		t.Fatal(err)
	}
	return namespace
}

func testKeyFor(kind, id string) string {
	hash := sha256.Sum256([]byte(kind + "\x00" + id))
	return hex.EncodeToString(hash[:])
}
