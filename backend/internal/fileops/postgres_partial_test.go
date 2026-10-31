package fileops

import (
	"context"
	"encoding/json"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestPostgresPartialChargingAndQuotaSafety(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("proven partial charge survives unknown restart without double debit", func(t *testing.T) {
		baseline := provenBaseline(0, 0, 100, 100)
		p, r, a := seedOwner(t, pool, baseline)
		req := genericRequest(r, a, "tree", 10)
		req.Guards[0].Subtree = true
		req.Manifest = []ManifestEntry{{Number: 0, Action: "copy", Source: "tree/a", Target: "tree/b", SourceVersion: []byte("v1"), TargetConditions: storage.Conditions{Absent: true}, Size: 5}, {Number: 1, Action: "copy", Source: "tree/c", Target: "tree/d", SourceVersion: []byte("v2"), TargetConditions: storage.Conditions{Absent: true}, Size: 5}}
		op := mustClaim(t, p, mustBegin(t, p, req), a)
		if err := p.BeginEntry(ctx, op.Lease, 0); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveEntry(ctx, op.Lease, 0, "started", []byte("native allocation one"), nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveEntry(ctx, op.Lease, 0, "started", []byte("native allocation two"), nil, nil); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.SaveEntry(ctx, op.Lease, 0, "applied", []byte("checkpoint"), nil, nil), "invalid_request")
		if err := p.SaveEntry(ctx, op.Lease, 0, "applied", []byte("terminal receipt"), []byte("all native requests terminal"), json.RawMessage(`{"size":5}`)); err != nil {
			t.Fatal(err)
		}
		if err := p.BeginEntry(ctx, op.Lease, 1); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveEntry(ctx, op.Lease, 1, "outcome_unknown", []byte("uncertain receipt"), []byte("native request may arrive"), nil); err != nil {
			t.Fatal(err)
		}
		charge := EntryCharge{Number: 0, LogicalDelta: 5, PhysicalDelta: 5}
		accounted, err := p.AccountEntry(ctx, op.Lease, charge)
		if err != nil {
			t.Fatal(err)
		}
		if accounted.State != Unknown || accounted.ChargedLogical != 5 || accounted.ChargedPhysical != 5 || accounted.Terminal() {
			t.Fatal(accounted)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 5 || u.PhysicalBytes != 5 || u.LogicalReserved != 5 || u.PhysicalReserved != 5 {
			t.Fatal(u)
		}
		restarted := NewPostgres(pool)
		if _, err = restarted.AccountEntry(ctx, op.Lease, charge); err != nil {
			t.Fatal(err)
		}
		changed := charge
		changed.LogicalDelta = 4
		_, err = p.AccountEntry(ctx, op.Lease, changed)
		requireCode(t, err, "intent_conflict")
		_, err = p.AccountEntry(ctx, op.Lease, EntryCharge{Number: 1, LogicalDelta: 5})
		requireCode(t, err, "recovery_required")
		requireCode(t, p.SaveEntry(ctx, op.Lease, 1, "started", nil, nil, nil), "invalid_transition")
		_, _, err = p.Begin(ctx, genericRequest(r, a, "tree/new", 1))
		requireCode(t, err, "operation_in_progress")
		_, err = p.Settle(ctx, op.Lease, Settlement{State: Failed, LogicalDelta: 5, PhysicalDelta: 5}, nil)
		requireCode(t, err, "recovery_required")
		expireLease(t, pool, op.ID)
		requireCode(t, p.SaveCheckpoint(ctx, op.Lease, nil), "lease_lost")
		next := mustClaim(t, restarted, op, a)
		if _, err = restarted.AccountEntry(ctx, next.Lease, charge); err != nil {
			t.Fatal(err)
		}
		if err = restarted.SaveEntry(ctx, next.Lease, 1, "not_applied", []byte("terminal failure"), []byte("all requests fenced, no effect"), nil); err != nil {
			t.Fatal(err)
		}
		if err = restarted.SaveApplied(ctx, next.Lease, storage.MutationResult{Evidence: []byte("known complete partial manifest")}); err != nil {
			t.Fatal(err)
		}
		_, err = restarted.Settle(ctx, next.Lease, Settlement{State: Failed, LogicalDelta: 4, PhysicalDelta: 5}, nil)
		requireCode(t, err, "reservation_exceeded")
		settlement := Settlement{State: Failed, LogicalDelta: 5, PhysicalDelta: 5, Result: json.RawMessage(`{"applied":[0],"notApplied":[1],"remaining":[]}`)}
		if _, err = restarted.Settle(ctx, next.Lease, settlement, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = restarted.Settle(ctx, next.Lease, settlement, nil); err != nil {
			t.Fatal(err)
		}
		if _, err = restarted.AccountEntry(ctx, next.Lease, charge); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 5 || u.PhysicalBytes != 5 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("double charge on final settlement", u)
		}
		if err = p.Initialize(ctx, r, baseline); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 5 {
			t.Fatal("baseline reset charged usage", u)
		}
	})
	t.Run("over quota baseline allows no growth but rejects new liability", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(120, 120, 100, 0))
		same := mustClaim(t, p, mustBegin(t, p, replacementRequest(r, a, "same", 10, 10, []byte("same-v1"))), a)
		shrink := mustClaim(t, p, mustBegin(t, p, replacementRequest(r, a, "shrink", 5, 10, []byte("shrink-v1"))), a)
		req := genericRequest(r, a, "remove", 0)
		req.Kind = "delete"
		mustBegin(t, p, req)
		_, _, err := p.Begin(ctx, genericRequest(r, a, "new", 1))
		requireCode(t, err, "quota_exceeded")
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 120 || u.LogicalReserved != 0 || u.PhysicalReserved != 15 {
			t.Fatal(u)
		}
		if err = p.FinishWithoutEffect(ctx, same.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		if err = p.FinishWithoutEffect(ctx, shrink.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 120 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
		bad := genericRequest(r, a, "untyped", 10)
		bad.Reservation.OldBytes = 10
		bad.Reservation.CreditPath = "untyped"
		bad.Reservation.CreditVersion = []byte("v1")
		_, _, err = p.Begin(ctx, bad)
		requireCode(t, err, "invalid_request")
	})
	t.Run("exact ancestor moves and root operations conservatively conflict", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		child := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "parent/child", 0)), a)
		move := genericRequest(r, a, "parent", 0)
		move.Kind = "move"
		_, _, err := p.Begin(ctx, move)
		requireCode(t, err, "operation_in_progress")
		root := genericRequest(r, a, "", 0)
		_, _, err = p.Begin(ctx, root)
		requireCode(t, err, "operation_in_progress")
		if err = p.FinishWithoutEffect(ctx, child.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		mover := mustClaim(t, p, mustBegin(t, p, move), a)
		_, _, err = p.Begin(ctx, genericRequest(r, a, "parent/new", 0))
		requireCode(t, err, "operation_in_progress")
		if err = p.FinishWithoutEffect(ctx, mover.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		mustBegin(t, p, root)
		_, _, err = p.Begin(ctx, genericRequest(r, a, "elsewhere", 0))
		requireCode(t, err, "operation_in_progress")
	})
}
