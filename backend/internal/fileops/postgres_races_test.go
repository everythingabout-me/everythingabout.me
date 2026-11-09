package fileops

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func TestPostgresAdmissionRacesAndHandoff(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("same key concurrent creates one intent", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		req := genericRequest(r, a, "same", 5)
		req.IdempotencyKey = "same-key"
		type result struct {
			op     Operation
			replay bool
			err    error
		}
		results := make(chan result, 12)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				op, replay, err := p.Begin(ctx, req)
				results <- result{op, replay, err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		fresh := 0
		id := ""
		for v := range results {
			if v.err != nil {
				t.Fatal(v.err)
			}
			if !v.replay {
				fresh++
			}
			if id != "" && id != v.op.ID {
				t.Fatal("multiple operation identities")
			}
			id = v.op.ID
		}
		if fresh != 1 {
			t.Fatalf("fresh intents: %d", fresh)
		}
		if u := mustUsage(t, p, r, a); u.LogicalReserved != 5 || u.PhysicalReserved != 5 {
			t.Fatal(u)
		}
	})
	t.Run("target conflict concurrently and owner isolation", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		results := make(chan error, 12)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, _, err := p.Begin(ctx, genericRequest(r, a, "same", 1))
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		accepted := 0
		for err := range results {
			if err == nil {
				accepted++
			} else {
				requireCode(t, err, "operation_in_progress")
			}
		}
		if accepted != 1 {
			t.Fatalf("accepted target contenders: %d", accepted)
		}
		p2, r2, a2 := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		mustBegin(t, p2, genericRequest(r2, a2, "same", 1))
	})
	t.Run("release fences successor and preserves unknown guard", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		op := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "file", 4)), a)
		if err := p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if err := p.Release(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.SaveCheckpoint(ctx, op.Lease, nil), "lease_lost")
		next := mustClaim(t, p, op, a)
		if next.Lease.Epoch != op.Lease.Epoch+1 || next.State != Unknown {
			t.Fatal(next)
		}
		requireCode(t, p.Release(ctx, op.Lease), "lease_lost")
		requireCode(t, p.StartEffect(ctx, next.Lease), "recovery_required")
		_, _, err := p.Begin(ctx, genericRequest(r, a, "file", 0))
		requireCode(t, err, "operation_in_progress")
		if err = p.SaveApplied(ctx, next.Lease, storage.MutationResult{Evidence: []byte("terminal proof")}); err != nil {
			t.Fatal(err)
		}
		settlement := Settlement{State: Succeeded, LogicalDelta: 4, PhysicalDelta: 4, Result: json.RawMessage(`{"zz":2,"a":{"longer":1,"b":2}}`)}
		result, err := p.Settle(ctx, next.Lease, settlement, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Release(ctx, next.Lease); err != nil {
			t.Fatal(err)
		}
		result2, err := p.Settle(ctx, next.Lease, settlement, nil)
		if err != nil || !equalJSON(result.Result, result2.Result) {
			t.Fatalf("JSONB replay changed outcome %v", err)
		}
	})
	t.Run("manifest mutation and source credit rejection", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(10, 10, 100, 100))
		req := genericRequest(r, a, "tree", 12)
		req.Guards[0].Subtree = true
		req.IdempotencyKey = "manifest"
		req.Manifest = []ManifestEntry{{Number: 0, Action: "copy", Source: "tree/a", Target: "tree/b", Size: 12, SourceRevision: []byte("v1"), TargetConditions: storage.Conditions{Absent: true}}}
		op := mustBegin(t, p, req)
		changed := req
		changed.Manifest = append([]ManifestEntry(nil), req.Manifest...)
		changed.Manifest[0].SourceRevision = []byte("v2")
		_, _, err := p.Begin(ctx, changed)
		requireCode(t, err, "idempotency_conflict")
		changed = req
		changed.Manifest = append([]ManifestEntry(nil), req.Manifest...)
		changed.Manifest[0].TargetConditions = storage.Conditions{ExpectedRevision: []byte("target-v1")}
		_, _, err = p.Begin(ctx, changed)
		requireCode(t, err, "idempotency_conflict")
		changed = req
		changed.IdempotencyKey = "different"
		changed.Reservation.OldBytes = 10
		changed.Reservation.CreditPath = "tree/b"
		changed.Reservation.CreditVersion = []byte("target-v1")
		_, _, err = p.Begin(ctx, changed)
		requireCode(t, err, "invalid_request")
		op = mustClaim(t, p, op, a)
		if err = p.BeginEntry(ctx, op.Lease, 0); err != nil {
			t.Fatal(err)
		}
		if err = p.SaveEntry(ctx, op.Lease, 0, "not_applied", []byte("terminal no effect"), []byte("verified native quiescence"), nil); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.SaveEntry(ctx, op.Lease, 0, "applied", nil, nil, nil), "intent_conflict")
		if err = p.ResolveNotApplied(ctx, op.Lease, Failed, []byte("all entries terminal, discarded"), nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cancel before effect blocks publication", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		op := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "file", 1)), a)
		cancelled, err := p.RequestCancel(ctx, r, a, op.ID)
		if err != nil || !cancelled.CancelRequested || cancelled.Terminal() {
			t.Fatal(cancelled, err)
		}
		requireCode(t, p.StartEffect(ctx, op.Lease), "invalid_transition")
		if err = p.FinishWithoutEffect(ctx, op.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		if err = p.Release(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if _, err = p.Renew(ctx, op.Lease, time.Minute); !IsCode(err, "invalid_transition") {
			t.Fatal(err)
		}
	})
}
