package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestFileOperationBackoffDoublesToACeiling(t *testing.T) {
	cases := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: fileOperationRetryFloor},
		{attempts: 1, want: fileOperationRetryFloor},
		{attempts: 2, want: 2 * fileOperationRetryFloor},
		{attempts: 3, want: 4 * fileOperationRetryFloor},
		{attempts: 60, want: fileOperationRetryCeiling},
	}
	for _, c := range cases {
		if got := fileOperationBackoff(c.attempts); got != c.want {
			t.Fatalf("backoff(%d) = %v, want %v", c.attempts, got, c.want)
		}
	}
	previous := time.Duration(0)
	for attempts := 1; attempts <= 40; attempts++ {
		wait := fileOperationBackoff(attempts)
		if wait < previous || wait > fileOperationRetryCeiling {
			t.Fatalf("backoff(%d) = %v, not monotonic within the ceiling (previous %v)", attempts, wait, previous)
		}
		previous = wait
	}
}

// A log line has to name every step that failed, otherwise the only record of a
// stuck operation is its outermost wrapper.
func TestErrorCauseChainNamesEveryLayer(t *testing.T) {
	chain := fileops.E("outcome_unknown", storage.NewError("timeout", "drive/owner/a.mov", errors.New("provider gave up")))
	got := errorCauseChain(chain)
	want := "outcome_unknown <- timeout <- provider gave up"
	if got != want {
		t.Fatalf("cause chain = %q, want %q", got, want)
	}
	if code := fileOperationErrorCode(chain); code != "outcome_unknown" {
		t.Fatalf("code = %q, want the outermost code", code)
	}
	if code := fileOperationErrorCode(context.DeadlineExceeded); code != "deadline_exceeded" {
		t.Fatalf("deadline code = %q", code)
	}
	if got := errorCauseChain(nil); got != "" {
		t.Fatalf("nil chain = %q", got)
	}
}

// A retry that keeps failing must be recorded, reported, and deferred instead of
// re-running on every scan tick: the record is what makes a stuck operation visible
// on its resource and in the log.
func TestFileOperationRetainedRecordsAndBacksOff(t *testing.T) {
	server := &Server{}
	op := fileops.Operation{ID: "6f1f8fd7-3a2c-4c9b-9d5e-9d2e6b1d1a11", Kind: "delete", State: "running", Phase: "native_applied"}
	err := fileops.E("recovery_required", nil)

	server.fileOperationRetained(op, err)
	first := server.fileOperationRetry(op.ID)
	if first == nil {
		t.Fatal("no retry record after a failed attempt")
	}
	if first["attempts"] != 1 || first["code"] != "recovery_required" {
		t.Fatalf("retry record = %v", first)
	}
	server.fileOperationRetained(op, err)
	second := server.fileOperationRetry(op.ID)
	if second["attempts"] != 2 {
		t.Fatalf("attempts = %v, want 2", second["attempts"])
	}
	if !second["nextAttemptAt"].(time.Time).After(first["nextAttemptAt"].(time.Time)) {
		t.Fatal("the second failure did not back off further")
	}

	server.fileOperationProgressed(op.ID)
	if record := server.fileOperationRetry(op.ID); record != nil {
		t.Fatalf("progress kept the failure record: %v", record)
	}
}
