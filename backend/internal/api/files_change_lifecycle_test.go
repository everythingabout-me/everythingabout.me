package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
)

// This suite obtains every fact through the production HTTP mutation pipeline.
// The disposable fixture initializes only account/root/journal infrastructure;
// it never inserts a change or edits the index to manufacture an expected feed.
type filesLifecycleHTTP struct {
	t       *testing.T
	fixture *fileWorkerFixture
	handler http.Handler
	bearer  string
	grant   *auth.Token
	owner   string
	cookie  string
}

func newFilesLifecycleHTTP(t *testing.T) *filesLifecycleHTTP {
	t.Helper()
	fixture := newFileWorkerFixture(t)
	root, _ := fixture.owner(t)
	plain, grant, err := fixture.auth.CreateToken(fixture.ctx, auth.NewTokenInput{UserID: root.OwnerID, Name: "Lifecycle integration", Kind: auth.KindApp, CanRead: true, CanWrite: true, CanEvents: true, CanChanges: true, CanSubscribe: true})
	if err != nil {
		t.Fatal(err)
	}
	cookie, _, err := fixture.auth.CreateSession(fixture.ctx, root.OwnerID, "Lifecycle owner", "")
	if err != nil {
		t.Fatal(err)
	}
	server := fixture.server()
	workerCtx, stopWorkers := context.WithCancel(fixture.ctx)
	server.workers().ctx = workerCtx
	t.Cleanup(stopWorkers)
	return &filesLifecycleHTTP{t: t, fixture: fixture, handler: server.Handler(), bearer: plain, grant: grant, owner: root.OwnerID, cookie: cookie}
}

func (h *filesLifecycleHTTP) request(method, target, credential string, body []byte, headers http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, "https://files.example"+target, bytes.NewReader(body)).WithContext(h.fixture.ctx)
	r.Header = headers.Clone()
	if r.Header == nil {
		r.Header = make(http.Header)
	}
	if credential == "owner-cookie" {
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: h.cookie})
	} else if credential != "" {
		r.Header.Set("Authorization", "Bearer "+credential)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func (h *filesLifecycleHTTP) jsonRequest(method, target, credential string, body any, headers http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		h.t.Fatal(err)
	}
	return h.request(method, target, credential, encoded, headers)
}

func lifecycleStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP status=%d want=%d body=%s", w.Code, status, w.Body.String())
	}
}

func lifecycleDecode[T any](t *testing.T, w *httptest.ResponseRecorder, status int) T {
	t.Helper()
	lifecycleStatus(t, w, status)
	var decoded T
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON resource: %v body=%s", err, w.Body.String())
	}
	return decoded
}

func lifecycleTarget(route, path string) string {
	return fc.Base + route + "?" + url.Values{"path": {path}}.Encode()
}

func (h *filesLifecycleHTTP) metadata(path string) fc.Metadata {
	h.t.Helper()
	return lifecycleDecode[fc.Metadata](h.t, h.request("GET", lifecycleTarget("", path), h.bearer, nil, nil), 200)
}

func (h *filesLifecycleHTTP) mkdir(path string, status int) fc.Metadata {
	h.t.Helper()
	return lifecycleDecode[fc.Metadata](h.t, h.jsonRequest("POST", fc.Base, h.bearer, map[string]any{"kind": "folder", "path": path}, nil), status)
}

func (h *filesLifecycleHTTP) move(from, to string) fc.Metadata {
	h.t.Helper()
	selected := h.metadata(from)
	return lifecycleDecode[fc.Metadata](h.t, h.jsonRequest("POST", fc.Base+"/move", h.bearer, map[string]any{"from": from, "to": to, "ifRevision": selected.Revision}, nil), 200)
}

