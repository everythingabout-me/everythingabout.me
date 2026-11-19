package api

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// uncertainMove begins one move whose single copy entry has an unknown native outcome —
// the state a lost provider reply leaves behind. The journal, not the test, owns every
// transition, so the operation is shaped exactly like the one a worker would recover.
func (f *fileWorkerFixture) uncertainMove(t *testing.T) (fileops.Root, fileops.Actor, fileops.Operation) {
	t.Helper()
	root, _ := f.owner(t)
	actor := workerActor(root)
	intent, err := json.Marshal(fileops.ActionRequest{Kind: "move", From: "source", To: "target", Root: root, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	begun, _, err := f.journal.Begin(f.ctx, fileops.BeginRequest{
		Root: root, Actor: actor, Kind: "move", Intent: intent,
		Guards: []fileops.Guard{{Path: "source", Subtree: true}, {Path: "target", Subtree: true}},
		Manifest: []fileops.ManifestEntry{{
			Number: 0, Action: "copy", Source: "source", Target: "target",
			SourceRevision:   []byte("native-etag:fixture"),
			TargetConditions: storage.Conditions{Absent: true},
			Size:             4,
		}},
		// A move reserves the possible duplicate outputs; it never discounts the source.
		Reservation: fileops.ReservationRequest{NewBytes: 4, PhysicalBytes: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.journal.Claim(f.ctx, root, actor, begun.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.journal.BeginEntry(f.ctx, claimed.Lease, 0); err != nil {
		t.Fatal(err)
	}
	// The native copy was issued and its reply was lost: the entry may have landed.
	if err = f.journal.SaveEntry(f.ctx, claimed.Lease, 0, "outcome_unknown", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = f.journal.Release(f.ctx, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	latest, err := f.journal.Get(f.ctx, root, actor, begun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != fileops.Unknown {
		t.Fatalf("operation with an uncertain entry is %q, want outcome_unknown", latest.State)
	}
	return root, actor, latest
}

// A move whose entries can never be resolved must end, and ending it has to release the
// path guards: while the operation is non-terminal every later mutation of that subtree
// answers 409, so an unbounded retry loop makes the user's directory permanently
// unusable. This exercises the wiring around the pure policy: the journal transition, the
// guard release and the reservation refund.
func TestStalledActionRetirementReleasesThePath(t *testing.T) {
	f := newFileWorkerFixture(t)
	root, actor, latest := f.uncertainMove(t)
	server := f.server()
	transient := storage.NewError("backend_error", "target", errors.New("HTTP 503 SlowDown"))
	retired, err := server.retireActionAfterAttempt(f.ctx, latest, actionRetirementAttempts, false, transient)
	if err != nil || !retired {
		t.Fatalf("retired=%v err=%v, want the operation ended", retired, err)
	}
	done, err := f.journal.Get(f.ctx, root, actor, latest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != fileops.Unresolved || done.Phase != fileops.Settled || done.FinishedAt == nil {
		t.Fatalf("retired operation state=%q phase=%q finished=%v", done.State, done.Phase, done.FinishedAt)
	}
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err = json.Unmarshal(done.Problem, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "outcome_unknown" || problem.Detail == "" {
		t.Fatalf("retired problem %+v", problem)
	}
	var guards int
	if err = f.db.Pool().QueryRow(f.ctx, `select count(*) from file_operation_guards where operation_id=$1`, latest.ID).Scan(&guards); err != nil {
		t.Fatal(err)
	}
	if guards != 0 {
		t.Fatalf("retirement kept %d path guards", guards)
	}
	f.checkNoLiability(t, root)

	// Retirement is repeatable: observing a terminal operation again changes nothing and
	// does not error.
	if again, err := server.retireActionAfterAttempt(f.ctx, done, actionRetirementAttempts, false, transient); err != nil || !again {
		t.Fatalf("second retirement=%v err=%v", again, err)
	}
}

// A cancel recorded while an entry's outcome is unknown ends the operation on the next
// failed attempt instead of waiting out the claim budget: the owner already asked to
// stop, and the same non-answer would only hold the guard longer.
func TestCancelledUncertainActionRetiresWithoutTheClaimBudget(t *testing.T) {
	f := newFileWorkerFixture(t)
	root, actor, latest := f.uncertainMove(t)
	if _, err := f.journal.RequestCancel(f.ctx, root, actor, latest.ID); err != nil {
		t.Fatal(err)
	}
	latest, err := f.journal.Get(f.ctx, root, actor, latest.ID)
	if err != nil {
		t.Fatal(err)
	}
	server := f.server()
	retired, err := server.retireActionAfterAttempt(f.ctx, latest, 1, false, fileops.E("outcome_unknown", nil))
	if err != nil || !retired {
		t.Fatalf("cancelled retirement=%v err=%v, want the operation ended", retired, err)
	}
	done, err := f.journal.Get(f.ctx, root, actor, latest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != fileops.Unresolved {
		t.Fatalf("cancelled uncertain operation state=%q, want unresolved", done.State)
	}
	f.checkNoLiability(t, root)
}
