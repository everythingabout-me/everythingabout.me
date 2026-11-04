package fileops

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// discardJournal is a cleanup-capable journal fixture: it records the transition to
// cleaned so the reconcile flow can be observed end to end without Postgres guards.
type discardJournal struct {
	Journal
	op       Operation
	calls    []string
	cleanErr error
}

func (j *discardJournal) Get(ctx context.Context, root Root, actor Actor, id string) (Operation, error) {
	j.calls = append(j.calls, "get")
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	if id != j.op.ID || root != j.op.Root || actor.ID != j.op.PrincipalID {
		return Operation{}, E("forbidden", nil)
	}
	return cloneOperation(j.op), nil
}
func (j *discardJournal) CleanupCandidates(context.Context, int) ([]Operation, error) {
	return []Operation{cloneOperation(j.op)}, nil
}
func (j *discardJournal) MarkCleaned(_ context.Context, root Root, _ Actor, id string) error {
	j.calls = append(j.calls, "cleaned")
	if root != j.op.Root || id != j.op.ID {
		return E("forbidden", nil)
	}
	if j.cleanErr != nil {
		return j.cleanErr
	}
	now := time.Now()
	j.op.CleanedAt = &now
	return nil
}

// discardWriter stands in for the numbered native writer. Refusing the first discard
// models a retained uncertain receipt; the prover is what lets the retry pass.
type discardWriter struct {
	binding    storage.Config
	names      storage.NameRules
	calls      []string
	discardErr error
	evidence   storage.DiscardEvidence
	proveErr   error
}

func (w *discardWriter) StorageBinding() storage.Config      { return w.binding }
func (w *discardWriter) StorageNameRules() storage.NameRules { return w.names }
func (w *discardWriter) Stage(context.Context, storage.WriteIntent, io.Reader, storage.Checkpoint) (storage.Staged, error) {
	return storage.Staged{}, E("unexpected", nil)
}
func (w *discardWriter) Publish(context.Context, storage.Staged, storage.Checkpoint) (storage.MutationResult, error) {
	return storage.MutationResult{}, E("unexpected", nil)
}
func (w *discardWriter) Inspect(context.Context, storage.WriteIntent, []byte) (storage.Inspection, error) {
	return storage.Inspection{}, E("unexpected", nil)
}
func (w *discardWriter) Discard(_ context.Context, _ storage.WriteIntent, _ []byte) error {
	w.calls = append(w.calls, "discard")
	return w.discardErr
}
func (w *discardWriter) ObserveDiscard(context.Context, storage.WriteIntent) (storage.DiscardEvidence, error) {
	w.calls = append(w.calls, "observe")
	return w.evidence, nil
}
func (w *discardWriter) ProveDiscard(context.Context, storage.WriteIntent) (storage.DiscardEvidence, error) {
	w.calls = append(w.calls, "prove")
	if w.proveErr != nil {
		return storage.DiscardEvidence{}, w.proveErr
	}
	// A recorded proof is what turns the retained outcome into a releasable one.
	w.discardErr = nil
	return w.evidence, nil
}

func newDiscardFixture(t *testing.T) (*Service, *discardJournal, *discardWriter, Actor) {
	t.Helper()
	root := Root{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "content", SystemRoot: "system", CaseSensitive: true}
	intent, err := json.Marshal(storage.WriteIntent{ID: "op-discard", Target: "target.bin", Size: 4, Conditions: storage.Conditions{Absent: true}, ParentPath: "", MediaType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	j := &discardJournal{op: Operation{ID: "op-discard", Root: root, PrincipalID: "principal", Kind: "upload_commit", State: Cancelled, Phase: Settled, Intent: intent, Reservation: ReservationRequest{NewBytes: 4, PhysicalBytes: 4}}}
	writer := &discardWriter{
		binding:    storage.Config{OwnerID: root.OwnerID, ConnectionID: root.ConnectionID, Generation: root.Generation, ContentRoot: root.ContentRoot, SystemRoot: root.SystemRoot},
		names:      storage.NameRules{CaseSensitive: root.CaseSensitive},
		discardErr: storage.NewError("recovery_required", "target.bin", nil),
		evidence:   storage.DiscardEvidence{UploadAbsent: true},
	}
	s := &Service{journal: j, options: ServiceOptions{LeaseTTL: time.Hour}}
	return s, j, writer, Actor{ID: "principal", CanRead: true, CanWrite: true, ManageOwner: true}
}

func TestReconcileDiscardProbeWritesNothingAndApplyReleasesOnlyAfterProof(t *testing.T) {
	s, j, writer, actor := newDiscardFixture(t)
	evidence, err := s.ReconcileDiscard(context.Background(), j.op.Root, actor, j.op.ID, writer, false)
	if err != nil {
		t.Fatal(err)
	}
	if !evidence.UploadAbsent || len(writer.calls) != 1 || writer.calls[0] != "observe" {
		t.Fatalf("dry run must only observe the provider: %v", writer.calls)
	}
	if len(j.calls) != 1 || j.op.CleanedAt != nil {
		t.Fatalf("dry run wrote journal state: %v", j.calls)
	}
	writer.calls, j.calls = nil, nil
	if _, err = s.ReconcileDiscard(context.Background(), j.op.Root, actor, j.op.ID, writer, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"discard", "prove", "discard"}
	if len(writer.calls) != len(want) {
		t.Fatalf("unexpected writer calls: %v", writer.calls)
	}
	for i := range want {
		if writer.calls[i] != want[i] {
			t.Fatalf("unexpected writer call order: %v", writer.calls)
		}
	}
	if j.op.CleanedAt == nil {
		t.Fatal("operation was not cleaned")
	}
}

func TestReconcileDiscardSkipsProofWhenCleanupSucceeds(t *testing.T) {
	s, j, writer, actor := newDiscardFixture(t)
	writer.discardErr = nil
	if _, err := s.ReconcileDiscard(context.Background(), j.op.Root, actor, j.op.ID, writer, true); err != nil {
		t.Fatal(err)
	}
	if len(writer.calls) != 1 || writer.calls[0] != "discard" {
		t.Fatalf("a completed cleanup must not probe the provider: %v", writer.calls)
	}
	if j.op.CleanedAt == nil {
		t.Fatal("operation was not cleaned")
	}
}

func TestReconcileDiscardKeepsLiabilityWhenProviderStillHoldsAllocation(t *testing.T) {
	s, j, writer, actor := newDiscardFixture(t)
	writer.proveErr = E("recovery_required", nil)
	if _, err := s.ReconcileDiscard(context.Background(), j.op.Root, actor, j.op.ID, writer, true); !IsCode(err, "recovery_required") {
		t.Fatalf("expected retained liability, got %v", err)
	}
	if j.op.CleanedAt != nil {
		t.Fatal("unproven allocation was released")
	}
}

func TestReconcileDiscardRefusesNonTerminalOperation(t *testing.T) {
	s, j, writer, actor := newDiscardFixture(t)
	j.op.State = Running
	if _, err := s.ReconcileDiscard(context.Background(), j.op.Root, actor, j.op.ID, writer, true); !IsCode(err, "operation_in_progress") {
		t.Fatalf("expected operation_in_progress, got %v", err)
	}
	if len(writer.calls) != 0 || len(j.calls) != 1 {
		t.Fatalf("in-flight work was touched: %v %v", writer.calls, j.calls)
	}
}
