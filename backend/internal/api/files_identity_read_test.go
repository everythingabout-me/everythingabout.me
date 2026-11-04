package api

import (
	"context"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
	"net/http/httptest"
	"testing"
)

func TestFilesIDMetadataNeverReturnsRecreatedPathEntry(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	// Resolution still sees A at the old location, but the row returned by the
	// subsequent metadata snapshot is a replacement B. It must not escape by ID A.
	index.rows[1].ID = mutationOtherID
	w := httptest.NewRecorder()
	err := s.handleFilesMetadata(w, filesReadRequest("id="+mutationFileID), f)
	if p := filesProblem(err); p.Status != 404 || w.Body.Len() != 0 {
		t.Fatalf("replacement disclosed: %v %s", err, w.Body.String())
	}
	for _, handler := range []fileHandler{s.handleFilesList, s.handleFilesSearch, s.handleFilesCapabilities} {
		index.rows[0].ID = mutationOtherID
		err = handler(httptest.NewRecorder(), filesReadRequest("id="+mutationFolderID), f)
		if p := filesProblem(err); p.Status != 404 {
			t.Fatalf("folder replacement accepted: %v", err)
		}
	}
}

func TestFilesPathPaginationRecognizesNewFolderIncarnation(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	index.rows = []nodes.Row{filesReadRow("Scope", true), filesReadRow("Scope/a", false), filesReadRow("Scope/b", false)}
	index.rows[0].ID = mutationFolderID
	page := filesReadCollection(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2FScope&limit=1"))
	if page.NextCursor == nil {
		t.Fatal("fixture missing next page")
	}
	index.identities["Scope"] = [2]string{mutationOtherID, mutationRootID}
	index.rows[0].ID = mutationOtherID
	r := filesReadRequest("path=%2FScope&limit=1&cursor=" + *page.NextCursor)
	err := s.handleFilesList(httptest.NewRecorder(), r, f)
	if p := filesProblem(err); p.Status != 409 || p.Code != "cursor_stale" {
		t.Fatalf("old incarnation cursor: %v", err)
	}
}

func TestFilesIdentityReadRejectsMappingChangeBeforeBytes(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	if err := s.filesCheckReadIdentity(context.Background(), f, "/Scope/current.txt", mutationFileID); err != nil {
		t.Fatal(err)
	}
	index.identities["Scope/current.txt"] = [2]string{mutationOtherID, mutationFolderID}
	if err := s.filesCheckReadIdentity(context.Background(), f, "/Scope/current.txt", mutationFileID); filesProblem(err).Status != 404 {
		t.Fatalf("mapping changed: %v", err)
	}
	if !filesEqualPath(f, fc.Path("/cafe\u0301"), fc.Path("/café")) {
		t.Fatal("native normalization aliases differ")
	}
}
