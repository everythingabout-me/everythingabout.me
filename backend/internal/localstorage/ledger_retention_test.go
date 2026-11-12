package localstorage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func TestSettledLocalLedgerAdmissionAndRetention(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	ctx := context.Background()
	if _, err := w.Mkdir(ctx, storage.Mutation{ID: "folder", Path: "folder", Conditions: storage.Conditions{Absent: true}}, stageOK); err != nil {
		t.Fatal(err)
	}
	// A terminal action ledger and its permanent lock must not look like a possible
	// effect to admission.
	if err := CheckSettledSystemRoot(cfg.SystemRoot); err != nil {
		t.Fatalf("settled action record blocked admission: %v", err)
	}

	// A ready write ledger still holds an unpublished payload that can be published,
	// so it must block admission and survive cleanup until it settles.
	in := writeIntent("pending-write", "file", "bytes")
	staged := stageFile(t, w.StagedWriter, in, "bytes")
	if err := CheckSettledSystemRoot(cfg.SystemRoot); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("ready write ledger admitted: %v", err)
	}
	future := time.Now().Add(storage.IntentRetention + time.Minute)
	if count, err := PruneSettledSystemRoot(cfg.SystemRoot, future, true); err != nil || count != 1 {
		t.Fatalf("prune=%d err=%v, want only the settled action record", count, err)
	}
	if err := CheckSettledSystemRoot(cfg.SystemRoot); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("cleanup removed a non-terminal write record: %v", err)
	}

	if err := w.Discard(ctx, in, staged.Handle); err != nil {
		t.Fatal(err)
	}
	if err := CheckSettledSystemRoot(cfg.SystemRoot); err != nil {
		t.Fatalf("discarded write record blocked admission: %v", err)
	}
	if count, err := PruneSettledSystemRoot(cfg.SystemRoot, future, true); err != nil || count != 1 {
		t.Fatalf("prune=%d err=%v, want the settled write record", count, err)
	}
	if err := CheckSettledSystemRoot(cfg.SystemRoot); err != nil {
		t.Fatalf("empty settled namespace blocked admission: %v", err)
	}
	if count, err := PruneSettledSystemRoot(cfg.SystemRoot, future, true); err != nil || count != 0 {
		t.Fatalf("idempotent prune=%d err=%v", count, err)
	}
}

func TestSettledLocalLedgerLegacyRecordAndUnknownMaterial(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	namespace := filepath.Join(cfg.SystemRoot, w.namespace)
	legacy := actionLedger{
		Version: 1,
		Binding: w.driver.binding,
		Intent:  storage.EffectIntent{ID: "legacy", Action: "mkdir", Target: "legacy", Conditions: storage.Conditions{Absent: true}},
		Phase:   "applied",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(namespace, actionKey("legacy")+".json")
	if err = os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * storage.IntentRetention)
	if err = os.Chtimes(name, old, old); err != nil {
		t.Fatal(err)
	}
	if err = CheckSettledSystemRoot(cfg.SystemRoot); err != nil {
		t.Fatalf("legacy settled record blocked admission: %v", err)
	}
	if count, err := PruneSettledSystemRoot(cfg.SystemRoot, time.Now(), true); err != nil || count != 1 {
		t.Fatalf("legacy prune=%d err=%v", count, err)
	}
	if _, err = os.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("legacy settled record survived pruning: %v", err)
	}

	// Unknown spool material is neither accepted nor deleted: it could be evidence
	// this build does not understand.
	unknown := filepath.Join(namespace, "ordinary-untracked.data")
	if err = os.WriteFile(unknown, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = CheckSettledSystemRoot(cfg.SystemRoot); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("unknown spool material admitted: %v", err)
	}
	if _, err = PruneSettledSystemRoot(cfg.SystemRoot, time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(unknown); err != nil || !strings.EqualFold(string(data), "keep") {
		t.Fatalf("unknown spool material changed: %q %v", data, err)
	}
}
