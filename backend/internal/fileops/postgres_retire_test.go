package fileops

import (
	"context"
	"encoding/json"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// RetireStalled is the only transition that may end an operation whose native effect is
// unknown, so it must release everything a terminal operation releases, mark the upload
// session replaceable, refuse a live worker, and be repeatable.
func TestPostgresRetireStalled(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))

	// A replacement session carries a credit claim, so retirement must release it too.
	req, opt := uploadRequest(r, a, "folder/retire", 8, "retire")
	req.Reservation.OldBytes = 8
	req.Reservation.CreditPath = "folder/retire"
	req.Reservation.CreditVersion = []byte("old-revision")
	replacement := opt.ClientIntent
	replacement.Conditions = storage.Conditions{ExpectedRevision: []byte("old-revision")}
	req.Intent, _ = json.Marshal(replacement)
	u, replay, err := p.CreateUpload(ctx, req, opt)
	if err != nil || replay {
		t.Fatalf("create replay=%v: %v", replay, err)
	}
	mustReceipt(t, p, r, a, u, 1, 4)
	mustReceipt(t, p, r, a, u, 2, 4)
	frozen, err := p.FreezeUpload(ctx, r, a, u.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := mustClaim(t, p, frozen.Operation, a)

	before := mustUsage(t, p, r, a)
	// A same-size replacement reserves no logical growth, but it still holds the
	// provider staging bytes; retirement must release both counters.
	if before.LogicalReserved != 0 || before.PhysicalReserved != 32 {
		t.Fatalf("reservation before retirement %+v", before)
	}
	problem := json.RawMessage(`{"code":"outcome_unknown","detail":"stalled fixture"}`)

	// A live lease belongs to a worker and must never be force-failed from outside.
	_, err = p.RetireStalled(ctx, r, a, claimed.ID, Unresolved, problem)
	requireCode(t, err, "operation_in_progress")

	expireLease(t, pool, claimed.ID)
	retired, err := p.RetireStalled(ctx, r, a, claimed.ID, Unresolved, problem)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != Unresolved || retired.Phase != Settled || retired.FinishedAt == nil {
		t.Fatalf("retired document %+v", retired)
	}
	if !equalJSON(retired.Problem, problem) {
		t.Fatalf("problem = %s, want %s", retired.Problem, problem)
	}
	if usage := mustUsage(t, p, r, a); usage.LogicalReserved != 0 || usage.PhysicalReserved != 0 {
		t.Fatalf("retirement kept a reservation: %+v", usage)
	}
	var logical, physical int64
	var settled bool
	if err = pool.QueryRow(ctx, `select logical_reserved,physical_reserved,settled from file_reservations where operation_id=$1 and owner_id=$2`, claimed.ID, r.OwnerID).Scan(&logical, &physical, &settled); err != nil {
		t.Fatal(err)
	}
	if logical != 0 || physical != 0 || !settled {
		t.Fatalf("reservation row %d/%d settled=%v", logical, physical, settled)
	}
	var guards, claims int
	if err = pool.QueryRow(ctx, `select (select count(*) from file_operation_guards where operation_id=$1 and owner_id=$2),(select count(*) from file_credit_claims where operation_id=$1 and owner_id=$2)`, claimed.ID, r.OwnerID).Scan(&guards, &claims); err != nil {
		t.Fatal(err)
	}
	if guards != 0 || claims != 0 {
		t.Fatalf("retirement left %d guards and %d credit claims", guards, claims)
	}
	status, err := p.GetUpload(ctx, r, a, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != UploadAborted || status.Operation.State != Unresolved {
		t.Fatalf("session after retirement %+v", status)
	}

	// Repeating the call is idempotent, even with a different requested state: the
	// journal's terminal answer is returned, never rewritten.
	again, err := p.RetireStalled(ctx, r, a, claimed.ID, Failed, nil)
	if err != nil || again.State != Unresolved || again.ID != claimed.ID {
		t.Fatalf("repeat retirement %+v %v", again, err)
	}
	// A retired operation is not pending work, and it cannot be settled as a success.
	queue, err := p.OperationQueue(ctx)
	if err != nil || queue.Total() != 0 {
		t.Fatalf("retired operation still queued: %+v %v", queue, err)
	}
	if _, err = p.RetireStalled(ctx, r, a, claimed.ID, Succeeded, nil); !IsCode(err, "invalid_request") {
		t.Fatalf("settlement state accepted by retirement: %v", err)
	}

	// Failed is the other permitted terminal answer for an attempt that provably
	// published nothing; it follows the same release rule.
	u = mustUpload(t, p, r, a, "folder/fail", 4, "fail")
	mustReceipt(t, p, r, a, u, 1, 4)
	frozen, err = p.FreezeUpload(ctx, r, a, u.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed = mustClaim(t, p, frozen.Operation, a)
	expireLease(t, pool, claimed.ID)
	failed, err := p.RetireStalled(ctx, r, a, claimed.ID, Failed, uploadProblem("quota_exceeded", "admission cannot succeed"))
	if err != nil || failed.State != Failed || failed.FinishedAt == nil {
		t.Fatalf("failed retirement %+v %v", failed, err)
	}
	status, err = p.GetUpload(ctx, r, a, u.ID)
	if err != nil || status.State != UploadAborted {
		t.Fatalf("failed-retirement session %+v %v", status, err)
	}
	if usage := mustUsage(t, p, r, a); usage.LogicalReserved != 0 || usage.PhysicalReserved != 0 {
		t.Fatalf("failed retirement kept a reservation: %+v", usage)
	}
}

// The state must be terminal for every reader that answers "may this operation still
// land an effect?"; Terminal is the one predicate they share.
func TestUnresolvedIsTerminal(t *testing.T) {
	op := Operation{State: Unresolved}
	if !op.Terminal() {
		t.Fatal("unresolved is not terminal")
	}
	if terminalState(Unresolved) {
		t.Fatal("unresolved must never be admitted as a settlement state")
	}
}
