package objectmutation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func TestObjectSettledLedgerAdmissionAndRetention(t *testing.T) {
	d, _, cfg, options := fixture(t)
	ctx := context.Background()
	if _, err := d.Mkdir(ctx, storage.Mutation{ID: "folder", Path: "folder", Conditions: storage.Conditions{Absent: true}}, cpOK); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpoolSettled(cfg, options); err != nil {
		t.Fatalf("settled mkdir record blocked admission: %v", err)
	}
	// A non-terminal record is never aged out, even by a far-future clock.
	pending := &ledger{Kind: "write", ID: "pending", Phase: "publishing"}
	if err := d.save(pending); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpoolSettled(cfg, options); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("publishing record admitted: %v", err)
	}
	future := time.Now().Add(storage.IntentRetention + time.Minute)
	if count, err := PruneSettled(cfg, options, future, true); err != nil || count != 1 {
		t.Fatalf("prune=%d err=%v, want only the settled mkdir record", count, err)
	}
	if err := CheckSpoolSettled(cfg, options); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("cleanup removed a non-terminal record: %v", err)
	}
	if err := d.root.Remove(keyFor("write", "pending") + ".json"); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpoolSettled(cfg, options); err != nil {
		t.Fatalf("empty settled namespace blocked admission: %v", err)
	}
}

func TestObjectSettledLedgerRemovesPayloadBeforeRecord(t *testing.T) {
	d, _, cfg, options := fixture(t)
	attempt := "00112233445566778899aabbccddeeff"
	settled := &ledger{Kind: "write", ID: "settled", Attempt: attempt, Phase: "not_applied"}
	if err := d.save(settled); err != nil {
		t.Fatal(err)
	}
	payload := keyFor(settled.Kind, settled.ID) + "-" + attempt + ".data"
	if err := d.root.WriteFile(payload, []byte("private bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(storage.IntentRetention + time.Minute)
	if count, err := PruneSettled(cfg, options, future, true); err != nil || count != 1 {
		t.Fatalf("prune=%d err=%v", count, err)
	}
	if _, err := d.root.Stat(payload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled payload survived: %v", err)
	}
	if _, err := d.root.Stat(keyFor(settled.Kind, settled.ID) + ".json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settled record survived: %v", err)
	}
	// A rerun after the first pass removes nothing and does not fail on the missing
	// payload, so an interrupted cleanup can always be retried.
	if count, err := PruneSettled(cfg, options, future, true); err != nil || count != 0 {
		t.Fatalf("idempotent prune=%d err=%v", count, err)
	}
}

func TestObjectSettledLedgerUsesModTimeForLegacyRecords(t *testing.T) {
	d, _, cfg, options := fixture(t)
	legacy := &ledger{Format: 1, Binding: d.namespace, Kind: "write", ID: "legacy", Phase: "applied"}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	name := keyFor(legacy.Kind, legacy.ID) + ".json"
	if err = d.root.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * storage.IntentRetention)
	if err = d.root.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	if err = CheckSpoolSettled(cfg, options); err != nil {
		t.Fatalf("legacy settled record blocked admission: %v", err)
	}
	if count, err := PruneSettled(cfg, options, time.Now(), true); err != nil || count != 1 {
		t.Fatalf("legacy prune=%d err=%v", count, err)
	}
	if _, err = d.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy settled record survived pruning: %v", err)
	}
}

func TestObjectUnknownSpoolMaterialIsNeitherAdmittedNorPruned(t *testing.T) {
	d, _, cfg, options := fixture(t)
	name := "ordinary-untracked.data"
	if err := d.root.WriteFile(name, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpoolSettled(cfg, options); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("unknown spool material admitted: %v", err)
	}
	future := time.Now().Add(10 * storage.IntentRetention)
	if _, err := PruneSettled(cfg, options, future, true); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(options.SpoolRoot, d.namespace, name)); err != nil || string(data) != "keep" {
		t.Fatalf("unknown spool material changed: %q %v", data, err)
	}
}

func TestObjectCorruptLedgerBlocksAdmissionAndSurvivesPrune(t *testing.T) {
	d, _, cfg, options := fixture(t)
	name := keyFor("write", "corrupt") + ".json"
	if err := d.root.WriteFile(name, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckSpoolSettled(cfg, options); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("corrupt record admitted: %v", err)
	}
	if _, err := PruneSettled(cfg, options, time.Now().Add(10*storage.IntentRetention), true); err != nil {
		t.Fatal(err)
	}
	if _, err := d.root.Stat(name); err != nil {
		t.Fatalf("corrupt record removed: %v", err)
	}
}
