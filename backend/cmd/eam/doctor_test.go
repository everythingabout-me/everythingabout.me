package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
)

func TestOperationQueueLinesReportTheQueue(t *testing.T) {
	owner := "11111111-1111-1111-1111-111111111111"
	queue := fileops.OperationQueue{
		Queued:          128,
		Running:         1,
		OutcomeUnknown:  1,
		CancelRequested: 2,
		Oldest: &fileops.OperationQueueOldest{
			State:     fileops.Queued,
			OwnerID:   owner,
			CreatedAt: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
			Age:       72*time.Minute + 3*time.Second + 400*time.Millisecond,
		},
	}
	want := []string{
		"  nonterminal       130 (queued 128, running 1, outcome_unknown 1)",
		"  oldest            1h12m3s  queued  owner " + owner + "  created 2026-11-16T10:00:00Z",
		"  cancel requested  2",
	}
	if got := operationQueueLines(queue); !slices.Equal(got, want) {
		t.Fatalf("queue section\n%q\nwant\n%q", got, want)
	}

	empty := []string{
		"  nonterminal       0 (queued 0, running 0, outcome_unknown 0)",
		"  oldest            none",
		"  cancel requested  0",
	}
	if got := operationQueueLines(fileops.OperationQueue{}); !slices.Equal(got, empty) {
		t.Fatalf("empty queue section\n%q\nwant\n%q", got, empty)
	}
}

func TestOperationQueueStallIsAnOperatorPolicy(t *testing.T) {
	queue := fileops.OperationQueue{
		Queued: 3,
		Oldest: &fileops.OperationQueueOldest{State: fileops.Queued, Age: 90 * time.Minute},
	}
	if err := operationQueueStalled(queue, 0); err != nil {
		t.Fatalf("zero threshold must report only, got %v", err)
	}
	if err := operationQueueStalled(queue, 90*time.Minute); err != nil {
		t.Fatalf("age equal to the threshold is not over it, got %v", err)
	}
	err := operationQueueStalled(queue, time.Hour)
	if err == nil || err.Error() != "3 nonterminal, oldest 1h30m0s" {
		t.Fatalf("stalled error %v, want it to name the count and age", err)
	}
	if err := operationQueueStalled(fileops.OperationQueue{}, time.Nanosecond); err != nil {
		t.Fatalf("an empty queue cannot be stalled, got %v", err)
	}
}

// A failed connection leaves doctor without a database; the section must say so
// without counting a failure against a queue it never read.
func TestOperationQueueCheckToleratesAMissingDatabase(t *testing.T) {
	checks := &doctor{}
	checkOperationQueue(context.Background(), nil, checks, time.Hour)
	if checks.failed != 0 {
		t.Fatalf("missing database counted %d failed checks, want 0", checks.failed)
	}
}
