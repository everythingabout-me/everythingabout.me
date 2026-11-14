package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A creation over bytes the library already holds opens no session: the digest recorded
// with the settlement that published them is the whole answer. A 20k-file re-import of an
// unchanged tree therefore costs one request per file and moves nothing, where it used to
// cost a session, a guard and a settlement per file.
func TestFilesUploadCreateSkipsUnchangedContent(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	data := []byte("same bytes")
	digest := sha256.Sum256(data)
	attempt := 0
	create := func(value string) *httptest.ResponseRecorder {
		attempt++
		body, _ := json.Marshal(map[string]any{
			"path": "/kept.txt", "size": len(data), "mediaType": "text/plain", "contentSha256": value,
		})
		headers := http.Header{"Content-Type": {"application/json"}}
		headers.Set("Idempotency-Key", "kept-"+string(rune('a'+attempt)))
		return h.request("POST", "/api/files/uploads", h.bearer, body, headers)
	}

	// The first import pays: it publishes the bytes and records the assertion with them.
	upload := uploadsHTTPJSON(t, create(hex.EncodeToString(digest[:])), http.StatusCreated)
	id, _ := upload["id"].(string)
	if id == "" {
		t.Fatalf("first creation opened no session: %v", upload)
	}
	uploadsHTTPJSON(t, h.part(id, 1, data, nil), http.StatusCreated)
	h.commit(t, id)

	// The second asks for the same bytes and gets the entry, not a transport.
	again := uploadsHTTPJSON(t, create(hex.EncodeToString(digest[:])), http.StatusOK)
	if again["unchanged"] != true || again["id"] != nil {
		t.Fatalf("an unchanged destination opened a session: %v", again)
	}
	item, _ := again["item"].(map[string]any)
	if item["path"] != "/kept.txt" || item["contentRevision"] == nil {
		t.Fatalf("the skip did not answer with the entry: %v", again)
	}

	// Different content at the same path is a decision for the person, not a skip.
	other := sha256.Sum256([]byte("different bytes"))
	uploadsHTTPProblem(t, create(hex.EncodeToString(other[:])), http.StatusConflict, "name_conflict")

	// A malformed digest is refused rather than silently dropped: a client that computes one
	// must not believe its creates carry it when they do not.
	uploadsHTTPProblem(t, create("not-a-digest"), http.StatusBadRequest, "invalid_request")
}
