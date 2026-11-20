package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/nodes"
)

// GET /list answers with the selected entry as well as its children. A file used to be
// refused with 409 not_a_folder, which forced every file preview to fetch metadata in
// parallel and watch the listing request fail; the entry in the page makes one request
// enough for both a folder's first paint and a file's preview.
func TestFilesListCarriesSelectedEntryAndChildren(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	index.rows = []nodes.Row{filesReadRow("Scope", true), filesReadRow("Scope/report.html", false), filesReadRow("Scope/notes.txt", false)}
	index.rows[0].ID = mutationFolderID

	file := filesReadBrowse(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2FScope%2Freport.html"))
	if file.Node.Kind != "file" || file.Node.Path != "/Scope/report.html" || file.Node.ID == "" {
		t.Fatalf("a file is a browse target whose page names it: node=%+v", file.Node)
	}
	if len(file.Items) != 0 || file.NextCursor != nil {
		t.Fatalf("a file page has no children: items=%d cursor=%v", len(file.Items), file.NextCursor)
	}

	folder := filesReadBrowse(t, filesReadCall(t, s, f, s.handleFilesList, "path=%2FScope"))
	if folder.Node.Kind != "folder" || folder.Node.Path != "/Scope" {
		t.Fatalf("folder page node=%+v", folder.Node)
	}
	if len(folder.Items) != 2 || folder.Items[0].ParentPath != "/Scope" {
		t.Fatalf("folder page items=%+v", folder.Items)
	}
}

// GET /neighbors is the pair a viewer steps between. Both keys are always present, because
// the client hides one arrow on a null and has to be able to tell "no neighbor" from "the
// field is missing"; and a folder is refused rather than answered empty, because a folder is
// an entry point, not a page the arrows can turn to.
func TestFilesNeighborsAnswersThePairAndRefusesAFolder(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	index.rows = []nodes.Row{
		filesReadRow("Album", true),
		filesReadRow("Album/A.txt", false),
		filesReadRow("Album/b.txt", false),
		filesReadRow("Album/Z.txt", false),
	}
	index.rows[0].ID = mutationFolderID

	w := filesReadCall(t, s, f, s.handleFilesNeighbors, "path=%2FAlbum%2FZ.txt")
	var pair filesNeighborsResource
	if err := json.Unmarshal(w.Body.Bytes(), &pair); err != nil {
		t.Fatal(err)
	}
	if pair.Previous == nil || pair.Previous.Path != "/Album/A.txt" || pair.Next == nil || pair.Next.Path != "/Album/b.txt" {
		t.Fatalf("neighbors of Z.txt: previous=%+v next=%+v", pair.Previous, pair.Next)
	}
	if index.neighborPaths[0] != "Album/Z.txt" || len(index.neighborHidden) != 0 {
		t.Fatalf("neighbor read target=%q hidden=%v", index.neighborPaths[0], index.neighborHidden)
	}

	first := filesReadNeighbors(t, filesReadCall(t, s, f, s.handleFilesNeighbors, "path=%2FAlbum%2FA.txt"))
	if first.Previous != nil || first.Next == nil || first.Next.Path != "/Album/Z.txt" {
		t.Fatalf("first file: previous=%+v next=%+v", first.Previous, first.Next)
	}
	if body := filesReadCall(t, s, f, s.handleFilesNeighbors, "path=%2FAlbum%2FA.txt").Body.String(); !strings.Contains(body, `"previous":null`) {
		t.Fatalf("an absent neighbor must be an explicit null: %s", body)
	}

	err := s.handleFilesNeighbors(httptest.NewRecorder(), filesReadRequest("path=%2FAlbum"), f)
	filesReadProblem(t, err, 409, "not_a_file")
}

func filesReadNeighbors(t *testing.T, w *httptest.ResponseRecorder) filesNeighborsResource {
	t.Helper()
	var result filesNeighborsResource
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
