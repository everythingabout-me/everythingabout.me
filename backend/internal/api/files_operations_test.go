package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"everythingabout.me/backend/internal/fileops"
)

// progressPayload is the part of the Operation resource a client reads to decide
// whether an operation is moving.
type progressPayload struct {
	State    string `json:"state"`
	Progress struct {
		Completed int    `json:"completed"`
		Total     *int   `json:"total"`
		Unit      string `json:"unit"`
	} `json:"progress"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func decodeProgress(t *testing.T, op fileops.Operation) progressPayload {
	t.Helper()
	w := httptest.NewRecorder()
	if err := writeFilesOperationWithRetry(w, op, 200, nil); err != nil {
		t.Fatalf("write operation: %v", err)
	}
	var payload progressPayload
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode operation: %v\n%s", err, w.Body.String())
	}
	return payload
}

// An accepted recursive deletion has no manifest, so counting entries reported 0/0 for
// the entire byte purge. The counters have to be the purge's own, and the total has to be
// the extent of the walk: a client polling a delete for a minute needs a fraction it can
// render, not a null it can only stare at.
func TestPurgeDeleteReportsObjectProgress(t *testing.T) {
	payload := decodeProgress(t, fileops.Operation{
		ID:    "op-purge",
		Kind:  "delete",
		State: fileops.Running,
		Purge: &fileops.PurgeState{Deleted: 4097, Adopted: 3, Total: 5000},
	})
	if payload.Progress.Unit != "objects" {
		t.Fatalf("unit=%q want objects", payload.Progress.Unit)
	}
	if payload.Progress.Completed != 4100 {
		t.Fatalf("completed=%d want 4100 (purged plus adopted)", payload.Progress.Completed)
	}
	if payload.Progress.Total == nil || *payload.Progress.Total != 5000 {
		t.Fatalf("total=%v want 5000", payload.Progress.Total)
	}
}

// A running deletion has not answered yet. `result` is the answer, so it may not carry a
// settled `{deleted:true}` next to `state:"running"` — that pair is what made a working
// purge look finished.
func TestRunningPurgePublishesNoResult(t *testing.T) {
	payload := decodeProgress(t, fileops.Operation{
		ID:    "op-purge",
		Kind:  "delete",
		State: fileops.Running,
		Purge: &fileops.PurgeState{Deleted: 12, Total: 40},
	})
	if payload.State != "running" {
		t.Fatalf("state=%q want running", payload.State)
	}
	if len(payload.Result) != 0 && string(payload.Result) != "null" {
		t.Fatalf("running purge published result %s", payload.Result)
	}
}

// Null is reserved for the one moment the extent is genuinely unknown: before the first
// walk has looked at the prefix. Once a walk has counted the objects, the total is a
// number even when nothing has been deleted yet.
func TestPurgeTotalIsNullOnlyBeforeTheFirstWalk(t *testing.T) {
	waiting := decodeProgress(t, fileops.Operation{ID: "op-purge", Kind: "delete", State: fileops.Running, Purge: &fileops.PurgeState{}})
	if waiting.Progress.Total != nil {
		t.Fatalf("total=%d before any walk, want null", *waiting.Progress.Total)
	}
	walked := decodeProgress(t, fileops.Operation{ID: "op-purge", Kind: "delete", State: fileops.Running, Purge: &fileops.PurgeState{Total: 120}})
	if walked.Progress.Total == nil || *walked.Progress.Total != 120 || walked.Progress.Completed != 0 {
		t.Fatalf("progress=%+v want 0 of 120", walked.Progress)
	}
}

// A finished deletion has to read as finished: every object the extent counted is
// settled, and the result names what happened to the bytes.
func TestFinishedPurgeLooksFinished(t *testing.T) {
	payload := decodeProgress(t, fileops.Operation{
		ID:     "op-purge",
		Kind:   "delete",
		State:  fileops.Succeeded,
		Purge:  &fileops.PurgeState{Deleted: 12, Adopted: 1, Total: 13},
		Result: json.RawMessage(`{"deleted":true,"purged":12,"adopted":1}`),
	})
	if payload.Progress.Total == nil || *payload.Progress.Total != payload.Progress.Completed {
		t.Fatalf("progress=%+v want completed == total", payload.Progress)
	}
	var result struct {
		Deleted bool `json:"deleted"`
		Purged  int  `json:"purged"`
	}
	if err := json.Unmarshal(payload.Result, &result); err != nil || !result.Deleted || result.Purged != 12 {
		t.Fatalf("result=%s err=%v", payload.Result, err)
	}
}

// A purge that never started a walk still finishes with a number: zero of zero objects
// means "nothing was left to purge", which is a complete answer, not a missing one.
func TestFinishedPurgeWithoutAWalkReportsZero(t *testing.T) {
	payload := decodeProgress(t, fileops.Operation{
		ID:     "op-purge",
		Kind:   "delete",
		State:  fileops.Succeeded,
		Purge:  &fileops.PurgeState{},
		Result: json.RawMessage(`{"deleted":true,"purged":0,"adopted":0}`),
	})
	if payload.Progress.Total == nil || *payload.Progress.Total != 0 || payload.Progress.Completed != 0 {
		t.Fatalf("progress=%+v want 0 of 0", payload.Progress)
	}
}

// A manifest operation keeps reporting its entries, so the purge branch cannot have
// taken over the kind it was cut from.
func TestManifestOperationStillReportsEntries(t *testing.T) {
	payload := decodeProgress(t, fileops.Operation{
		ID:    "op-copy",
		Kind:  "copy",
		State: fileops.Running,
		Entries: []fileops.EntryEffect{
			{ManifestEntry: fileops.ManifestEntry{Number: 0}, State: "applied"},
			{ManifestEntry: fileops.ManifestEntry{Number: 1}, State: "started"},
		},
	})
	if payload.Progress.Unit != "entries" || payload.Progress.Completed != 1 {
		t.Fatalf("progress=%+v want 1 of 2 entries", payload.Progress)
	}
	if payload.Progress.Total == nil || *payload.Progress.Total != 2 {
		t.Fatalf("total=%v want 2", payload.Progress.Total)
	}
}
