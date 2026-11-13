package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// An upload may ask for the folders above it instead of sending one mkdir per
// directory first. The rule it relaxes is who creates that folder, not whether the
// parent exists before the file is published: the chain is a real indexed folder
// with the same identity an explicit create would give it.
func TestFilesUploadParentsCreateAncestors(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	body := func(path string, parents bool) []byte {
		data, _ := json.Marshal(map[string]any{
			"path": path, "size": 5, "mediaType": "application/octet-stream", "parents": parents,
		})
		return data
	}
	headers := func(key string) http.Header {
		return http.Header{"Idempotency-Key": {key}, "Content-Type": {"application/json"}}
	}
	uploadsHTTPProblem(t, h.request("POST", "/api/files/uploads", h.bearer, body("/deep/tree/a.bin", false), headers("parents-off")), 409, "parent_missing")
	session := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads", h.bearer, body("/deep/tree/a.bin", true), headers("parents-on")), 201)
	if session["path"] != "/deep/tree/a.bin" {
		t.Fatalf("session target %v", session)
	}
	for _, path := range []string{"/deep", "/deep/tree"} {
		meta := uploadsHTTPJSON(t, h.request("GET", "/api/files?path="+path, h.bearer, nil, nil), 200)
		if meta["kind"] != "folder" || meta["id"] == "" || meta["path"] != path {
			t.Fatalf("%s is not the folder an explicit create would produce: %v", path, meta)
		}
	}
	// A second upload under the same tree finds the folders and does not recreate them.
	again := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads", h.bearer, body("/deep/tree/b.bin", true), headers("parents-again")), 201)
	if again["path"] != "/deep/tree/b.bin" {
		t.Fatalf("second session %v", again)
	}
}

// A retry is the same request: the refused create was not journaled, so sending it again
// with the flag set — same idempotency key, same destination — must succeed instead of
// replaying 409 parent_missing forever. This is the row the product owner could not retry:
// its first attempt ran a client that did not ask for the ancestors.
func TestFilesUploadParentsRetrySameKeySucceeds(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	body := func(parents bool) []byte {
		data, _ := json.Marshal(map[string]any{
			"path": "/retried/tree/a.bin", "size": 5, "mediaType": "application/octet-stream", "parents": parents,
		})
		return data
	}
	headers := http.Header{"Idempotency-Key": {"retried-parents"}, "Content-Type": {"application/json"}}
	uploadsHTTPProblem(t, h.request("POST", "/api/files/uploads", h.bearer, body(false), headers), 409, "parent_missing")
	if meta := uploadsHTTPJSON(t, h.request("GET", "/api/files?path=%2Fretried", h.bearer, nil, nil), 404); meta["code"] != "not_found" {
		t.Fatalf("a refused create must not materialize a folder: %v", meta)
	}
	session := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads", h.bearer, body(true), headers), 201)
	if session["path"] != "/retried/tree/a.bin" {
		t.Fatalf("the retried request bound the wrong target: %v", session)
	}
	uploadsHTTPJSON(t, h.request("GET", "/api/files?path=%2Fretried%2Ftree", h.bearer, nil, nil), 200)
}

