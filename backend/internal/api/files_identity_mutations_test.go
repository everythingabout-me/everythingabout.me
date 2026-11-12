package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nodes"
)

const (
	mutationRootID   = "10000000-0000-4000-8000-000000000001"
	mutationFolderID = "10000000-0000-4000-8000-000000000002"
	mutationFileID   = "10000000-0000-4000-8000-000000000003"
	mutationOtherID  = "10000000-0000-4000-8000-000000000004"
)

type mutationIdentityIndex struct {
	*filesReadTestIndex
	locations  map[string]string
	identities map[string][2]string
}

func (i *mutationIdentityIndex) FileResolveID(_ context.Context, owner, id string) (string, bool, error) {
	if owner != "owner-a" {
		return "", false, nil
	}
	p, ok := i.locations[id]
	return p, ok, nil
}
func (i *mutationIdentityIndex) FileIdentity(_ context.Context, owner, p string) (string, string, error) {
	if owner != "owner-a" {
		return "", "", nil
	}
	identity := i.identities[p]
	return identity[0], identity[1], nil
}
func mutationIdentityFixture() (*Server, *fileRequest, *mutationIdentityIndex) {
	s, f, _, base := filesReadFixture()
	base.rows = []nodes.Row{filesReadRow("Scope", true), filesReadRow("Scope/current.txt", false), filesReadRow("Elsewhere", true)}
	index := &mutationIdentityIndex{filesReadTestIndex: base,
		locations:  map[string]string{mutationRootID: "", mutationFolderID: "Scope", mutationFileID: "Scope/current.txt", mutationOtherID: "Elsewhere"},
		identities: map[string][2]string{"": {mutationRootID, ""}, "Scope": {mutationFolderID, mutationRootID}, "Scope/current.txt": {mutationFileID, mutationFolderID}, "Elsewhere": {mutationOtherID, mutationRootID}},
	}
	s.filesIndex = index
	s.cfg.Storage.CoordinatedWrites = true
	return s, f, index
}

