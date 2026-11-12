package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
)

// An accepted deletion is final for every read at once, but it is not a fence on the name:
// the acceptance transaction removes the rows, records the subtree change, refunds the
// quota and releases the guard, so the path is free while the byte purge is still running.
// A create or upload aimed at that path must therefore be an ordinary create — not
// 404 not_found from the read overlay, and not 409 operation_in_progress from a delete
// guard that acceptance has already released (docs/design/deletes.md, docs/api/api.md).
func TestFilesCreateDuringAcceptedDeletePurge(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	created := uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Gone"}`), http.Header{"Content-Type": {"application/json"}}), 201)
	if w := h.request("PUT", "/api/files/content?path=%2FGone%2Fold.txt", h.bearer, []byte("old"), http.Header{"Content-Type": {"text/plain"}}); w.Code != 201 {
		t.Fatalf("seed file: HTTP%d %s", w.Code, w.Body.String())
	}

	// Accept the deletion without queueing the purge worker: the operation stays
	// non-terminal while the index has already forgotten the subtree, which is exactly the
	// state the client sees for the whole purge.
	intent, err := json.Marshal(fileops.ActionRequest{Kind: "delete", From: "Gone", Recursive: true, Root: h.root, Actor: workerActor(h.root)})
	if err != nil {
		t.Fatal(err)
	}
	begun, _, err := f.journal.Begin(f.ctx, fileops.BeginRequest{Root: h.root, Actor: workerActor(h.root), Kind: "delete", Intent: intent, Guards: []fileops.Guard{{Path: "Gone", Subtree: true}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.journal.Claim(f.ctx, h.root, workerActor(h.root), begun.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := f.journal.AcceptPrefixDelete(f.ctx, claimed.Lease, "Gone", nodes.CommitPrefixDelete)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != fileops.Running || accepted.Purge == nil {
		t.Fatalf("delete is not an accepted pending purge: %+v", accepted)
	}

	// Reads keep concealing the captured subtree until the purge is terminal.
	for _, target := range []string{
		"/api/files?path=%2FGone",
		"/api/files/list?path=%2FGone",
		"/api/files?path=%2FGone%2Fold.txt",
	} {
		uploadsHTTPProblem(t, h.request("GET", target, h.bearer, nil, nil), 404, "not_found")
	}

	// Deleting an already-deleted subtree is unchanged: it stays not_found and begins no
	// second deletion, even while its purge is non-terminal.
	uploadsHTTPProblem(t, h.request("DELETE", "/api/files?path=%2FGone&recursive=true", h.bearer, nil, nil), 404, "not_found")

	// The name is free: the same folder can be created again while the purge runs (the
	// native marker may still be there, so mkdir adopts it and answers 200).
	recreated := uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Gone"}`), http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {"recreate-during-purge"}}), 200)
	if recreated["path"] != "/Gone" || recreated["kind"] != "folder" || recreated["id"] == created["id"] {
		t.Fatalf("re-created folder did not get a fresh identity: %v", recreated)
	}
	// And an upload into that re-created folder both opens and publishes.
	id := h.newUpload(t, "/Gone/new.txt", "recreate-during-purge-upload", 3)
	uploadsHTTPJSON(t, h.part(id, 1, []byte("new"), nil), 201)
	h.commit(t, id)

	// The purge is still the operation it was; nothing above turned it terminal.
	operation := uploadsHTTPJSON(t, h.request("GET", "/api/files/operations/"+accepted.ID, "cookie", nil, nil), 200)
	if operation["state"] != "running" || operation["result"] != nil {
		t.Fatalf("re-creation settled the pending purge: %v", operation)
	}
}
