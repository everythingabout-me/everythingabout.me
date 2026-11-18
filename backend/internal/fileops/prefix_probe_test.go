package fileops

import (
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// probeNative is the smallest native that validates one native-tree move: one
// source folder, every other path absent, and an optional destination prefix probe.
// The Stat counter is what makes the saving visible: without the probe each planned
// target costs its own three-request absence proof.
type probeNative struct {
	NativeActions
	statByPath map[storage.Path]int
	probes     int
	probePath  storage.Path
	occupied   bool
	probeErr   error
}

func (n *probeNative) StorageNameRules() storage.NameRules {
	return storage.NameRules{CaseSensitive: true}
}

func (n *probeNative) Stat(_ context.Context, p storage.Path) (storage.Entry, error) {
	if n.statByPath == nil {
		n.statByPath = map[storage.Path]int{}
	}
	n.statByPath[p]++
	if p == "from" {
		return storage.Entry{Path: p, Kind: storage.Folder, Revision: []byte("rev:from")}, nil
	}
	return storage.Entry{}, storage.NewError("not_found", p, nil)
}

func (n *probeNative) PrefixEmpty(_ context.Context, p storage.Path) (bool, error) {
	n.probes++
	n.probePath = p
	if n.probeErr != nil {
		return false, n.probeErr
	}
	return !n.occupied, nil
}

// plainNative is the same native without the optional capability. The coordinator
// must keep its per-target checks instead of failing the action.
type plainNative struct {
	NativeActions
	inner *probeNative
}

func (n *plainNative) StorageNameRules() storage.NameRules { return n.inner.StorageNameRules() }
func (n *plainNative) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	return n.inner.Stat(ctx, p)
}

func probeCapture(t *testing.T, native NativeActions) error {
	t.Helper()
	binding := actionBinding{
		Strategy: nativeTreeMoveStrategy,
		Sources:  []storage.Entry{{Path: "from", Kind: storage.Folder, Revision: []byte("rev:from")}},
	}
	manifest := []ManifestEntry{{Target: "to/a"}, {Target: "to/b"}, {Target: "to/c"}}
	return validateCapture(context.Background(), ActionRequest{Kind: "move", From: "from", To: "to"}, native, binding, manifest)
}

func TestValidateCaptureProbesDestinationOnce(t *testing.T) {
	native := &probeNative{}
	if err := probeCapture(t, native); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if native.probes != 1 || native.probePath != "to" {
		t.Fatalf("destination probe = %d call(s) for %q, want 1 for %q", native.probes, native.probePath, "to")
	}
	for _, target := range []storage.Path{"to/a", "to/b", "to/c"} {
		if native.statByPath[target] != 0 {
			t.Fatalf("target %q was observed %d time(s); the destination probe already proved it absent", target, native.statByPath[target])
		}
	}
}

func TestValidateCaptureKeepsPerTargetChecksWithoutProbe(t *testing.T) {
	inner := &probeNative{}
	if err := probeCapture(t, &plainNative{inner: inner}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if inner.probes != 0 {
		t.Fatalf("a native without the capability was probed %d time(s)", inner.probes)
	}
	for _, target := range []storage.Path{"to/a", "to/b", "to/c"} {
		if inner.statByPath[target] != 1 {
			t.Fatalf("target %q was observed %d time(s), want 1", target, inner.statByPath[target])
		}
	}
}

func TestValidateCaptureFallsBackWhenProbeIsUnsupported(t *testing.T) {
	native := &probeNative{probeErr: storage.NewError("unsupported", "to", nil)}
	if err := probeCapture(t, native); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, target := range []storage.Path{"to/a", "to/b", "to/c"} {
		if native.statByPath[target] != 1 {
			t.Fatalf("target %q was observed %d time(s), want 1", target, native.statByPath[target])
		}
	}
}

func TestValidateCaptureRefusesOccupiedDestination(t *testing.T) {
	native := &probeNative{occupied: true}
	if err := probeCapture(t, native); !IsCode(err, "target_changed") {
		t.Fatalf("occupied destination: %v, want target_changed", err)
	}
	if native.probes != 1 {
		t.Fatalf("occupied destination was probed %d time(s), want 1", native.probes)
	}
}
