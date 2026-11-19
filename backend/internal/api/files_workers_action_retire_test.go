package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// A move, copy or delete that keeps failing has to reach a terminal state on its own:
// while it is non-terminal its guards hold the path, and every later mutation of that
// path answers 409 until the operation ends. The policy is a decision about one failed
// attempt, pure so every case is exhaustive without a provider or a journal.
func TestStalledActionState(t *testing.T) {
	transient := storage.NewError("backend_error", "drive/owner/a", errors.New("HTTP 503 SlowDown"))
	permanent := fileops.E("root_conflict", nil)
	uncertain := fileops.Operation{Phase: fileops.EffectStarted, State: fileops.Unknown}
	preEffect := fileops.Operation{Phase: fileops.Admitted}

	// An effect may have landed: only Unresolved is truthful, and the owner's cancel is
	// honoured immediately instead of waiting out a budget for the same non-answer.
	cancelled := uncertain
	cancelled.CancelRequested = true
	if state, ok := stalledActionState(cancelled, 1, false, fileops.E("outcome_unknown", nil)); !ok || state != fileops.Unresolved {
		t.Fatalf("cancelled uncertain effect: state=%q ok=%v, want unresolved", state, ok)
	}
	// An attempt that resolved an entry is making progress, and a cancel must not throw
	// that answer away: it keeps the ordinary attempt budget instead of ending at once.
	if state, ok := stalledActionState(cancelled, 1, true, fileops.E("outcome_unknown", nil)); ok {
		t.Fatalf("cancel retired an attempt that made progress as %q", state)
	}
	// Without the cancel, the claim budget still applies: five attempts keep retrying.
	if state, ok := stalledActionState(uncertain, actionRetirementAttempts-1, false, transient); ok {
		t.Fatalf("uncertain effect one attempt early retired as %q", state)
	}
	if state, ok := stalledActionState(uncertain, actionRetirementAttempts, false, transient); !ok || state != fileops.Unresolved {
		t.Fatalf("uncertain effect at the bound: state=%q ok=%v, want unresolved", state, ok)
	}

	// Nothing reached the native tree, so the answer is the refusal or the owner's intent.
	if state, ok := stalledActionState(preEffect, 1, false, permanent); !ok || state != fileops.Failed {
		t.Fatalf("permanent refusal before any effect: state=%q ok=%v, want failed", state, ok)
	}
	cancelledPre := preEffect
	cancelledPre.CancelRequested = true
	if state, ok := stalledActionState(cancelledPre, 1, false, permanent); !ok || state != fileops.Cancelled {
		t.Fatalf("cancelled permanent refusal: state=%q ok=%v, want cancelled", state, ok)
	}
	if state, ok := stalledActionState(cancelledPre, actionRetirementAttempts, false, transient); !ok || state != fileops.Cancelled {
		t.Fatalf("cancelled pre-effect stall: state=%q ok=%v, want cancelled", state, ok)
	}
	if state, ok := stalledActionState(preEffect, actionRetirementAttempts-1, false, transient); ok {
		t.Fatalf("pre-effect stall one attempt early retired as %q", state)
	}
	if state, ok := stalledActionState(preEffect, actionRetirementAttempts, false, transient); !ok || state != fileops.Failed {
		t.Fatalf("pre-effect stall at the bound: state=%q ok=%v, want failed", state, ok)
	}
}

func TestRetiredActionProblemExplainsTheResidue(t *testing.T) {
	var unresolved struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(retiredActionProblem(fileops.Unresolved, true, fileops.E("outcome_unknown", nil)), &unresolved); err != nil {
		t.Fatal(err)
	}
	if unresolved.Code != "outcome_unknown" || unresolved.Detail == "" {
		t.Fatalf("unresolved problem %+v", unresolved)
	}
	var cancelled struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(retiredActionProblem(fileops.Cancelled, true, nil), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Code != "cancelled" || cancelled.Detail == "" {
		t.Fatalf("cancelled problem %+v", cancelled)
	}
	var failed struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(retiredActionProblem(fileops.Failed, false, fileops.E("root_conflict", nil)), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Code != "root_conflict" || failed.Detail == "" {
		t.Fatalf("failed problem %+v", failed)
	}
}

// A joined error used to hide the leaf cause from the only diagnostic line a stuck
// operation produces: errors.Unwrap answers nil for a join, so the chain stopped at the
// first branch and printed the join's own text.
func TestErrorCauseChainReachesJoinedCauses(t *testing.T) {
	leaf := storage.NewError("backend_error", "drive/owner/a", errors.New("HTTP 503"))
	err := errors.Join(fileops.E("native_effect_unresolved", leaf), fileops.E("outcome_unknown", nil))
	chain := errorCauseChain(err)
	for _, want := range []string{"native_effect_unresolved", "backend_error", "outcome_unknown"} {
		if !strings.Contains(chain, want) {
			t.Fatalf("chain %q does not name %q", chain, want)
		}
	}
}
