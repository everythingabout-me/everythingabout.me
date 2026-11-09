package fileops

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPhysicalBudgetPolicy(t *testing.T) {
	for _, tc := range []struct {
		retain                   bool
		newBytes, oldBytes, want int64
	}{
		{false, 10, 0, 10}, {false, 10, 7, 3}, {false, 0, 7, -7}, {false, 4, 7, -3},
		{true, 10, 0, 10}, {true, 10, 7, 10}, {true, 0, 7, 0}, {true, 4, 7, 4},
	} {
		if got := PhysicalDelta(Root{RetainsVersions: tc.retain}, tc.newBytes, tc.oldBytes); got != tc.want {
			t.Fatalf("retain=%t new=%d old=%d: %d want %d", tc.retain, tc.newBytes, tc.oldBytes, got, tc.want)
		}
	}
	for _, retain := range []bool{false, true} {
		root := Root{RetainsVersions: retain}
		got, err := PhysicalReservation(root, 7)
		want := int64(7)
		if retain {
			want = 14
		}
		if err != nil || got != want {
			t.Fatalf("reservation %d %v", got, err)
		}
	}
	if _, err := PhysicalReservation(Root{RetainsVersions: true}, math.MaxInt64); !IsCode(err, "reservation_exceeded") {
		t.Fatalf("overflow admitted: %v", err)
	}
	if _, err := PhysicalReservation(Root{}, -1); !IsCode(err, "invalid_request") {
		t.Fatalf("negative admitted: %v", err)
	}
}

func TestRetainingUploadAdmissionRequiresReplaySpoolBudget(t *testing.T) {
	root := testRoot()
	root.RetainsVersions = true
	actor := testActor(root)
	intent, _ := json.Marshal(storage.WriteIntent{Target: "file", Size: 5, Conditions: storage.Conditions{Absent: true}})
	req := BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", Intent: intent, Guards: []Guard{{Path: "file"}}, Reservation: ReservationRequest{NewBytes: 5, PhysicalBytes: 5}}
	if _, _, _, err := validateBegin(req); !IsCode(err, "invalid_request") {
		t.Fatalf("single allocation budget admitted for spool plus retained native version: %v", err)
	}
	for _, physical := range []int64{10, 12} {
		req.Reservation.PhysicalBytes = physical
		if _, _, _, err := validateBegin(req); err != nil {
			t.Fatalf("sufficient budget rejected: %v", err)
		}
	}
	root.RetainsVersions = false
	req.Root = root
	req.Reservation.PhysicalBytes = 5
	if _, _, _, err := validateBegin(req); err != nil {
		t.Fatalf("local one-inode staging budget changed: %v", err)
	}
}

func seedRetainingOwner(t *testing.T, pool *pgxpool.Pool, base Baseline) (*Postgres, Root, Actor) {
	t.Helper()
	root := testRoot()
	root.OwnerID = newID()
	root.RetainsVersions = true
	actor := testActor(root)
	if _, err := pool.Exec(context.Background(), `insert into users(id,email,password_hash) values($1,$2,'test-only')`, root.OwnerID, root.OwnerID+"@example.test"); err != nil {
		t.Fatal(err)
	}
	journal := NewPostgres(pool)
	if err := journal.Initialize(context.Background(), root, base); err != nil {
		t.Fatal(err)
	}
	return journal, root, actor
}