func (h *filesLifecycleHTTP) changes(cursor, credential string) filesChangesPage {
	h.t.Helper()
	page := lifecycleDecode[filesChangesPage](h.t, h.request("GET", fc.Base+"/changes?"+url.Values{"cursor": {cursor}, "limit": {"1000"}}.Encode(), credential, nil, nil), 200)
	if page.HasMore || page.NextCursor == "" || page.Items == nil {
		h.t.Fatalf("small lifecycle feed did not return a complete bounded page: %+v", page)
	}
	return page
}

func lifecycleMarkerChanged(t *testing.T, before, after fc.Metadata) {
	t.Helper()
	if after.ChangeToken == "" || after.LastChangedAt == nil || after.ChangeToken == before.ChangeToken || (before.LastChangedAt != nil && !after.LastChangedAt.After(*before.LastChangedAt)) {
		t.Errorf("recursive marker did not advance: before=%+v after=%+v", before, after)
	}
}

func lifecycleAffectedFolders(t *testing.T, event filesChangeItem, scope fc.Path, maximum int, expected ...fc.Path) {
	t.Helper()
	if len(event.AffectedFolders) > maximum {
		t.Errorf("ancestor metadata expanded beyond path depth: %+v", event.AffectedFolders)
	}
	seen := map[fc.Path]bool{}
	for _, folder := range event.AffectedFolders {
		if seen[folder.Path] || folder.Kind != "folder" || !fc.WithinScope(scope, folder.Path, nil) || folder.ChangeToken == "" || folder.LastChangedAt == nil || !fc.ValidEntryID(folder.ID) {
			t.Errorf("invalid visible affected folder: %+v", folder)
		}
		seen[folder.Path] = true
		if folder.ParentPath != "" && !fc.WithinScope(scope, folder.ParentPath, nil) {
			t.Errorf("concealed affected-folder parent disclosed: %+v", folder)
		}
	}
	for _, path := range expected {
		if !seen[path] {
			t.Errorf("affected folder %q missing from event %+v", path, event)
		}
	}
}

