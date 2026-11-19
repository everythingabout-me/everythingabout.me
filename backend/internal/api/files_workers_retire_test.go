package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

// The retirement policy is a decision about one failed attempt: which phase and mode it
// was in, how many claims the journal recorded, and whether the error is one a retry can
// ever change. It is tested as a pure function so the cases are exhaustive and no provider
// stands in for one of them.
func TestStalledUploadState(t *testing.T) {
	permanent := fileops.E("quota_exceeded", nil)
	transient := storage.NewError("backend_error", "drive/owner/a.mov", errors.New("HTTP 503 SlowDown"))
	permission := &fs.PathError{Op: "open", Path: "/spool/part", Err: fs.ErrPermission}
	preEffect := []fileops.Phase{fileops.Admitted, fileops.Receiving, fileops.StagedReady}
	postEffect := []fileops.Phase{fileops.EffectStarted, fileops.NativeApplied}

	for _, phase := range preEffect {
		for _, mode := range []string{fileops.UploadModeMultipart, fileops.UploadModeDirect} {
			// A permanent refusal before any effect ends a multipart attempt at once: no
			// number of claims can make a quota or configuration refusal succeed.
			state, ok := stalledUploadState(phase, mode, 1, false, permanent)
			if mode == fileops.UploadModeMultipart {
				if !ok || state != fileops.Failed {
					t.Fatalf("%s/%s permanent: state=%q ok=%v, want failed", phase, mode, state, ok)
				}
			} else if ok {
				// A direct client PUT may already have published, so even a permanent
				// refusal is not proof that nothing landed; it waits for the claim bound.
				t.Fatalf("%s/%s permanent: retired early as %q", phase, mode, state)
			}
		}
	}

	for _, phase := range preEffect {
		if state, ok := stalledUploadState(phase, fileops.UploadModeMultipart, uploadRetirementClaims, false, transient); !ok || state != fileops.Failed {
			t.Fatalf("%s/multipart at the claim bound: state=%q ok=%v, want failed", phase, state, ok)
		}
		if state, ok := stalledUploadState(phase, fileops.UploadModeMultipart, uploadRetirementClaims-1, false, transient); ok {
			t.Fatalf("%s/multipart one claim early: retired as %q", phase, state)
		}
		// A direct session at any pre-effect phase is not pre-effect for this rule.
		if state, ok := stalledUploadState(phase, fileops.UploadModeDirect, uploadRetirementClaims, false, transient); !ok || state != fileops.Unresolved {
			t.Fatalf("%s/direct at the claim bound: state=%q ok=%v, want unresolved", phase, state, ok)
		}
	}

	for _, phase := range postEffect {
		for _, mode := range []string{fileops.UploadModeMultipart, fileops.UploadModeDirect} {
			if state, ok := stalledUploadState(phase, mode, uploadRetirementClaims, false, transient); !ok || state != fileops.Unresolved {
				t.Fatalf("%s/%s at the claim bound: state=%q ok=%v, want unresolved", phase, mode, state, ok)
			}
		}
	}

	// Six claims is the bound: five attempts still keep retrying.
	if state, ok := stalledUploadState(fileops.NativeApplied, fileops.UploadModeMultipart, 5, false, transient); ok {
		t.Fatalf("post-effect at five claims retired as %q", state)
	}

	// A cancel recorded before any effect is honoured at once for a multipart session:
	// nothing was published, so there is no reason to make the client wait for the claim
	// bound. A direct session may already have published, so the same cancel cannot be
	// reported as Cancelled; it waits for the bound and then ends Unresolved.
	for _, phase := range preEffect {
		if state, ok := stalledUploadState(phase, fileops.UploadModeMultipart, 1, true, transient); !ok || state != fileops.Cancelled {
			t.Fatalf("%s/multipart cancelled before effect: state=%q ok=%v, want cancelled", phase, state, ok)
		}
		if state, ok := stalledUploadState(phase, fileops.UploadModeDirect, 1, true, transient); ok {
			t.Fatalf("%s/direct cancelled before effect retired as %q", phase, state)
		}
	}
	for _, phase := range postEffect {
		if state, ok := stalledUploadState(phase, fileops.UploadModeMultipart, uploadRetirementClaims, true, transient); !ok || state != fileops.Unresolved {
			t.Fatalf("%s/multipart cancelled after effect: state=%q ok=%v, want unresolved", phase, state, ok)
		}
	}

	// A permission refusal is permanent, whether it arrives bare or wrapped in a fileops
	// error, and the client-visible reason keeps the failing step.
	if !permanentOperationError(permission) || !permanentOperationError(fileops.E("invalid_intent", permission)) {
		t.Fatal("permission refusal not classified as permanent")
	}
	if permanentOperationError(transient) || permanentOperationError(nil) {
		t.Fatal("transient/nil error classified as permanent")
	}
	for _, code := range []string{"invalid_config", "invalid_intent", "unsupported", "baseline_required", "root_conflict", "quota_exceeded", "staging_full"} {
		if !permanentOperationError(fileops.E(code, nil)) {
			t.Fatalf("%s not classified as permanent", code)
		}
	}
	for _, code := range []string{"storage_failure", "upload_incomplete", "forbidden", "not_found"} {
		if permanentOperationError(fileops.E(code, nil)) {
			t.Fatalf("%s classified as permanent", code)
		}
	}
	if code := permanentOperationCode(permission); code != "permission_denied" {
		t.Fatalf("permission code = %q", code)
	}
}

func TestStalledUploadProblemNamesTheRecovery(t *testing.T) {
	var failed struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(stalledUploadProblem(fileops.Failed, fileops.E("quota_exceeded", errors.New("over the limit"))), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Code != "quota_exceeded" || failed.Detail == "" {
		t.Fatalf("failed problem %+v", failed)
	}
	var unresolved struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(stalledUploadProblem(fileops.Unresolved, errors.New("anything")), &unresolved); err != nil {
		t.Fatal(err)
	}
	if unresolved.Code != "outcome_unknown" || unresolved.Detail == "" {
		t.Fatalf("unresolved problem %+v", unresolved)
	}
	var cancelled struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(stalledUploadProblem(fileops.Cancelled, errors.New("cancel")), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Code != "upload_aborted" || cancelled.Detail == "" {
		t.Fatalf("cancelled problem %+v", cancelled)
	}
}
