package fileops

import (
	"context"
	"testing"
)

// A settlement that outlives the client must still be allowed to finish. The durable
// context is therefore detached from the caller's cancellation, and it must not carry
// a wall clock: the amount of work it settles scales with the operation, so a budget
// picked in advance only ever fires on the operation large enough to matter.
func TestDurableContextSurvivesCancellationWithoutAWallClock(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	persist, done := durableContext(ctx)
	defer done()
	stop()
	if err := persist.Err(); err != nil {
		t.Fatalf("durable context followed the client: %v", err)
	}
	if deadline, ok := persist.Deadline(); ok {
		t.Fatalf("durable context carries a wall clock (%v); a large settlement can never meet it", deadline)
	}
	select {
	case <-persist.Done():
		t.Fatal("durable context completed without being cancelled")
	default:
	}
}
