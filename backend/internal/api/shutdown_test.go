package api

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitBackgroundDrainsAdmittedOperations pins the ordering the server's
// shutdown relies on: WaitBackground may not return while an admitted file
// operation is still running, because that operation can still be writing (the
// settlement of an already-published effect deliberately ignores cancellation).
// It must also close admission before it waits, so the WaitGroup cannot grow
// while it is being waited on.
func TestWaitBackgroundDrainsAdmittedOperations(t *testing.T) {
	s := &Server{}
	worker := s.workers()
	worker.operations.Add(1)
	release := make(chan struct{})
	go func() {
		defer worker.operations.Done()
		<-release
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drained := make(chan error, 1)
	go func() { drained <- s.WaitBackground(ctx) }()

	select {
	case err := <-drained:
		t.Fatalf("returned while an admitted operation was still running: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("did not return after the admitted operation finished")
	}

	worker.mu.Lock()
	draining := worker.draining
	worker.mu.Unlock()
	if !draining {
		t.Fatal("admission stayed open after the drain started")
	}
}

// TestWaitBackgroundHonoursTheDrainBudget proves the wait is bounded: a stuck
// operation reports the caller's deadline instead of turning shutdown into a
// hang, and the caller can then say which work was cut.
func TestWaitBackgroundHonoursTheDrainBudget(t *testing.T) {
	s := &Server{}
	worker := s.workers()
	worker.operations.Add(1)
	release := make(chan struct{})
	go func() {
		defer worker.operations.Done()
		<-release
	}()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.WaitBackground(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain error = %v, want the caller's deadline", err)
	}
}