func TestRetainingActionsPostgresPhysicalBudget(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("copy move delete retain historical bytes", func(t *testing.T) {
		p, root, actor := seedRetainingOwner(t, pool, provenBaseline(3, 40, 100, 1000))
		native := newActionNative(root)
		native.put("source", storage.File, "abc")
		actions := actionCoordinator(t, p, nil)
		copyOp := prepareAction(t, actions, ActionRequest{Root: root, Actor: actor, Kind: "copy", From: "source", To: "copy", IfRevision: "captured", IdempotencyKey: "copy"}, native)
		if u := mustUsage(t, p, root, actor); u.LogicalReserved != 3 || u.PhysicalReserved != 6 {
			t.Fatal("copy spool+native reservation", u)
		}
		copyResult := runAction(t, actions, copyOp, actor, native)
		if copyResult.State != Succeeded {
			t.Fatal(copyResult)
		}
		if u := mustUsage(t, p, root, actor); u.LogicalBytes != 6 || u.PhysicalBytes != 43 || u.PhysicalReserved != 0 {
			t.Fatal("copy charge", u)
		}
		moveOp := prepareAction(t, actions, ActionRequest{Root: root, Actor: actor, Kind: "move", From: "source", To: "moved", IfRevision: "captured", IdempotencyKey: "move"}, native)
		if u := mustUsage(t, p, root, actor); u.PhysicalReserved != 6 {
			t.Fatal("move spool reservation", u)
		}
		moved := runAction(t, actions, moveOp, actor, native)
		if moved.State != Succeeded {
			t.Fatal(moved)
		}
		if u := mustUsage(t, p, root, actor); u.LogicalBytes != 6 || u.PhysicalBytes != 46 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("move freed old versions", u)
		}
		deleteReq := ActionRequest{Root: root, Actor: actor, Kind: "delete", From: "copy", IdempotencyKey: "delete"}
		deleted := runAction(t, actions, prepareAction(t, actions, deleteReq, native), actor, native)
		if deleted.State != Succeeded {
			t.Fatal(deleted)
		}
		if u := mustUsage(t, p, root, actor); u.LogicalBytes != 3 || u.PhysicalBytes != 46 {
			t.Fatal("delete credited retained history", u)
		}
		again, replayed, err := actions.Prepare(ctx, deleteReq, native)
		if err != nil || !replayed || again.ID != deleted.ID {
			t.Fatalf("delete replay %t %v", replayed, err)
		}
		if u := mustUsage(t, p, root, actor); u.PhysicalBytes != 46 {
			t.Fatal("replay changed usage", u)
		}
		wrong := root
		wrong.RetainsVersions = false
		if _, err = p.Usage(ctx, wrong, actor); !IsCode(err, "root_conflict") {
			t.Fatalf("retention root mutation accepted %v", err)
		}
	})
	t.Run("unknown move charges known copies once and preserves liabilities", func(t *testing.T) {
		p, root, actor := seedRetainingOwner(t, pool, provenBaseline(7, 50, 100, 1000))
		native := newActionNative(root)
		native.put("source", storage.Folder, "")
		native.put("source/a", storage.File, "abc")
		native.put("source/b", storage.File, "1234")
		native.unknownAt = 3
		actions := actionCoordinator(t, p, nil)
		op := prepareAction(t, actions, ActionRequest{Root: root, Actor: actor, Kind: "move", From: "source", To: "target", IfRevision: "captured"}, native)
		if u := mustUsage(t, p, root, actor); u.PhysicalReserved != 14 {
			t.Fatal(u)
		}
		partial, err := actions.Run(ctx, root, actor, op.ID, native)
		if !IsCode(err, "outcome_unknown") || partial.State != Unknown || partial.ChargedPhysical != 3 {
			t.Fatalf("unknown %+v %v", partial, err)
		}
		if u := mustUsage(t, p, root, actor); u.LogicalBytes != 10 || u.PhysicalBytes != 53 || u.LogicalReserved != 4 || u.PhysicalReserved != 11 {
			t.Fatal("partial exposure", u)
		}
		again, err := actions.Run(ctx, root, actor, op.ID, native)
		if !IsCode(err, "outcome_unknown") || again.ChargedPhysical != 3 {
			t.Fatalf("partial replay %+v %v", again, err)
		}
		if u := mustUsage(t, p, root, actor); u.PhysicalBytes != 53 || u.PhysicalReserved != 11 {
			t.Fatal("partial double charge", u)
		}
		intent := native.calls[len(native.calls)-1]
		native.put(intent.Target, storage.File, "1234")
		entry := native.entries[intent.Target]
		result := storage.MutationResult{Entry: &entry, Evidence: []byte("resolved immutable native version")}
		native.receipts[intent.ID] = storage.Inspection{State: storage.Applied, Result: &result, Evidence: result.Evidence}
		completed := runAction(t, actions, op, actor, native)
		if completed.State != Succeeded || completed.ChargedPhysical != 7 {
			t.Fatal(completed)
		}
		if u := mustUsage(t, p, root, actor); u.LogicalBytes != 7 || u.PhysicalBytes != 57 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal("final cumulative charge", u)
		}
		if _, ok := native.entries["source"]; ok {
			t.Fatal("successful move left source current key")
		}
	})
	t.Run("physical admission includes local copy spool", func(t *testing.T) {
		p, root, actor := seedRetainingOwner(t, pool, provenBaseline(3, 40, 100, 45))
		native := newActionNative(root)
		native.put("source", storage.File, "abc")
		actions := actionCoordinator(t, p, nil)
		_, _, err := actions.Prepare(ctx, ActionRequest{Root: root, Actor: actor, Kind: "copy", From: "source", To: "copy", IfRevision: "captured"}, native)
		if !IsCode(err, "staging_full") {
			t.Fatalf("copy admitted without complete spool exposure %v", err)
		}
		if u := mustUsage(t, p, root, actor); u.PhysicalBytes != 40 || u.PhysicalReserved != 0 || u.LogicalReserved != 0 {
			t.Fatal("failed capture leaked reservation", u)
		}
	})
}

