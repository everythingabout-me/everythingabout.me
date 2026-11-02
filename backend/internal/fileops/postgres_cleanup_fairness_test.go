package fileops

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestPostgresCleanupCandidatesAdvancePastBusyOwners(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, root, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 10000))
	busy := make(map[string]bool)
	for i := 0; i < 16; i++ {
		u := mustUpload(t, p, root, actor, storage.Path(fmt.Sprintf("busy-%02d", i)), 1, "")
		if _, err := p.AbortUpload(ctx, root, actor, u.ID); err != nil {
			t.Fatal(err)
		}
		busy[u.ID] = true
	}
	_, otherRoot, otherActor := seedOwner(t, pool, provenBaseline(0, 0, 100, 10000))
	later := mustUpload(t, p, otherRoot, otherActor, "later-owner", 1, "")
	if _, err := p.AbortUpload(ctx, otherRoot, otherActor, later.ID); err != nil {
		t.Fatal(err)
	}
	first, err := p.CleanupCandidates(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 16 {
		t.Fatalf("first bounded cleanup batch = %d", len(first))
	}
	for _, op := range first {
		if !busy[op.ID] {
			t.Fatalf("fixture did not place busy rows first: %s", op.ID)
		}
	}
	// Every first-batch cleanup is modeled as ErrBusy: no MarkCleaned call is made.
	// Discovery itself must advance fairly, without forgetting their liability.
	second, err := p.CleanupCandidates(ctx, 16)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, op := range second {
		if op.ID == later.ID {
			found = true
		}
		if !op.Terminal() || op.CleanedAt != nil {
			t.Fatalf("scheduling changed cleanup proof: %+v", op)
		}
	}
	if !found {
		t.Fatal("oldest busy owner monopolized cleanup; later owner never scheduled")
	}
	if usage := mustUsage(t, p, root, actor); usage.LogicalReserved != 0 || usage.PhysicalReserved != 16*18 {
		t.Fatalf("busy discovery changed reservation: %+v", usage)
	}
	if usage := mustUsage(t, p, otherRoot, otherActor); usage.LogicalReserved != 0 || usage.PhysicalReserved != 18 {
		t.Fatalf("later discovery changed reservation: %+v", usage)
	}
}

func TestPostgresCleanupCandidatesConcurrentScheduling(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, root, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 10000))
	for i := 0; i < 33; i++ {
		u := mustUpload(t, p, root, actor, storage.Path(fmt.Sprintf("terminal-%02d", i)), 1, "")
		if _, err := p.AbortUpload(ctx, root, actor, u.ID); err != nil {
			t.Fatal(err)
		}
	}
	type result struct {
		ops []Operation
		err error
	}
	out := make(chan result, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			ops, err := p.CleanupCandidates(ctx, 16)
			out <- result{ops, err}
		}()
	}
	close(start)
	workers.Wait()
	close(out)
	seen := make(map[string]bool)
	for batch := range out {
		if batch.err != nil {
			t.Fatal(batch.err)
		}
		if len(batch.ops) != 16 {
			t.Fatalf("concurrent bounded batch = %d", len(batch.ops))
		}
		for _, op := range batch.ops {
			if seen[op.ID] {
				t.Fatalf("concurrent schedulers repeated candidate instead of untried work: %s", op.ID)
			}
			seen[op.ID] = true
		}
	}
	if len(seen) != 32 {
		t.Fatalf("distinct cleanup candidates = %d", len(seen))
	}
	if usage := mustUsage(t, p, root, actor); usage.PhysicalReserved != 33*18 {
		t.Fatalf("scheduling released uncleaned physical bytes: %+v", usage)
	}
}
