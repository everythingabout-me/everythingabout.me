package objectmutation

import (
	"bytes"
	"context"
	"errors"

	"testing"

	"everythingabout.me/backend/internal/storage"
)

// A copy effect's identity is the source revision; Size and MediaType only describe the
// object the copy produces. The coordinator rebuilds that description from the frozen
// manifest on every inspection, so a ledger that recorded the effect without it refused
// every interrupted copy with intent_conflict — an error no retry can ever change, which
// is how one lost reply pinned a whole move.
func TestCopyInspectionAcceptsTheManifestDescription(t *testing.T) {
	d, _, _ := copyFixture(t)
	ctx := context.Background()
	source := publishSource(t, d, "source", "source", "payload")

	// Interrupt the copy right after its "ready" receipt: no native request was issued.
	var ready []byte
	interrupted := errors.New("checkpoint reply lost")
	request := storage.MoveRequest{ID: "copy", Source: "source", Target: "copy", SourceRevision: source.Revision, TargetConditions: storage.Conditions{Absent: true}, Size: source.Size}
	if _, err := d.Copy(ctx, request, func(_ context.Context, value []byte) error {
		ready = bytes.Clone(value)
		return interrupted
	}); err == nil {
		t.Fatal("interrupted copy reported success")
	}

	// The coordinator's intent, rebuilt from the frozen manifest, carries the source size.
	effect := storage.EffectIntent{ID: "copy", Action: "copy", Source: "source", Target: "copy", Conditions: storage.Conditions{Absent: true}, SourceRevision: source.Revision, Size: source.Size}
	inspection, err := d.InspectEffect(ctx, effect, ready)
	if storage.IsCode(err, "intent_conflict") {
		t.Fatalf("inspection refused the manifest's own description of the effect: %v", err)
	}
	if err != nil || inspection.State != storage.NotApplied {
		t.Fatalf("inspection state=%v err=%v, want not_applied", inspection.State, err)
	}

	// A ledger written before the description was recorded has to adopt it instead of
	// refusing forever; those records are what an interrupted operation already holds.
	r, err := d.load("copy", "copy")
	if err != nil {
		t.Fatal(err)
	}
	r.Effect.Size = 0
	r.Effect.MediaType = ""
	r.Phase = "ready"
	if err = d.save(r); err != nil {
		t.Fatal(err)
	}
	inspection, err = d.InspectEffect(ctx, effect, ready)
	if err != nil || inspection.State != storage.NotApplied {
		t.Fatalf("legacy inspection state=%v err=%v, want not_applied", inspection.State, err)
	}
	upgraded, err := d.load("copy", "copy")
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Effect.Size != source.Size {
		t.Fatalf("legacy effect size = %d, want %d", upgraded.Effect.Size, source.Size)
	}

	// A real disagreement about the effect is still a conflict: adopting the description
	// must not erase the identity the copy was admitted with.
	conflict := effect
	conflict.Source = "other"
	if _, err = d.InspectEffect(ctx, conflict, ready); !storage.IsCode(err, "intent_conflict") {
		t.Fatalf("changed source error=%v, want intent_conflict", err)
	}
}
