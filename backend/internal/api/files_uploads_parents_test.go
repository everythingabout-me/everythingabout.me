package api

import (
	"encoding/json"
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