func TestFilesChangeLifecycleHTTPPostgres(t *testing.T) {
	h := newFilesLifecycleHTTP(t)
	rootBefore := h.metadata("/")
	filter := map[string]any{"target": map[string]string{"path": "/"}, "kind": "folder", "recursive": true}
	subscriptionBody := map[string]any{"target": filter["target"], "kind": "folder", "recursive": true, "delivery": map[string]string{"type": "sse"}}
	subscription := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", h.bearer, subscriptionBody, nil), 201)
	snapshot := lifecycleDecode[filesChangeSnapshot](t, h.jsonRequest("POST", fc.Base+"/change-snapshots", h.bearer, filter, nil), 201)
	baseline := lifecycleDecode[snapshotTestPage](t, h.request("GET", snapshot.EntriesURL, h.bearer, nil, nil), 200)
	if snapshot.ChangesCursor == "" || snapshot.TotalEntries != 1 || len(baseline.Items) != 1 || baseline.Items[0].ID != rootBefore.ID || subscription.ChangesCursor == "" {
		t.Fatalf("root baseline incomplete: subscription=%+v snapshot=%+v entries=%+v", subscription, snapshot, baseline.Items)
	}

	oldRead, _, err := h.fixture.auth.CreateToken(h.fixture.ctx, auth.NewTokenInput{UserID: h.owner, Name: "Old read grant", Kind: auth.KindApp, CanRead: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []*httptest.ResponseRecorder{
		h.request("GET", fc.Base+"/changes?cursor="+url.QueryEscape(subscription.ChangesCursor), oldRead, nil, nil),
		h.jsonRequest("POST", fc.Base+"/subscriptions", oldRead, subscriptionBody, nil),
		h.jsonRequest("POST", fc.Base+"/change-snapshots", oldRead, filter, nil),
	} {
		problem := lifecycleDecode[fc.Problem](t, request, 403)
		if problem.Code != "forbidden" {
			t.Fatalf("old read grant unexpectedly acquired change permissions: %+v", problem)
		}
	}

	scope := h.mkdir("/Scope", 201)
	tree := h.mkdir("/Scope/Tree", 201)
	sibling := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/Scope/sibling.txt"), h.bearer, []byte("sibling-stays"), nil), 201)
	createHeaders := http.Header{"Idempotency-Key": {"lifecycle-create"}}
	createdResponse := h.request("PUT", lifecycleTarget("/content", "/Scope/Tree/note.txt"), h.bearer, []byte("old-data"), createHeaders)
	created := lifecycleDecode[fc.Metadata](t, createdResponse, 201)
	createdRequestID := createdResponse.Header().Get("X-Request-Id")
	createdOperation := strings.TrimPrefix(createdResponse.Header().Get("Location"), fc.Base+"/operations/")
	if !fc.ValidEntryID(created.ID) || created.ContentRevision == nil || created.ChangeToken == "" || created.LastChangedAt == nil {
		t.Fatalf("created representation lacks durable identity/marker: %+v", created)
	}
	// Reconstruct API services before consuming the checkpoint: neither the feed
	// nor its baseline resources may depend on the process that admitted them.
	restarted := h.fixture.server()
	restarted.workers().ctx = h.fixture.ctx
	h.handler = restarted.Handler()
	page := h.changes(subscription.ChangesCursor, h.bearer)
	if len(page.Items) != 4 {
		t.Fatalf("actual mkdir/PUT pipeline emitted %d facts, want four: %+v", len(page.Items), page.Items)
	}
	for index, metadata := range []fc.Metadata{scope, tree, sibling, created} {
		event := page.Items[index]
		if event.Type != "upsert" || event.Before != nil || event.After == nil || event.After.ID != metadata.ID || event.EntryID == nil || *event.EntryID != metadata.ID || event.After.ChangeToken != event.ID {
			t.Errorf("created identity does not match durable fact: metadata=%+v event=%+v", metadata, event)
		}
	}
	snapshotPage := h.changes(snapshot.ChangesCursor, h.bearer)
	if len(snapshotPage.Items) != len(page.Items) {
		t.Fatal("snapshot checkpoint and subscription missed different committed facts")
	}
	for i := range page.Items {
		if snapshotPage.Items[i].ID != page.Items[i].ID {
			t.Fatal("baseline cursors returned different durable event identities")
		}
	}
	rootAfterCreate := h.metadata("/")
	lifecycleMarkerChanged(t, rootBefore, rootAfterCreate)

	// Neither a verified existing mkdir nor an idempotent publication replay is
	// a new native/index transition, even though their HTTP attempts are audited.
	h.mkdir("/Scope/Tree", 200)
	replayed := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/Scope/Tree/note.txt"), h.bearer, []byte("old-data"), createHeaders), 201)
	if replayed.ID != created.ID || replayed.ChangeToken != created.ChangeToken {
		t.Errorf("terminal replay changed its saved identity/marker: %+v %+v", created, replayed)
	}
	empty := h.changes(page.NextCursor, h.bearer)
	if len(empty.Items) != 0 || h.metadata("/").ChangeToken != rootAfterCreate.ChangeToken {
		t.Fatalf("no-op/replay emitted facts or advanced markers: %+v", empty.Items)
	}

	replaced := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/Scope/Tree/note.txt"), h.bearer, []byte("new-data"), http.Header{"If-Match": {`"` + *created.ContentRevision + `"`}}), 200)
	if replaced.ID != created.ID || replaced.ContentRevision == nil || *replaced.ContentRevision == *created.ContentRevision {
		t.Fatalf("replacement lost ID or content generation: %+v %+v", created, replaced)
	}
	lifecycleMarkerChanged(t, created, replaced)
	replacePage := h.changes(empty.NextCursor, h.bearer)
	if len(replacePage.Items) != 1 || replacePage.Items[0].Before == nil || replacePage.Items[0].After == nil || replacePage.Items[0].Before.ID != created.ID || replacePage.Items[0].After.ID != created.ID {
		t.Fatalf("replacement did not publish one durable identity transition: %+v", replacePage.Items)
	}
	lifecycleAffectedFolders(t, replacePage.Items[0], "/", 3, "/", "/Scope", "/Scope/Tree")

	beforeDirectory, err := os.Stat(filepath.Join(h.fixture.cfg.Storage.Local.ContentRoot, h.owner, "Scope/Tree"))
	if err != nil {
		t.Fatal(err)
	}
	beforeMove := h.metadata("/Scope/Tree/note.txt")
	moved := h.move("/Scope/Tree", "/Scope/Twin") // equal-length paths exercise native rename, not string length heuristics
	if moved.ID != tree.ID {
		t.Fatal("native folder move allocated a new identity")
	}
	afterDirectory, err := os.Stat(filepath.Join(h.fixture.cfg.Storage.Local.ContentRoot, h.owner, "Scope/Twin"))
	if err != nil || !os.SameFile(beforeDirectory, afterDirectory) {
		t.Fatalf("admitted local move did not preserve the native directory: %v", err)
	}
	afterMove := h.metadata("/Scope/Twin/note.txt")
	if afterMove.ID != created.ID || afterMove.ContentRevision == nil || *afterMove.ContentRevision != *beforeMove.ContentRevision {
		t.Fatal("ancestor rename changed descendant identity or bytes generation")
	}
	lifecycleMarkerChanged(t, beforeMove, afterMove)
	movePage := h.changes(replacePage.NextCursor, h.bearer)
	if len(movePage.Items) != 1 || movePage.Items[0].Type != "moved" || movePage.Items[0].Coverage != "subtree" || movePage.Items[0].Before == nil || movePage.Items[0].After == nil || movePage.Items[0].Before.Path != "/Scope/Tree" || movePage.Items[0].After.Path != "/Scope/Twin" {
		t.Fatalf("native subtree move expanded or lost its endpoints: %+v", movePage.Items)
	}
	lifecycleAffectedFolders(t, movePage.Items[0], "/", 3, "/", "/Scope")
	unchangedSibling := h.metadata("/Scope/sibling.txt")
	if unchangedSibling.ChangeToken != sibling.ChangeToken || unchangedSibling.LastChangedAt == nil || sibling.LastChangedAt == nil || !unchangedSibling.LastChangedAt.Equal(*sibling.LastChangedAt) {
		t.Errorf("unrelated sibling file marker changed: before=%+v after=%+v", sibling, unchangedSibling)
	}

	copyResponse := h.jsonRequest("POST", fc.Base+"/copy", h.bearer, map[string]any{"from": "/Scope/Twin/note.txt", "to": "/Scope/copied.txt", "ifSourceRevision": afterMove.Revision}, nil)
	copied := lifecycleDecode[fc.Metadata](t, copyResponse, 201)
	if copied.ID == created.ID || !fc.ValidEntryID(copied.ID) {
		t.Fatal("copy did not allocate a fresh service ID")
	}
	lifecycleStatus(t, h.request("DELETE", lifecycleTarget("", "/Scope/copied.txt"), h.bearer, nil, nil), 204)
	copyPage := h.changes(movePage.NextCursor, h.bearer)
	if len(copyPage.Items) != 2 || copyPage.Items[0].After == nil || copyPage.Items[0].After.ID != copied.ID || copyPage.Items[0].Before != nil || copyPage.Items[1].Type != "remove" || copyPage.Items[1].Before == nil || copyPage.Items[1].Before.ID != copied.ID || copyPage.Items[1].After != nil {
		t.Fatalf("copy/delete lost fresh-ID or retained tombstone facts: %+v", copyPage.Items)
	}
	lifecycleStatus(t, h.request("GET", fc.Base+"?id="+copied.ID, h.bearer, nil, nil), 404)

	// An application watches only its authorized path. Both native moves are
	// performed by the broad grant, so projection has to redact the other side.
	private := h.mkdir("/Private", 201)
	incoming := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/Private/incoming.txt"), h.bearer, []byte("private-native-bytes"), nil), 201)
	scopedToken, _, err := h.fixture.auth.CreateToken(h.fixture.ctx, auth.NewTokenInput{UserID: h.owner, Name: "Scoped lifecycle observer", Kind: auth.KindApp, Prefix: "Scope", CanRead: true, CanChanges: true, CanSubscribe: true})
	if err != nil {
		t.Fatal(err)
	}
	scopedSubscription := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", scopedToken, map[string]any{"target": map[string]string{"path": "/Scope"}, "kind": "folder", "recursive": true, "delivery": map[string]string{"type": "sse"}}, nil), 201)
	h.move("/Private/incoming.txt", "/Scope/incoming.txt")
	h.move("/Scope/Twin/note.txt", "/Private/outgoing.txt")
	scopedPage := h.changes(scopedSubscription.ChangesCursor, scopedToken)
	if len(scopedPage.Items) != 2 || scopedPage.Items[0].Type != "upsert" || scopedPage.Items[0].Before != nil || scopedPage.Items[0].After == nil || scopedPage.Items[0].After.ID != incoming.ID || scopedPage.Items[1].Type != "remove" || scopedPage.Items[1].After != nil || scopedPage.Items[1].Before == nil || scopedPage.Items[1].Before.ID != created.ID {
		t.Fatalf("cross-scope native moves did not project one authorized side: %+v", scopedPage.Items)
	}
	wire, _ := json.Marshal(scopedPage)
	for _, secret := range []string{"/Private", private.ID, "private-native-bytes", h.bearer, scopedToken} {
		if bytes.Contains(wire, []byte(secret)) {
			t.Errorf("scoped feed disclosed concealed information %q", secret)
		}
	}
	for _, item := range scopedPage.Items {
		lifecycleAffectedFolders(t, item, "/Scope", 2, "/Scope")
	}

	// Materialized metadata stays at its original HTTP baseline after all writes.
	immutable := lifecycleDecode[snapshotTestPage](t, h.request("GET", snapshot.EntriesURL, h.bearer, nil, nil), 200)
	beforeJSON, _ := json.Marshal(baseline.Items)
	afterJSON, _ := json.Marshal(immutable.Items)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Errorf("HTTP snapshot changed after native lifecycle: before=%s after=%s", beforeJSON, afterJSON)
	}
	lifecycleMarkerChanged(t, scope, h.metadata("/Scope"))
	lifecycleMarkerChanged(t, rootAfterCreate, h.metadata("/"))
	lifecycleDecode[filesSubscription](t, h.request("GET", fc.Base+"/subscriptions/"+subscription.ID, h.bearer, nil, nil), 200)
	content := h.request("GET", lifecycleTarget("/content", "/Private/outgoing.txt"), h.bearer, nil, nil)
	lifecycleStatus(t, content, 200)
	if content.Body.String() != "new-data" {
		t.Fatal("native content was lost across replace and both moves")
	}

	var audits []filechanges.Audit
	auditURL := "/api/audit?limit=100"
	for pages := 0; pages < 10; pages++ {
		auditPage := lifecycleDecode[fc.Collection[filechanges.Audit]](t, h.request("GET", auditURL, "owner-cookie", nil, nil), 200)
		audits = append(audits, auditPage.Items...)
		if auditPage.NextCursor == nil {
			break
		}
		auditURL = "/api/audit?" + url.Values{"limit": {"100"}, "cursor": {*auditPage.NextCursor}}.Encode()
	}
	confirmedCreate, completedRead, deniedGrant := false, false, false
	for _, audit := range audits {
		if audit.Action == "file.created" && audit.Outcome == "confirmed" && audit.EntryID != nil && *audit.EntryID == created.ID {
			confirmedCreate = audit.Actor.Kind == "app" && audit.Actor.PrincipalID == "token:"+h.grant.ID && audit.Actor.GrantID == h.grant.ID && audit.Actor.Label == h.grant.Name && audit.Actor.Scope == "/" && audit.Actor.CanRead && audit.Actor.CanWrite && audit.Actor.CanEvents && audit.Actor.CanChanges && audit.Actor.CanSubscribe && audit.Actor.PolicyGeneration == "1" && audit.OperationID == createdOperation && audit.RequestID == createdRequestID && audit.Allowed != nil && *audit.Allowed
		}
		if audit.Action == "read" && audit.Outcome == "completed" && audit.RequestID == content.Header().Get("X-Request-Id") {
			completedRead = audit.Status == 200 && audit.BytesWritten != nil && *audit.BytesWritten == int64(len("new-data")) && audit.Allowed != nil && *audit.Allowed
		}
		if audit.Action == "changes" && audit.Status == 403 && audit.Outcome == "denied" {
			deniedGrant = audit.Allowed != nil && !*audit.Allowed
		}
	}
	if !confirmedCreate || !completedRead || !deniedGrant {
		t.Errorf("owner audit omitted actual mutation/read/denied facts: create=%t read=%t denied=%t records=%d", confirmedCreate, completedRead, deniedGrant, len(audits))
	}
	auditWire, _ := json.Marshal(audits)
	for _, secret := range []string{h.bearer, oldRead, scopedToken, "old-data", "new-data", "private-native-bytes"} {
		if bytes.Contains(auditWire, []byte(secret)) {
			t.Errorf("owner audit persisted content or credential %q", secret)
		}
	}
	lifecycleStatus(t, h.request("GET", "/api/audit", h.bearer, nil, nil), 403)
	renewed := lifecycleDecode[filesSubscription](t, h.request("POST", fc.Base+"/subscriptions/"+subscription.ID+"/renew", h.bearer, nil, nil), 200)
	if renewed.ID != subscription.ID || !renewed.ExpiresAt.After(subscription.ExpiresAt) || renewed.ChangesCursor == "" {
		t.Errorf("subscription renewal lost durable identity or lifetime: %+v", renewed)
	}
	lifecycleStatus(t, h.request("DELETE", fc.Base+"/subscriptions/"+subscription.ID, h.bearer, nil, nil), 204)
	lifecycleStatus(t, h.request("GET", fc.Base+"/subscriptions/"+subscription.ID, h.bearer, nil, nil), 404)
	lifecycleStatus(t, h.request("DELETE", snapshot.EntriesURL, h.bearer, nil, nil), 204)
	lifecycleStatus(t, h.request("GET", snapshot.EntriesURL, h.bearer, nil, nil), 404)
}

func TestFilesChangeLifecycleStaleFolderRecreatedAsFileHTTPPostgres(t *testing.T) {
	h := newFilesLifecycleHTTP(t)
	folder := h.mkdir("/Recreated", 201)
	child := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/Recreated/child.txt"), h.bearer, []byte("old-child"), nil), 201)
	rootBefore := h.metadata("/")
	subscription := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", h.bearer, map[string]any{"target": map[string]string{"path": "/"}, "kind": "folder", "recursive": true, "delivery": map[string]string{"type": "sse"}}, nil), 201)
	oldIDWatch := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", h.bearer, map[string]any{"target": map[string]string{"id": folder.ID}, "kind": "folder", "recursive": true, "delivery": map[string]string{"type": "sse"}}, nil), 201)
	childIDWatch := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", h.bearer, map[string]any{"target": map[string]string{"id": child.ID}, "kind": "file", "delivery": map[string]string{"type": "sse"}}, nil), 201)

	// Simulate the supported external-writer case without changing the index or
	// manufacturing journal facts. Remove only these two known temporary paths.
	ownerRoot, err := filepath.Abs(filepath.Join(h.fixture.cfg.Storage.Local.ContentRoot, h.owner))
	if err != nil {
		t.Fatal(err)
	}
	folderPath := filepath.Join(ownerRoot, "Recreated")
	if relative, err := filepath.Rel(ownerRoot, folderPath); err != nil || relative != "Recreated" {
		t.Fatalf("unexpected disposable native target: %q %v", relative, err)
	}
	if err := os.Remove(filepath.Join(folderPath, "child.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(folderPath); err != nil {
		t.Fatal(err)
	}
	// Browsing is indexed and deliberately still returns the former folder.
	if stale := h.metadata("/Recreated"); stale.ID != folder.ID || stale.Kind != "folder" {
		t.Fatalf("fixture did not retain stale indexed folder: %+v", stale)
	}
	created := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/Recreated"), h.bearer, []byte("fresh-file"), nil), 201)
	if created.ID == folder.ID || created.ID == child.ID || created.Kind != "file" || !fc.ValidEntryID(created.ID) {
		t.Fatalf("recreated file inherited retired subtree identity: %+v", created)
	}
	for _, retired := range []string{folder.ID, child.ID} {
		lifecycleStatus(t, h.request("GET", fc.Base+"?id="+retired, h.bearer, nil, nil), 404)
	}
	lifecycleStatus(t, h.request("GET", lifecycleTarget("", "/Recreated/child.txt"), h.bearer, nil, nil), 404)
	lifecycleMarkerChanged(t, rootBefore, h.metadata("/"))
	page := h.changes(subscription.ChangesCursor, h.bearer)
	if len(page.Items) != 2 {
		t.Errorf("stale subtree retirement expanded beyond two root facts: %+v", page.Items)
	}
	retiredSubtree, freshFile := false, false
	for _, event := range page.Items {
		if event.Type == "remove" && event.Coverage == "subtree" && event.Before != nil && event.Before.ID == folder.ID && event.After == nil {
			retiredSubtree = true
		}
		if event.Type == "upsert" && event.Before == nil && event.After != nil && event.After.ID == created.ID && event.After.Kind == "file" {
			freshFile = true
		}
		lifecycleAffectedFolders(t, event, "/", 1, "/")
	}
	if !retiredSubtree || !freshFile {
		t.Errorf("recreated file omitted old subtree retirement or fresh identity: retired=%t created=%t events=%+v", retiredSubtree, freshFile, page.Items)
	}
	watchPage := h.changes(oldIDWatch.ChangesCursor, h.bearer)
	watchEnded := false
	for _, event := range watchPage.Items {
		if event.After != nil && event.After.ID == created.ID {
			t.Error("old ID watch followed recreated path occupant")
		}
		if event.Type == "watch_ended" && event.Reason == "deleted" {
			watchEnded = true
		}
	}
	if !watchEnded {
		t.Errorf("retired folder ID watch did not terminate: %+v", watchPage.Items)
	}
	childPage := h.changes(childIDWatch.ChangesCursor, h.bearer)
	childRemoved, childEnded := false, false
	for _, event := range childPage.Items {
		if event.Type == "remove" && event.Path == "/Recreated/child.txt" && event.Coverage == "entry" && event.EntryID != nil && *event.EntryID == child.ID {
			childRemoved = true
		}
		if event.Type == "watch_ended" && event.Reason == "deleted" {
			childEnded = true
		}
		if event.After != nil || (event.EntryID != nil && *event.EntryID != child.ID) {
			t.Errorf("retired descendant watch exposed a different occupant: %+v", event)
		}
	}
	if !childRemoved || !childEnded {
		t.Errorf("retired descendant ID watch omitted targeted removal/deletion: %+v", childPage.Items)
	}
	body := h.request("GET", lifecycleTarget("/content", "/Recreated"), h.bearer, nil, nil)
	lifecycleStatus(t, body, 200)
	if body.Body.String() != "fresh-file" {
		t.Fatal("recreated native file content unavailable")
	}
}

func TestFilesChangeLifecycleDeleteNativeEmptyFolderWithStaleChildHTTPPostgres(t *testing.T) {
	h := newFilesLifecycleHTTP(t)
	folder := h.mkdir("/EmptyNative", 201)
	child := lifecycleDecode[fc.Metadata](t, h.request("PUT", lifecycleTarget("/content", "/EmptyNative/stale.txt"), h.bearer, []byte("externally-removed"), nil), 201)
	rootBefore := h.metadata("/")
	rootWatch := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", h.bearer, map[string]any{"target": map[string]string{"path": "/"}, "kind": "folder", "recursive": true, "delivery": map[string]string{"type": "sse"}}, nil), 201)
	childWatch := lifecycleDecode[filesSubscription](t, h.jsonRequest("POST", fc.Base+"/subscriptions", h.bearer, map[string]any{"target": map[string]string{"id": child.ID}, "kind": "file", "delivery": map[string]string{"type": "sse"}}, nil), 201)

	ownerRoot, err := filepath.Abs(filepath.Join(h.fixture.cfg.Storage.Local.ContentRoot, h.owner))
	if err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(ownerRoot, "EmptyNative", "stale.txt")
	if relative, err := filepath.Rel(ownerRoot, childPath); err != nil || relative != filepath.Join("EmptyNative", "stale.txt") {
		t.Fatalf("unexpected disposable native target: %q %v", relative, err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatal(err)
	}
	if stale := h.metadata("/EmptyNative/stale.txt"); stale.ID != child.ID {
		t.Fatalf("external native removal unexpectedly edited the index: %+v", stale)
	}

	// Native enumeration proves the folder is empty, so a nonrecursive delete
	// succeeds even while the observed index still contains the missing child.
	lifecycleStatus(t, h.request("DELETE", lifecycleTarget("", "/EmptyNative"), h.bearer, nil, nil), 204)
	if _, err := os.Stat(filepath.Join(ownerRoot, "EmptyNative")); !os.IsNotExist(err) {
		t.Fatalf("HTTP delete left the native empty folder: %v", err)
	}
	for _, retired := range []string{folder.ID, child.ID} {
		lifecycleStatus(t, h.request("GET", fc.Base+"?id="+retired, h.bearer, nil, nil), 404)
	}
	lifecycleMarkerChanged(t, rootBefore, h.metadata("/"))
	page := h.changes(rootWatch.ChangesCursor, h.bearer)
	if len(page.Items) != 1 || page.Items[0].Type != "remove" || page.Items[0].Coverage != "subtree" || page.Items[0].Before == nil || page.Items[0].Before.ID != folder.ID || page.Items[0].Before.Kind != "folder" || page.Items[0].After != nil {
		t.Fatalf("native folder deletion did not retain one bounded subtree tombstone: %+v", page.Items)
	}
	lifecycleAffectedFolders(t, page.Items[0], "/", 1, "/")
	childPage := h.changes(childWatch.ChangesCursor, h.bearer)
	if len(childPage.Items) != 2 || childPage.Items[0].Type != "remove" || childPage.Items[0].Coverage != "entry" || childPage.Items[0].Path != "/EmptyNative/stale.txt" || childPage.Items[0].EntryID == nil || *childPage.Items[0].EntryID != child.ID || childPage.Items[1].Type != "watch_ended" || childPage.Items[1].Reason != "deleted" {
		t.Fatalf("stale descendant ID watch did not receive targeted removal and terminal deletion: %+v", childPage.Items)
	}
	ended := h.request("GET", fc.Base+"/changes?"+url.Values{"cursor": {childPage.NextCursor}}.Encode(), h.bearer, nil, nil)
	problem := lifecycleDecode[fc.Problem](t, ended, 409)
	if problem.Code != "watch_deleted" {
		t.Fatalf("deleted descendant watch kept polling without termination: %+v", problem)
	}
}