func TestRetainingCreatePostgresSettlementAndCleanup(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, root, actor := seedRetainingOwner(t, pool, provenBaseline(0, 19, 100, 100))
	intent := storage.WriteIntent{Target: "new", Size: 4, Conditions: storage.Conditions{Absent: true}}
	encoded, _ := json.Marshal(intent)
	op := mustBegin(t, p, BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", Intent: encoded, Guards: []Guard{{Path: "new"}}, Reservation: ReservationRequest{NewBytes: 4, PhysicalBytes: 8}})
	if u := mustUsage(t, p, root, actor); u.LogicalReserved != 4 || u.PhysicalReserved != 8 {
		t.Fatal(u)
	}
	op = mustClaim(t, p, op, actor)
	intent.ID = op.ID
	if err := p.SaveStage(ctx, op.Lease, storage.Staged{Intent: intent, Handle: []byte("durable spool"), Size: 4, Digest: &storage.Digest{Algorithm: "sha-256", Value: []byte("verified")}}); err != nil {
		t.Fatal(err)
	}
	if err := p.StartEffect(ctx, op.Lease); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveApplied(ctx, op.Lease, storage.MutationResult{Entry: &storage.Entry{Path: "new", Kind: storage.File, Size: 4}, Evidence: []byte("created immutable version")}); err != nil {
		t.Fatal(err)
	}
	settlement := Settlement{State: Succeeded, LogicalDelta: 4, PhysicalDelta: 4, KeepPhysical: 4, Result: json.RawMessage(`{"path":"/new"}`)}
	settled, err := p.Settle(ctx, op.Lease, settlement, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Settle(ctx, op.Lease, settlement, nil); err != nil {
		t.Fatal(err)
	}
	if u := mustUsage(t, p, root, actor); u.LogicalBytes != 4 || u.PhysicalBytes != 23 || u.PhysicalReserved != 4 {
		t.Fatal("create/replay physical usage", u)
	}
	if err = p.MarkCleaned(ctx, root, actor, settled.ID); err != nil {
		t.Fatal(err)
	}
	if u := mustUsage(t, p, root, actor); u.PhysicalBytes != 23 || u.PhysicalReserved != 0 {
		t.Fatal("spool cleanup changed retained usage", u)
	}
}

func TestRetainingUploadPostgresSettlementAndCleanup(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, root, actor := seedRetainingOwner(t, pool, provenBaseline(8, 30, 100, 100))
	intent := storage.WriteIntent{Target: "file", Size: 5, Conditions: storage.Conditions{ExpectedRevision: []byte("captured-old-version")}}
	encoded, _ := json.Marshal(intent)
	reserved, err := PhysicalReservation(root, intent.Size)
	if err != nil {
		t.Fatal(err)
	}
	op := mustBegin(t, p, BeginRequest{Root: root, Actor: actor, Kind: "upload_commit", Intent: encoded, Guards: []Guard{{Path: "file"}}, Reservation: ReservationRequest{NewBytes: 5, OldBytes: 8, CreditPath: "file", CreditVersion: intent.Conditions.ExpectedRevision, PhysicalBytes: reserved}})
	op = mustClaim(t, p, op, actor)
	intent.ID = op.ID
	stage := storage.Staged{Intent: intent, Handle: []byte("durable private spool"), Size: 5, Digest: &storage.Digest{Algorithm: "sha-256", Value: []byte("verified")}}
	if err = p.SaveStage(ctx, op.Lease, stage); err != nil {
		t.Fatal(err)
	}
	if err = p.StartEffect(ctx, op.Lease); err != nil {
		t.Fatal(err)
	}
	result := storage.MutationResult{Entry: &storage.Entry{Path: "file", Kind: storage.File, Size: 5}, Evidence: []byte("native new retained version")}
	if err = p.SaveApplied(ctx, op.Lease, result); err != nil {
		t.Fatal(err)
	}
	_, err = p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: -3, PhysicalDelta: -3, Result: json.RawMessage(`{"path":"/file"}`)}, nil)
	requireCode(t, err, "reservation_exceeded")
	settled, err := p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: -3, PhysicalDelta: PhysicalDelta(root, 5, 8), KeepPhysical: 5, Result: json.RawMessage(`{"path":"/file"}`)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := mustUsage(t, p, root, actor); u.LogicalBytes != 5 || u.PhysicalBytes != 35 || u.PhysicalReserved != 5 {
		t.Fatal("replacement freed old history/spool prematurely", u)
	}
	if err = p.MarkCleaned(ctx, root, actor, settled.ID); err != nil {
		t.Fatal(err)
	}
	if u := mustUsage(t, p, root, actor); u.PhysicalBytes != 35 || u.PhysicalReserved != 0 {
		t.Fatal("cleanup deleted charged native history", u)
	}
}
