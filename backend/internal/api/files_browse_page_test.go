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

// GET /neighbors is the run a viewer steps through. Both keys are always arrays, empty at
// the end of the folder, because the client hides an arrow on an empty list and prepares the
// rest of the run: a missing field and "nothing that way" must not look alike. `before` and
// `after` bound the run, and a folder is refused rather than answered empty — a folder is an
// entry point, not a page the arrows can turn to.
func TestFilesNeighborsAnswersTheRunAndRefusesAFolder(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	index.rows = []nodes.Row{
		filesReadRow("Album", true),
		filesReadRow("Album/A.txt", false),
		filesReadRow("Album/b.txt", false),
		filesReadRow("Album/Z.txt", false),
		filesReadRow("Album/E.txt", false),
	}
	index.rows[0].ID = mutationFolderID

	// Listing order is name bytes, so E < Z < b; two back from Z and nothing ahead.
	window := filesReadNeighbors(t, filesReadCall(t, s, f, s.handleFilesNeighbors, "path=%2FAlbum%2FZ.txt&before=2&after=0"))
	if len(window.Previous) != 2 || window.Previous[0].Path != "/Album/E.txt" || window.Previous[1].Path != "/Album/A.txt" {
		t.Fatalf("previous run: %+v", window.Previous)
	}
	if len(window.Next) != 0 {
		t.Fatalf("after=0 must ask for nothing ahead: %+v", window.Next)
	}
	if got := index.neighborLimits[0]; got.Before != 2 || got.After != 0 {
		t.Fatalf("index window=%+v", got)
	}

	// The default is one each way: enough for the arrows, which is what a caller that sends
	// no window is asking for.
	defaults := filesReadNeighbors(t, filesReadCall(t, s, f, s.handleFilesNeighbors, "path=%2FAlbum%2FZ.txt"))
	if len(defaults.Previous) != 1 || defaults.Previous[0].Path != "/Album/E.txt" || len(defaults.Next) != 1 || defaults.Next[0].Path != "/Album/b.txt" {
		t.Fatalf("default run: previous=%+v next=%+v", defaults.Previous, defaults.Next)
	}

	// The first file has nothing behind it, and that absence is an empty array on the wire.
	first := filesReadCall(t, s, f, s.handleFilesNeighbors, "path=%2FAlbum%2FA.txt&before=3&after=1")
	if body := first.Body.String(); !strings.Contains(body, `"previous":[]`) {
		t.Fatalf("an absent run must be an explicit empty array: %s", body)
	}
	if run := filesReadNeighbors(t, first); len(run.Next) != 1 || run.Next[0].Path != "/Album/E.txt" {
		t.Fatalf("first file next=%+v", run.Next)
	}

	err := s.handleFilesNeighbors(httptest.NewRecorder(), filesReadRequest("path=%2FAlbum"), f)
	filesReadProblem(t, err, 409, "not_a_file")
	for _, query := range []string{"path=%2FAlbum%2FZ.txt&before=99", "path=%2FAlbum%2FZ.txt&after=-1", "path=%2FAlbum%2FZ.txt&before=x"} {
		err := s.handleFilesNeighbors(httptest.NewRecorder(), filesReadRequest(query), f)
		filesReadProblem(t, err, 400, "invalid_request")
	}
}

func filesReadNeighbors(t *testing.T, w *httptest.ResponseRecorder) filesNeighborsResource {
	t.Helper()
	var result filesNeighborsResource
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
