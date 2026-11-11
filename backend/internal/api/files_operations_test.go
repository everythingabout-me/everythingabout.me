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
	Progress struct {
		Completed int    `json:"completed"`
		Total     *int   `json:"total"`
		Unit      string `json:"unit"`
	} `json:"progress"`
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
// the entire byte purge. That is the shape that reads as "nothing is happening" while
// thousands of objects are going away: the counters have to be the purge's own.
func TestPurgeDeleteReportsObjectProgress(t *testing.T) {
	payload := decodeProgress(t, fileops.Operation{
		ID:    "op-purge",
		Kind:  "delete",
		State: fileops.Running,
		Purge: &fileops.PurgeState{Deleted: 4097, Adopted: 3},
	})
	if payload.Progress.Unit != "objects" {
		t.Fatalf("unit=%q want objects", payload.Progress.Unit)
	}
	if payload.Progress.Completed != 4100 {
		t.Fatalf("completed=%d want 4100 (purged plus adopted)", payload.Progress.Completed)
	}
	if payload.Progress.Total != nil {
		t.Fatalf("total=%d want null: the purge discovers its extent as it walks", *payload.Progress.Total)
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