func TestFilesIdentityMutationMoveCopyWireForms(t *testing.T) {
	s, f, _ := mutationIdentityFixture()
	for _, body := range []string{
		`{"from":"/Scope/current.txt","to":"/Scope/new.txt"}`,
		`{"from":{"path":"/Scope/current.txt"},"to":{"path":"/Scope/new.txt"}}`,
		`{"from":{"id":"` + mutationFileID + `"},"to":{"parent":{"id":"` + mutationFolderID + `"},"name":"new.txt"}}`,
		`{"from":{"id":"` + mutationFileID + `"},"to":{"parent":{"path":"/Scope"},"name":"new.txt"}}`,
	} {
		for _, handler := range []fileHandler{s.handleFilesMove, s.handleFilesCopy} {
			err := handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/files/move", strings.NewReader(body)), f)
			// Reaching the source revision check proves both forms resolved on the wire.
			p := filesProblem(err)
			if p.Status != 428 || p.Code != "precondition_required" {
				t.Fatalf("body %s: %+v", body, p)
			}
		}
	}
	for _, body := range []string{
		`{"from":{"id":"` + mutationFileID + `","path":"/Scope/current.txt"},"to":"/Scope/new.txt"}`,
		`{"from":{"id":"` + mutationFileID + `","id":"` + mutationOtherID + `"},"to":"/Scope/new.txt"}`,
		`{"from":{"id":"` + mutationFileID + `","unknown":true},"to":"/Scope/new.txt"}`,
		`{"from":{"id":"` + mutationFileID + `"},"to":{"id":"` + mutationOtherID + `"}}`,
		`{"from":"/Scope/current.txt","to":{"parent":{"id":"` + mutationFolderID + `"},"name":"../escape"}}`,
		`{"from":"/Scope/current.txt","to":{"parent":{"id":"` + mutationFolderID + `"},"name":"a","name":"b"}}`,
	} {
		err := s.handleFilesMove(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/files/move", strings.NewReader(body)), f)
		if p := filesProblem(err); p.Status != 400 {
			t.Fatalf("invalid form accepted: %s: %+v", body, p)
		}
	}
}

func TestFilesIdentityMutationMkdirDestinations(t *testing.T) {
	s, f, _ := mutationIdentityFixture()
	for _, body := range []string{
		`{"kind":"folder","path":"/Scope/new"}`,
		`{"kind":"folder","destination":{"path":"/Scope/new"}}`,
		`{"kind":"folder","destination":{"parent":{"id":"` + mutationFolderID + `"},"name":"new"}}`,
	} {
		var req filesMkdirRequest
		if err := strictFilesJSON(httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader(body)), &req); err != nil {
			t.Fatal(err)
		}
		dest, err := filesCreateDestination(req.Destination, req.Path)
		if err != nil {
			t.Fatal(err)
		}
		p, err := s.filesResolveDestination(context.Background(), f, dest)
		if err != nil || p != "/Scope/new" {
			t.Fatalf("body %s: %s %v", body, p, err)
		}
	}
	for _, body := range []string{
		`{"kind":"folder","path":"/Scope/new","destination":{"path":"/Scope/new"}}`,
		`{"kind":"folder","path":"","destination":{"path":"/Scope/new"}}`,
		`{"kind":"folder","path":null,"destination":{"path":"/Scope/new"}}`,
		`{"kind":"folder","destination":{"parent":{"id":"` + mutationFolderID + `"},"name":"new"},"parents":true}`,
	} {
		err := s.handleFilesMkdir(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/files", strings.NewReader(body)), f)
		if p := filesProblem(err); p.Status != 400 {
			t.Fatalf("invalid mkdir accepted: %s: %+v", body, p)
		}
	}
}

func TestFilesIdentityMutationUploadSelection(t *testing.T) {
	s, f, _ := mutationIdentityFixture()
	for _, tc := range []struct {
		body string
		path fc.Path
		mode string
	}{
		{`{"path":"/Scope/new.txt"}`, "/Scope/new.txt", "path"},
		{`{"destination":{"path":"/Scope/new.txt"}}`, "/Scope/new.txt", "path"},
		{`{"destination":{"parent":{"id":"` + mutationFolderID + `"},"name":" +%2F?# "}}`, "/Scope/ +%2F?# ", "parentId"},
		{`{"destination":{"parent":{"path":"/Scope"},"name":"new.txt"},"condition":{"mode":"create"}}`, "/Scope/new.txt", "parentPath"},
		{`{"target":{"id":"` + mutationFileID + `"},"condition":{"mode":"replace","contentRevision":"cr_test"}}`, "/Scope/current.txt", "id"},
		{`{"target":{"path":"/Scope/current.txt"},"condition":{"mode":"replace"}}`, "/Scope/current.txt", "path"},
		{`{"path":"/Scope/current.txt","condition":{"mode":"replace"}}`, "/Scope/current.txt", "path"},
		// Overwrite selects its destination like a replacement, but it does not require the
		// entry to exist: a re-import also carries files that were deleted since the last one.
		{`{"path":"/Scope/new.txt","condition":{"mode":"overwrite"}}`, "/Scope/new.txt", "path"},
		{`{"path":"/Scope/current.txt","condition":{"mode":"overwrite"}}`, "/Scope/current.txt", "path"},
		{`{"target":{"path":"/Scope/current.txt"},"condition":{"mode":"overwrite"}}`, "/Scope/current.txt", "path"},
	} {
		var req filesCreateUploadRequest
		if err := strictFilesJSON(httptest.NewRequest(http.MethodPost, "/api/files/uploads", strings.NewReader(tc.body)), &req); err != nil {
			t.Fatal(err)
		}
		p, mode, _, err := s.filesResolveUploadTarget(context.Background(), f, req)
		if err != nil || p != tc.path || mode != tc.mode {
			t.Fatalf("body %s: %s %s %v", tc.body, p, mode, err)
		}
	}
	for _, body := range []string{
		`{}`,
		`{"target":{"id":"` + mutationFileID + `"}}`,
		`{"path":"/Scope/new.txt","destination":{"path":"/Scope/new.txt"}}`,
		`{"path":null,"destination":{"path":"/Scope/new.txt"}}`,
		`{"target":null,"destination":{"path":"/Scope/new.txt"}}`,
		`{"target":{"path":"/Scope/current.txt"},"path":"/Scope/current.txt","condition":{"mode":"replace"}}`,
		`{"destination":{"path":"/Scope/current.txt"},"condition":{"mode":"replace"}}`,
		`{"destination":{"path":"/Scope/new.txt"},"condition":{"mode":"overwrite"}}`,
		`{"destination":{"path":"/Scope/new.txt"},"target":{"id":"` + mutationFileID + `"}}`,
		`{"destination":{"parent":{"id":"` + mutationFolderID + `","path":"/Scope"},"name":"new.txt"}}`,
		`{"destination":{"path":"/Scope/a"},"destination":{"path":"/Scope/b"}}`,
	} {
		var req filesCreateUploadRequest
		err := strictFilesJSON(httptest.NewRequest(http.MethodPost, "/api/files/uploads", strings.NewReader(body)), &req)
		if err == nil {
			_, _, _, err = s.filesResolveUploadTarget(context.Background(), f, req)
		}
		if err == nil || filesProblem(err).Status != 400 {
			t.Fatalf("invalid upload selection accepted: %s: %v", body, err)
		}
	}
}

func TestFilesIdentityMutationPutQueryForms(t *testing.T) {
	s, f, _ := mutationIdentityFixture()
	for _, tc := range []struct {
		query, match string
		status       int
		path         fc.Path
	}{
		{"id=" + mutationFileID, "", 428, ""},
		{"id=" + mutationFileID, `"cr_test"`, 0, "/Scope/current.txt"},
		{"parentId=" + mutationFolderID + "&name=new.txt", "", 0, "/Scope/new.txt"},
		{"parentPath=%2FScope&name=%252F%2B%3F%23", "", 0, "/Scope/%2F+?#"},
		{"parentId=" + mutationFolderID + "&name=new.txt", `"cr_test"`, 400, ""},
		{"id=" + mutationFileID + "&path=%2FScope%2Fcurrent.txt", `"cr_test"`, 400, ""},
		{"parentId=" + mutationFolderID + "&parentPath=%2FScope&name=new.txt", "", 400, ""},
		{"parentId=" + mutationFolderID + "&name=one&name=two", "", 400, ""},
	} {
		r := httptest.NewRequest(http.MethodPut, "/api/files/content?"+tc.query, nil)
		if tc.match != "" {
			r.Header.Set("If-Match", tc.match)
		}
		p, err := s.filesResolvePutQuery(context.Background(), f, r)
		if tc.status == 0 {
			if err != nil || p != tc.path {
				t.Fatalf("query %s: %s %v", tc.query, p, err)
			}
		} else if err == nil || filesProblem(err).Status != tc.status {
			t.Fatalf("query %s: %v", tc.query, err)
		}
	}
}

func TestFilesIdentityMutationScopeAndAdmissionRace(t *testing.T) {
	s, f, index := mutationIdentityFixture()
	f.Scope = "/Scope"
	f.Principal.Token = &auth.Token{Prefix: "Scope", CanWrite: true}
	var req filesCreateUploadRequest
	body := `{"destination":{"parent":{"id":"` + mutationFolderID + `"},"name":"new.txt"}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	p, mode, selectedID, err := s.filesResolveUploadTarget(context.Background(), f, req)
	if err != nil || f.RequireWrite(p) != nil {
		t.Fatalf("write-only parent lookup failed: %v", err)
	}
	if err = f.RequireRead(p); err == nil {
		t.Fatal("fixture unexpectedly allows reading")
	}
	_, parentID, err := s.filesBindMutationSelection(context.Background(), f, p, mode, selectedID)
	if err != nil || parentID != mutationFolderID {
		t.Fatalf("parent identity not captured: %s %v", parentID, err)
	}
	index.identities["Scope"] = [2]string{mutationOtherID, mutationRootID}
	if _, _, err = s.filesBindMutationSelection(context.Background(), f, p, mode, selectedID); filesProblem(err).Status != 412 {
		t.Fatalf("recreated parent admitted: %v", err)
	}
	index.locations[mutationFileID] = "Elsewhere/moved.txt"
	for _, handler := range []fileHandler{s.handleFilesDelete, s.handleFilesPut} {
		r := httptest.NewRequest(http.MethodDelete, "/api/files?id="+mutationFileID, nil)
		r.Header.Set("If-Match", `"cr_test"`)
		err = handler(httptest.NewRecorder(), r, f)
		if problem := filesProblem(err); problem.Status != 404 {
			t.Fatalf("ID outside current scope accepted: %+v", problem)
		}
	}
	for _, id := range []string{mutationOtherID, "10000000-0000-4000-8000-999999999999"} {
		target := fc.EntrySelector{ID: id}
		if _, err = s.filesResolveSelector(context.Background(), f, target); filesProblem(err).Status != 404 {
			t.Fatalf("concealed or unknown ID accepted: %s %v", id, err)
		}
	}
}
