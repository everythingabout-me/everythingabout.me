package filecontract_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	fc "everythingabout.me/backend/internal/filecontract"
)

var fixtureTime = time.Date(2026, 10, 30, 4, 0, 0, 0, time.UTC)

func fixtureMetadata(path fc.Path) fc.Metadata {
	return fc.Metadata{ID: "11111111-1111-4111-8111-111111111111", ParentID: ptr("22222222-2222-4222-8222-222222222222"), Path: path, ParentPath: "/Notes", Name: "cafe\u0301%2F+.md", Kind: "file", Revision: "nr_observed", ContentRevision: ptr("cr_observed"), Size: ptr(int64(0)), MediaType: ptr("text/markdown"), CreatedAt: fixtureTime, UpdatedAt: fixtureTime}
}

func jsonObject(t *testing.T, value any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	return object
}

func assertJSON(t *testing.T, value any, expected string) {
	t.Helper()
	got := jsonObject(t, value)
	var want map[string]any
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v; want %#v", got, want)
	}
}

func TestStableIdentityMetadataWireFixture(t *testing.T) {
	assertJSON(t, fixtureMetadata("/Notes/cafe\u0301%2F+.md"), `{
  "id":"11111111-1111-4111-8111-111111111111","parentId":"22222222-2222-4222-8222-222222222222",
  "path":"/Notes/cafe\u0301%2F+.md","parentPath":"/Notes","name":"cafe\u0301%2F+.md","kind":"file",
  "revision":"nr_observed","contentRevision":"cr_observed","size":0,"mediaType":"text/markdown",
  "sourceModifiedAt":null,"createdAt":"2026-10-30T04:00:00Z","updatedAt":"2026-10-30T04:00:00Z","checksum":null
 }`)
	folder := fc.Metadata{Path: "/Notes", ParentPath: "/", Name: "Notes", Kind: "folder", Revision: "nr_folder", CreatedAt: fixtureTime, UpdatedAt: fixtureTime}
	assertJSON(t, folder, `{
  "id":"","parentId":null,"path":"/Notes","parentPath":"/","name":"Notes","kind":"folder","revision":"nr_folder",
  "contentRevision":null,"size":null,"mediaType":null,"sourceModifiedAt":null,
  "createdAt":"2026-10-30T04:00:00Z","updatedAt":"2026-10-30T04:00:00Z","checksum":null
 }`)
	assertJSON(t, fc.NewCollection[fc.Metadata](nil, nil), `{"items":[],"nextCursor":null}`)
}

func TestNamespaceRequestsUseFullPaths(t *testing.T) {
	assertJSON(t, fc.CreateFolderRequest{Kind: "folder", Path: "/Notes/2026", Parents: true}, `{"kind":"folder","path":"/Notes/2026","parents":true}`)
	assertJSON(t, fc.MoveRequest{From: "/Notes/a", To: "/Archive/a", IfRevision: "nr_a"}, `{"from":"/Notes/a","to":"/Archive/a","ifRevision":"nr_a"}`)
	assertJSON(t, fc.CopyRequest{From: "/Notes/a", To: "/Archive/a", IfSourceRevision: "nr_a"}, `{"from":"/Notes/a","to":"/Archive/a","ifSourceRevision":"nr_a"}`)
	// JSON is literal: a percent-looking segment is never query-decoded.
	var request fc.MoveRequest
	if err := json.Unmarshal([]byte(`{"from":"/Notes/%2e%2e","to":"/Archive/cafe\u0301","ifRevision":"nr_a"}`), &request); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, fc.ValidateMove(request), 0, "")
	if request.From != "/Notes/%2e%2e" || request.To != "/Archive/cafe\u0301" {
		t.Fatalf("JSON paths changed: %+v", request)
	}
}

func TestProblemWireFixture(t *testing.T) {
	problem := fc.NewProblem(412, "precondition_failed", "The file changed before publication.")
	problem.RequestID = "req_fixture"
	assertJSON(t, problem, `{"type":"urn:eam:error:precondition_failed","title":"Precondition Failed","status":412,"code":"precondition_failed","detail":"The file changed before publication.","requestId":"req_fixture"}`)
}

func TestOperationWireFixtures(t *testing.T) {
	op := fc.Operation{ID: "o_fixture", Kind: fc.OperationMove, State: fc.OperationOutcomeUnknown, Progress: fc.Progress{Completed: 1, Unit: "entries"}, CancelRequested: true, CreatedAt: fixtureTime}
	// Unknown native effects are nonterminal even if some entries are complete and
	// cancellation was requested. This fixture represents the unresolved state; it
	// does not implement durable path guards, worker fencing, or reconciliation.
	assertJSON(t, op, `{
  "id":"o_fixture","kind":"move","state":"outcome_unknown","progress":{"completed":1,"total":null,"unit":"entries"},
  "result":null,"error":null,"cancelRequested":true,"createdAt":"2026-10-30T04:00:00Z","finishedAt":null
 }`)
	for _, state := range []fc.OperationState{fc.OperationQueued, fc.OperationRunning, fc.OperationOutcomeUnknown, fc.OperationSucceeded, fc.OperationFailed, fc.OperationCancelled} {
		t.Run(string(state), func(t *testing.T) {
			value := op
			value.State = state
			if got := jsonObject(t, value)["state"]; got != string(state) {
				t.Fatalf("state = %v", got)
			}
		})
	}
	metadata := fixtureMetadata("/Notes/cafe\u0301%2F+.md")
	for _, kind := range []fc.OperationKind{fc.OperationMkdir, fc.OperationMove, fc.OperationCopy, fc.OperationUploadCommit} {
		t.Run(string(kind), func(t *testing.T) {
			value := op
			value.Kind, value.State, value.Result, value.FinishedAt = kind, fc.OperationSucceeded, metadata, &fixtureTime
			result := jsonObject(t, value)["result"].(map[string]any)
			if result["path"] != string(metadata.Path) || result["revision"] != metadata.Revision {
				t.Fatalf("result = %#v", result)
			}
			if result["id"] != metadata.ID || result["parentId"] != *metadata.ParentID {
				t.Fatal("file result lost stable identity")
			}
		})
	}
	op.Kind, op.State, op.Result = fc.OperationDelete, fc.OperationSucceeded, fc.DeleteResult{Deleted: true}
	assertJSON(t, op.Result, `{"deleted":true}`)
	op.Kind, op.Result = fc.OperationThumbnail, fc.ThumbnailResult{ContentURL: "/api/files/thumbnail?path=%2FNotes%2Fa.png&width=320", MediaType: "image/webp", Size: 1024, ContentRevision: "cr_a"}
	assertJSON(t, op.Result, `{"contentUrl":"/api/files/thumbnail?path=%2FNotes%2Fa.png&width=320","mediaType":"image/webp","size":1024,"contentRevision":"cr_a"}`)
	op.State, op.Result, op.Error = fc.OperationFailed, nil, fc.NewProblem(409, "name_conflict", "The destination already exists.")
	errorJSON := jsonObject(t, op)["error"].(map[string]any)
	if errorJSON["type"] != "urn:eam:error:name_conflict" || errorJSON["status"] != float64(409) {
		t.Fatalf("operation error = %#v", errorJSON)
	}
}