// The client's standing replace answer admits a re-import as overwrite sessions, which are
// exactly the rows that meet a folder an earlier cancelled import never created. Overwrite
// must therefore be able to ask for its ancestors too; without the flag it keeps the
// destination's own refusal.
func TestFilesUploadOverwriteParentsCreateAncestors(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	body := func(path string, parents bool) []byte {
		data, _ := json.Marshal(map[string]any{
			"path": path, "size": 5, "mediaType": "application/octet-stream",
			"condition": map[string]any{"mode": "overwrite"}, "parents": parents,
		})
		return data
	}
	headers := func(key string) http.Header {
		return http.Header{"Idempotency-Key": {key}, "Content-Type": {"application/json"}}
	}
	uploadsHTTPProblem(t, h.request("POST", "/api/files/uploads", h.bearer, body("/overwrite/tree/a.bin", false), headers("overwrite-off")), 409, "parent_missing")
	session := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads", h.bearer, body("/overwrite/tree/a.bin", true), headers("overwrite-on")), 201)
	if session["path"] != "/overwrite/tree/a.bin" {
		t.Fatalf("session target %v", session)
	}
	for _, path := range []string{"/overwrite", "/overwrite/tree"} {
		meta := uploadsHTTPJSON(t, h.request("GET", "/api/files?path="+path, h.bearer, nil, nil), 200)
		if meta["kind"] != "folder" || meta["path"] != path {
			t.Fatalf("%s is not the folder the overwrite imported: %v", path, meta)
		}
	}
	// The session is bound to the parent the chain just materialized, not to a path it will
	// resolve again at commit: the intent records the parent identity it observed.
	var document []byte
	if err := f.db.Pool().QueryRow(f.ctx, `select o.snapshot from file_uploads u join file_operations o on o.id=u.operation_id where u.owner_id=$1 and u.idempotency_key=$2`, h.root.OwnerID, "overwrite-on").Scan(&document); err != nil {
		t.Fatal(err)
	}
	var op struct {
		Intent struct{ ParentPath string }
	}
	if err := json.Unmarshal(document, &op); err != nil {
		t.Fatal(err)
	}
	if op.Intent.ParentPath != "overwrite/tree" {
		t.Fatalf("session bound to parent %q, want the materialized overwrite/tree", op.Intent.ParentPath)
	}
}

// One admission page is one dropped tree, so its items share one ancestor materialization.
// A re-import left a whole folder absent for exactly this shape: a page of files under one
// destination. Without the page guard every item pays the same mkdir chain — the native
// folder still ends up created once, but the provider requests and operation rows are paid
// once per file. Two mkdir operations (one per missing level) is the whole page's cost.
func TestFilesUploadBatchParentsCreateChainOnce(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	items := make([]map[string]any, 32)
	for index := range items {
		items[index] = map[string]any{
			"path": fmt.Sprintf("/imports/tree/file-%02d.bin", index), "size": 5,
			"mediaType": "application/octet-stream", "parents": true,
			"idempotencyKey": fmt.Sprintf("import-page-%02d", index),
		}
	}
	body, _ := json.Marshal(map[string]any{"items": items})
	page := uploadsHTTPJSON(t, h.request("POST", "/api/files/uploads/batch", h.bearer, body, http.Header{"Content-Type": {"application/json"}}), 200)
	answers, _ := page["items"].([]any)
	if len(answers) != len(items) {
		t.Fatalf("page items %v", page)
	}
	for index, raw := range answers {
		upload, _ := raw.(map[string]any)["upload"].(map[string]any)
		if upload["id"] == "" || upload["path"] != items[index]["path"] {
			t.Fatalf("item %d was not admitted: %v", index, raw)
		}
	}
	var mkdirs int
	if err := f.db.Pool().QueryRow(f.ctx, `select count(*) from file_operations where owner_id=$1 and action='mkdir'`, h.root.OwnerID).Scan(&mkdirs); err != nil {
		t.Fatal(err)
	}
	if mkdirs != 2 {
		t.Fatalf("a page under one absent folder created %d mkdir operations, want one per missing level (2)", mkdirs)
	}
	// The ancestors are ordinary folder rows: they list, and the empty folder has its marker.
	listing := uploadsHTTPJSON(t, h.request("GET", "/api/files/list?path=%2Fimports", h.bearer, nil, nil), 200)
	rows, _ := listing["items"].([]any)
	found := false
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["name"] == "tree" && row["kind"] == "folder" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the created ancestor is not an ordinary listed folder: %v", listing)
	}
	uploadsHTTPJSON(t, h.request("GET", "/api/files?path=%2Fimports%2Ftree", h.bearer, nil, nil), 200)
}
