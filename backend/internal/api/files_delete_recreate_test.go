package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/storage"
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

// An empty folder that a pending purge has not reached yet is the ordering the reassert
// exists for: mkdir adopts the old directory without a native write, so the only way the
// purge's conditional marker delete can be made to refuse is to move the marker's native
// identity. The accepted delete is held at running without a purge worker, so the marker
// is certainly still present when the create runs.
func TestFilesCreateReassertsEmptyFolderDuringPurge(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Empty"}`), http.Header{"Content-Type": {"application/json"}}), 201)
	before, err := os.Stat(filepath.Join(f.cfg.Storage.Local.ContentRoot, h.root.OwnerID, "Empty"))
	if err != nil {
		t.Fatal(err)
	}
	intent, err := json.Marshal(fileops.ActionRequest{Kind: "delete", From: "Empty", Recursive: true, Root: h.root, Actor: workerActor(h.root)})
	if err != nil {
		t.Fatal(err)
	}
	begun, _, err := f.journal.Begin(f.ctx, fileops.BeginRequest{Root: h.root, Actor: workerActor(h.root), Kind: "delete", Intent: intent, Guards: []fileops.Guard{{Path: "Empty", Subtree: true}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.journal.Claim(f.ctx, h.root, workerActor(h.root), begun.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := f.journal.AcceptPrefixDelete(f.ctx, claimed.Lease, "Empty", nodes.CommitPrefixDelete)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.State != fileops.Running || accepted.Purge == nil {
		t.Fatalf("delete is not an accepted pending purge: %+v", accepted)
	}
	uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Empty"}`), http.Header{"Content-Type": {"application/json"}, "Idempotency-Key": {"reassert-empty"}}), 200)
	after, err := os.Stat(filepath.Join(f.cfg.Storage.Local.ContentRoot, h.root.OwnerID, "Empty"))
	if err != nil {
		t.Fatalf("re-created empty folder is not native: %v", err)
	}
	if os.SameFile(before, after) {
		t.Fatal("mkdir during an accepted delete did not reassert the folder marker")
	}
	operation := uploadsHTTPJSON(t, h.request("GET", "/api/files/operations/"+accepted.ID, "cookie", nil, nil), 200)
	if operation["state"] != "running" {
		t.Fatalf("reassert settled the pending purge: %v", operation)
	}
}

// The reassert trigger is one bounded existence query: owner- and namespace-scoped, true
// only while the delete is accepted (Purge set) and non-terminal, and covering descendants
// but not similarly named siblings.
func TestPendingDeleteCoversIsScoped(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Scope/Empty","parents":true}`), http.Header{"Content-Type": {"application/json"}}), 201)
	intent, err := json.Marshal(fileops.ActionRequest{Kind: "delete", From: "Scope/Empty", Recursive: true, Root: h.root, Actor: workerActor(h.root)})
	if err != nil {
		t.Fatal(err)
	}
	begun, _, err := f.journal.Begin(f.ctx, fileops.BeginRequest{Root: h.root, Actor: workerActor(h.root), Kind: "delete", Intent: intent, Guards: []fileops.Guard{{Path: "Scope/Empty", Subtree: true}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.journal.Claim(f.ctx, h.root, workerActor(h.root), begun.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.journal.AcceptPrefixDelete(f.ctx, claimed.Lease, "Scope/Empty", nodes.CommitPrefixDelete); err != nil {
		t.Fatal(err)
	}
	namespace := fmt.Sprintf("%s:%s:%d:%s:%s", h.root.OwnerID, h.root.ConnectionID, h.root.Generation, h.root.ContentRoot, h.root.SystemRoot)
	request := &fileRequest{
		Principal:        &auth.Principal{User: &auth.User{ID: h.root.OwnerID}},
		Driver:           &filesReadTestDriver{descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: true}}},
		ContentNamespace: namespace,
	}
	for path, want := range map[fc.Path]bool{
		"/Scope/Empty":       true,
		"/Scope/Empty/child": true,
		"/Scope/Emptyish":    false,
		"/Scope":             false,
		"/":                  false,
	} {
		got, err := h.server.filesPendingDeleteCovers(f.ctx, request, path)
		if err != nil || got != want {
			t.Fatalf("covers %q = %v (%v), want %v", path, got, err, want)
		}
	}
	// A different owner sees nothing, even on the same path and namespace spelling.
	other := *request
	other.Principal = &auth.Principal{User: &auth.User{ID: "10000000-0000-4000-8000-999999999999"}}
	if got, err := h.server.filesPendingDeleteCovers(f.ctx, &other, "/Scope/Empty"); err != nil || got {
		t.Fatalf("another owner saw the purge: %v %v", got, err)
	}
	// A terminal delete stops covering the path.
	if _, err = f.journal.FinishPurge(f.ctx, claimed.Lease, fileops.Succeeded, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := h.server.filesPendingDeleteCovers(f.ctx, request, "/Scope/Empty"); err != nil || got {
		t.Fatalf("terminal delete still covered the path: %v %v", got, err)
	}
}
