package api

import (
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
