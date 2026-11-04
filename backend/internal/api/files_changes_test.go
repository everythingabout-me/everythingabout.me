package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/filechanges"
	fc "everythingabout.me/backend/internal/filecontract"
)

func filesChangesTestCursor() filesChangeCursor {
	return filesChangeCursor{Epoch: mutationRootID, After: 9, Expires: time.Now().Add(time.Hour).Unix(), Filter: filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope"}, Kind: "folder", Recursive: true}, Anchor: "/Scope"}
}
func TestFilesChangesCursorBindsPolicyAndHidesPositions(t *testing.T) {
	s, f, _, _ := filesReadFixture()
	f.Principal.Token = &auth.Token{ID: "cursor-token", CanRead: true}
	f.Principal.Token.CanChanges = true
	f.Principal.Token.CanSubscribe = true
	f.Principal.Token.PolicyVersion = 1
	c := filesChangesTestCursor()
	token, err := s.filesEncodeChangeCursor(f, c)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := s.filesDecodeChangeCursor(f, token)
	if err != nil || decoded.After != 9 || decoded.Anchor != "/Scope" {
		t.Fatalf("roundtrip: %+v %v", decoded, err)
	}
	c.After = 1000000000
	token2, err := s.filesEncodeChangeCursor(f, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != len(token2) || token == token2 {
		t.Fatal("cursor length or randomness discloses progress")
	}
	restart := &Server{cfg: s.cfg}
	if _, err = restart.filesDecodeChangeCursor(f, token); err != nil {
		t.Fatalf("restart invalidated durable cursor: %v", err)
	}
	for _, change := range []struct {
		name  string
		apply func(*fileRequest)
	}{
		{"root", func(f *fileRequest) { f.ContentNamespace += "different" }},
		{"owner", func(f *fileRequest) { f.Principal.User.ID = "other-owner" }},
		{"principal", func(f *fileRequest) { f.Principal.Token.ID = "other-token" }},
		{"scope", func(f *fileRequest) { f.Scope = "/Scope/child" }},
		{"grant", func(f *fileRequest) { f.Principal.Token.CanChanges = false }},
		{"policy restored", func(f *fileRequest) { f.Principal.Token.PolicyVersion++ }},
		{"hidden roots", func(f *fileRequest) {
			f.Driver = &pendingDeleteDriver{Driver: f.Driver, roots: []string{"Scope/hidden"}}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			_, other, _, _ := filesReadFixture()
			other.Principal.Token = &auth.Token{ID: "cursor-token", CanRead: true}
			other.Principal.Token.CanChanges = true
			other.Principal.Token.CanSubscribe = true
			other.Principal.Token.PolicyVersion = 1
			change.apply(other)
			if _, err := s.filesDecodeChangeCursor(other, token); err == nil {
				t.Fatal("changed binding accepted")
			}
		})
	}
	c.Expires = time.Now().Add(-time.Second).Unix()
	expired, err := s.filesEncodeChangeCursor(f, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.filesDecodeChangeCursor(f, expired); filesProblem(err).Status != 410 {
		t.Fatalf("expired cursor: %v", err)
	}
}
func TestFilesChangesRequiresExplicitGrants(t *testing.T) {
	_, f, _, _ := filesReadFixture()
	f.Principal.Token = &auth.Token{ID: "read-only", CanRead: true}
	f.Principal.Token.CanRead = true
	if err := f.requireChanges(false); filesProblem(err).Status != 403 {
		t.Fatalf("read granted changes: %v", err)
	}
	f.Principal.Token.CanChanges = true
	if err := f.requireChanges(false); err != nil {
		t.Fatal(err)
	}
	if err := f.requireChanges(true); filesProblem(err).Status != 403 {
		t.Fatalf("changes granted subscriptions: %v", err)
	}
	f.Principal.Token.CanSubscribe = true
	if err := f.requireChanges(true); err != nil {
		t.Fatal(err)
	}
	f.Principal.Token.CanRead = false
	if err := f.requireChanges(false); filesProblem(err).Status != 403 {
		t.Fatalf("changes bypassed read: %v", err)
	}
	f.Principal.Token = nil
	if err := f.requireChanges(true); err != nil {
		t.Fatalf("owner denied: %v", err)
	}
}
func TestFilesChangesQueryCarriesOnlyOpaqueCursor(t *testing.T) {
	for _, q := range []string{"", "cursor=", "cursor=a&cursor=b", "cursor=a&path=/Scope", "cursor=a&limit=0", "cursor=a&limit=1001", "cursor=a&limit=oops", "cursor=a&unknown=1"} {
		if _, _, err := filesChangesQuery(httptest.NewRequest(http.MethodGet, "/api/files/changes?"+q, nil)); err == nil {
			t.Fatalf("invalid query accepted: %s", q)
		}
	}
	if cursor, limit, err := filesChangesQuery(httptest.NewRequest(http.MethodGet, "/api/files/changes?cursor=opaque&limit=1", nil)); err != nil || cursor != "opaque" || limit != 1 {
		t.Fatalf("valid query: %s %d %v", cursor, limit, err)
	}
}

func filesChangesPGOwner(t *testing.T, fixture *fileWorkerFixture) (*Server, *fileRequest, filesChangeCursor) {
	t.Helper()
	var owner string
	if err := fixture.db.Pool().QueryRow(fixture.ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("changes-%d@example.test", time.Now().UnixNano())).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	s, f, _, _ := filesReadFixture()
	s.db = fixture.db
	s.cfg = fixture.cfg
	f.Principal = &auth.Principal{User: &auth.User{ID: owner}}
	f.Scope = "/Scope"
	f.ContentNamespace = "changes-test-root:" + owner
	tx, err := fixture.db.Pool().Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, err := filechanges.EnsureHead(fixture.ctx, tx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	c := filesChangesTestCursor()
	c.Epoch = head.RootEpoch
	c.After = head.Position
	return s, f, c
}
func filesChangesPGAppend(t *testing.T, s *Server, f *fileRequest, c filechanges.Change) int64 {
	t.Helper()
	c.OwnerID = f.Principal.User.ID
	tx, err := s.db.Pool().Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	position, err := filechanges.Append(context.Background(), tx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return position
}
func filesChangesMetadata(id string, p fc.Path) *fc.Metadata {
	parent := "/"
	name := string(p)
	if at := strings.LastIndexByte(string(p), '/'); at >= 0 {
		parent = string(p)[:at]
		name = string(p)[at+1:]
		if parent == "" {
			parent = "/"
		}
	}
	return &fc.Metadata{ID: id, ParentID: filesChangesString(mutationFolderID), Path: p, ParentPath: fc.Path(parent), Name: name, Kind: "file", Revision: "fixture-revision"}
}
func filesChangesString(v string) *string { return &v }
func filesChangesRead(t *testing.T, s *Server, f *fileRequest, c filesChangeCursor, limit int) filesChangesPage {
	t.Helper()
	page, err := s.filesReadChanges(context.Background(), f, c, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}
func filesChangesDecode(t *testing.T, s *Server, f *fileRequest, page filesChangesPage) filesChangeCursor {
	t.Helper()
	c, err := s.filesDecodeChangeCursor(f, page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func TestFilesChangesPostgresFilteringPinnedHeadAndRetention(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	f.Driver = &pendingDeleteDriver{Driver: f.Driver, roots: []string{"Scope/hidden"}}
	appendCreated := func(key string, p fc.Path) {
		filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: key, Kind: "created", After: filesChangesMetadata(mutationFileID, p)})
	}
	for i := 0; i < 6; i++ {
		appendCreated(fmt.Sprintf("private-%d", i), "/Private/hidden")
	}
	appendCreated("visible-a", "/Scope/a")
	for i := 0; i < 6; i++ {
		appendCreated(fmt.Sprintf("hidden-%d", i), "/Scope/hidden/a")
	}
	appendCreated("visible-b", "/Scope/b")
	first := filesChangesRead(t, s, f, c, 1)
	if len(first.Items) != 1 || first.Items[0].After.Path != "/Scope/a" || !first.HasMore {
		t.Fatalf("sparse first page: %+v", first)
	}
	next := filesChangesDecode(t, s, f, first)
	if next.Through == nil {
		t.Fatal("first page did not bind committed head")
	}
	appendCreated("late-visible-c", "/Scope/c")
	second := filesChangesRead(t, s, f, next, 1)
	if len(second.Items) != 1 || second.Items[0].After.Path != "/Scope/b" || second.HasMore {
		t.Fatalf("late commit escaped pinned window: %+v", second)
	}
	third := filesChangesRead(t, s, f, filesChangesDecode(t, s, f, second), 1)
	if len(third.Items) != 1 || third.Items[0].After.Path != "/Scope/c" {
		t.Fatalf("next window omitted new commit: %+v", third)
	}
	appendCreated("only-hidden", "/Private/another")
	empty := filesChangesRead(t, s, f, filesChangesDecode(t, s, f, third), 1)
	if len(empty.Items) != 0 || empty.Items == nil || empty.HasMore || empty.NextCursor == "" {
		t.Fatalf("hidden-only page leaks: %+v", empty)
	}
	encoded, _ := json.Marshal(empty)
	for _, secret := range []string{"position", "epoch", "Private", "ownerId", "retained"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("private progress disclosed: %s", encoded)
		}
	}
	if _, err := s.db.Pool().Exec(fixture.ctx, `update file_change_heads set retained_after=head_position where owner_id=$1::uuid`, f.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.filesReadChanges(fixture.ctx, f, c, 1); filesProblem(err).Status != 410 {
		t.Fatalf("retention resume: %v", err)
	}
}
func TestFilesChangesPostgresMoveProjectionAndTargetBeforeLimit(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "move-out", Kind: "moved", Before: filesChangesMetadata(mutationFileID, "/Scope/a"), After: filesChangesMetadata(mutationFileID, "/Private/a")})
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "move-in", Kind: "moved", Before: filesChangesMetadata(mutationOtherID, "/Private/b"), After: filesChangesMetadata(mutationOtherID, "/Scope/b")})
	page := filesChangesRead(t, s, f, c, 10)
	if len(page.Items) != 2 || page.Items[0].Type != "remove" || page.Items[0].After != nil || page.Items[1].Type != "upsert" || page.Items[1].Before != nil {
		t.Fatalf("concealed move endpoints: %+v", page)
	}
	encoded, _ := json.Marshal(page)
	if bytes.Contains(encoded, []byte("Private")) {
		t.Fatalf("concealed path disclosed: %s", encoded)
	}
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/b"}, Kind: "file"}
	c.Anchor = "/Scope/b"
	page = filesChangesRead(t, s, f, c, 1)
	if len(page.Items) != 1 || page.Items[0].After.Path != "/Scope/b" || page.HasMore {
		t.Fatalf("target filtered after limit: %+v", page)
	}
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{ID: mutationFileID}, Kind: "file"}
	c.Anchor = "/Scope/a"
	page = filesChangesRead(t, s, f, c, 1)
	if len(page.Items) != 1 || page.Items[0].Type != "remove" {
		t.Fatalf("retained ID tombstone not replayed: %+v", page)
	}
}
func TestFilesChangesPostgresSubtreeInvalidationAndIsolation(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	before := filesChangesMetadata(mutationFolderID, "/Scope")
	before.Kind = "folder"
	after := filesChangesMetadata(mutationFolderID, "/Elsewhere")
	after.Kind = "folder"
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "ancestor-move", Kind: "moved", Coverage: "subtree", Before: before, After: after})
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/child"}, Kind: "folder", Recursive: true}
	c.Anchor = "/Scope/child"
	page := filesChangesRead(t, s, f, c, 1)
	if len(page.Items) != 1 || page.Items[0].Type != "invalidated" || page.Items[0].Before != nil || page.Items[0].After != nil || page.Items[0].EntryID != nil || page.Items[0].Path != "/Scope/child" {
		t.Fatalf("ancestor disclosed or expanded: %+v", page)
	}
	_, other, otherCursor := filesChangesPGOwner(t, fixture)
	otherPage := filesChangesRead(t, s, other, otherCursor, 1)
	if len(otherPage.Items) != 0 {
		t.Fatalf("cross-owner event: %+v", otherPage)
	}
}
func TestFilesChangesAffectedFolderViewFiltering(t *testing.T) {
	_, f, _, _ := filesReadFixture()
	f.Scope = "/Scope"
	folder := func(p fc.Path) fc.Metadata {
		m := *filesChangesMetadata(mutationFolderID, p)
		m.Kind = "folder"
		now := time.Now()
		m.LastChangedAt = &now
		m.ChangeToken = "recursive-marker"
		return m
	}
	patches := []fc.Metadata{folder("/Scope/Watch"), folder("/Scope/Watch/child"), folder("/Scope/Watch/child/deep"), folder("/Scope/Other"), folder("/Private")}
	for _, test := range []struct {
		name            string
		filter          filesChangeFilter
		current, anchor fc.Path
		typ             string
		want            int
	}{
		{"file", filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/Watch/file"}, Kind: "file"}, "/Scope/Watch/file", "/Scope/Watch/file", "upsert", 0},
		{"recursive path", filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/Watch"}, Kind: "folder", Recursive: true}, "/Scope/Watch", "/Scope/Watch", "upsert", 3},
		{"direct path", filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/Watch"}, Kind: "folder"}, "/Scope/Watch", "/Scope/Watch", "upsert", 2},
		{"ID current", filesChangeFilter{Target: fc.EntrySelector{ID: mutationFolderID}, Kind: "folder", Recursive: true}, "/Scope/Other", "/Scope/Watch", "upsert", 1},
		{"ID departure", filesChangeFilter{Target: fc.EntrySelector{ID: mutationFolderID}, Kind: "folder", Recursive: true}, "/Private", "/Scope/Watch", "remove", 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := filechanges.Projection{Type: test.typ, AffectedFolders: patches}
			filesFilterChangeFolders(f, filesChangeCursor{Filter: test.filter, Anchor: test.anchor}, test.current, &item)
			if len(item.AffectedFolders) != test.want {
				t.Fatalf("patches outside selected view: %+v", item.AffectedFolders)
			}
		})
	}
	f.Driver = &pendingDeleteDriver{Driver: f.Driver, roots: []string{"Scope/Watch/child"}}
	before, after := folder("/Scope/Watch"), folder("/Scope/Watch")
	item := filechanges.Projection{Before: &before, After: &after, AffectedFolders: patches}
	filesFilterChangeFolders(f, filesChangeCursor{Filter: filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/Watch"}, Kind: "folder", Recursive: true}}, "/Scope/Watch", &item)
	if len(item.AffectedFolders) != 1 || item.AffectedFolders[0].ChangeToken != "" || item.AffectedFolders[0].LastChangedAt != nil || item.Before.ChangeToken != "" || item.After.ChangeToken != "" {
		t.Fatalf("concealed aggregates escaped redaction: %+v", item)
	}
	root := folder("/Scope")
	item = filechanges.Projection{AffectedFolders: []fc.Metadata{root}}
	filesFilterChangeFolders(f, filesChangeCursor{Filter: filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope"}, Kind: "folder"}}, "/Scope", &item)
	if len(item.AffectedFolders) != 1 || item.AffectedFolders[0].ParentID != nil || item.AffectedFolders[0].ParentPath != "" {
		t.Fatalf("concealed parent escaped patch: %+v", item)
	}
}

func TestFilesChangesPostgresAffectedFoldersRespectWatch(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	patch := func(id string, p fc.Path) fc.Metadata { m := *filesChangesMetadata(id, p); m.Kind = "folder"; return m }
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/Watch"}, Kind: "folder", Recursive: true}
	c.Anchor = "/Scope/Watch"
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "patch-view", Kind: "created", After: filesChangesMetadata(mutationFileID, "/Scope/Watch/a"), AffectedFolders: []fc.Metadata{patch(mutationFolderID, "/Scope/Watch"), patch(mutationRootID, "/Scope"), patch(mutationOtherID, "/Scope/Other")}})
	page := filesChangesRead(t, s, f, c, 10)
	if len(page.Items) != 1 || len(page.Items[0].AffectedFolders) != 1 || page.Items[0].AffectedFolders[0].Path != "/Scope/Watch" {
		t.Fatalf("retained patch view mismatch: %+v", page)
	}
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/Watch/a"}, Kind: "file"}
	page = filesChangesRead(t, s, f, c, 10)
	if len(page.Items) != 1 || len(page.Items[0].AffectedFolders) != 0 {
		t.Fatalf("file watch disclosed ancestor aggregates: %+v", page)
	}
}

func TestFilesChangesPostgresIDTerminationAndPathReplacement(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "delete-selected", Kind: "deleted", Before: filesChangesMetadata(mutationFileID, "/Scope/a")})
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "replace-path", Kind: "created", After: filesChangesMetadata(mutationOtherID, "/Scope/a")})
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{ID: mutationFileID}, Kind: "file"}
	c.Anchor = "/Scope/a"
	remove := filesChangesRead(t, s, f, c, 1)
	if len(remove.Items) != 1 || remove.Items[0].Type != "remove" || !remove.HasMore {
		t.Fatalf("missing terminal continuation: %+v", remove)
	}
	ended := filesChangesRead(t, s, f, filesChangesDecode(t, s, f, remove), 1)
	if len(ended.Items) != 1 || ended.Items[0].Type != "watch_ended" || ended.Items[0].Reason != "deleted" || ended.HasMore {
		t.Fatalf("terminal marker: %+v", ended)
	}
	if _, err := s.filesReadChanges(fixture.ctx, f, filesChangesDecode(t, s, f, ended), 1); filesProblem(err).Status != 409 || filesProblem(err).Code != "watch_deleted" {
		t.Fatalf("ended ID watch resumed: %v", err)
	}
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{Path: "/Scope/a"}, Kind: "file"}
	pathPage := filesChangesRead(t, s, f, c, 10)
	if len(pathPage.Items) != 2 || pathPage.Items[0].Type != "remove" || pathPage.Items[1].Type != "upsert" {
		t.Fatalf("fixed path did not follow replacement: %+v", pathPage)
	}
}

func TestFilesChangesPostgresRootBindingRequiresExplicitReset(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "root-bound-event", Kind: "created", After: filesChangesMetadata(mutationFileID, "/Scope/a")})
	originalNamespace := f.ContentNamespace
	page := filesChangesRead(t, s, f, c, 10)
	if len(page.Items) != 1 {
		t.Fatalf("initial root feed: %+v", page)
	}
	f.ContentNamespace = originalNamespace + ":new-root"
	if _, err := s.filesReadChanges(fixture.ctx, f, c, 10); filesProblem(err).Status != 410 || filesProblem(err).Code != "reset_required" {
		t.Fatalf("root rebind silently reused history: %v", err)
	}
	head, err := filechanges.ReadHead(fixture.ctx, s.db.Pool(), f.Principal.User.ID)
	if err != nil || head.RootNamespace != originalNamespace {
		t.Fatalf("mismatch rebound durable root: %+v %v", head, err)
	}
	if _, err = s.filesPollSubscription(fixture.ctx, f, filesSubscription{endedReason: "deleted"}); filesProblem(err).Code != "reset_required" {
		t.Fatalf("ended matcher bypassed root check: %v", err)
	}
	f.ContentNamespace = originalNamespace
	filesChangesRead(t, s, f, c, 10)
	tx, err := s.db.Pool().Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := filechanges.ResetRoot(fixture.ctx, tx, f.Principal.User.ID, originalNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if reset.RootEpoch == c.Epoch {
		t.Fatal("same-root reset reused epoch")
	}
	if _, err = s.filesReadChanges(fixture.ctx, f, c, 10); filesProblem(err).Status != 410 {
		t.Fatalf("old cursor revived after reset: %v", err)
	}
	fresh := c
	fresh.Epoch = reset.RootEpoch
	fresh.After = reset.Position
	if page = filesChangesRead(t, s, f, fresh, 10); len(page.Items) != 0 {
		t.Fatalf("fresh reset epoch replayed old history: %+v", page)
	}
}

func TestFilesChangesPostgresConcealedSubtreeArrivalRequiresBaseline(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	before := filesChangesMetadata(mutationFolderID, "/Private/tree")
	before.Kind = "folder"
	after := filesChangesMetadata(mutationFolderID, "/Scope/tree")
	after.Kind = "folder"
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "concealed-tree-arrival", Kind: "moved", Coverage: "subtree", Before: before, After: after})
	for _, target := range []fc.EntrySelector{{Path: "/Scope"}, {Path: "/Scope/tree"}} {
		c.Filter = filesChangeFilter{Target: target, Kind: "folder", Recursive: true}
		c.Anchor = target.Path
		page := filesChangesRead(t, s, f, c, 10)
		if len(page.Items) != 1 || page.Items[0].Type != "invalidated" || page.Items[0].Path != "/Scope/tree" || page.Items[0].Before != nil || page.Items[0].After != nil || page.Items[0].EntryID != nil {
			t.Fatalf("subtree arrival silently omitted membership: %+v", page)
		}
		if _, err := s.filesReadChanges(fixture.ctx, f, filesChangesDecode(t, s, f, page), 10); filesProblem(err).Code != "baseline_required" {
			t.Fatalf("incomplete arrival view continued: %v", err)
		}
	}
	f.Scope = "/"
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{Path: "/"}, Kind: "folder", Recursive: true}
	c.Anchor = "/"
	page := filesChangesRead(t, s, f, c, 10)
	if len(page.Items) != 1 || page.Items[0].Type != "moved" || page.Items[0].Before == nil || page.Items[0].After == nil {
		t.Fatalf("fully visible linkage was expanded: %+v", page)
	}
}

func TestFilesChangesPostgresIDMoveThenNewAncestorDeparture(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{ID: mutationFileID}, Kind: "file"}
	c.Anchor = "/Scope/A/file"
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "id-to-new-parent", Kind: "moved", Before: filesChangesMetadata(mutationFileID, "/Scope/A/file"), After: filesChangesMetadata(mutationFileID, "/Scope/B/file")})
	before := filesChangesMetadata(mutationFolderID, "/Scope/B")
	before.Kind = "folder"
	after := filesChangesMetadata(mutationFolderID, "/Private/B")
	after.Kind = "folder"
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "new-ancestor-departure", Kind: "moved", Coverage: "subtree", Before: before, After: after})
	page := filesChangesRead(t, s, f, c, 10)
	if len(page.Items) != 3 || page.Items[0].Type != "moved" || page.Items[1].Type != "remove" || page.Items[1].Path != "/Scope/B/file" || page.Items[2].Type != "watch_ended" || page.Items[2].Reason != "access_lost" {
		t.Fatalf("new ancestor terminal was skipped: %+v", page)
	}
	wire, _ := json.Marshal(page)
	if bytes.Contains(wire, []byte("Private")) {
		t.Fatalf("concealed new ancestor leaked: %s", wire)
	}
	if _, err := s.filesReadChanges(fixture.ctx, f, filesChangesDecode(t, s, f, page), 10); filesProblem(err).Code != "watch_access_lost" {
		t.Fatalf("terminal watch resumed: %v", err)
	}
	first := filesChangesRead(t, s, f, c, 1)
	firstCursor := filesChangesDecode(t, s, f, first)
	if firstCursor.Anchor != "/Scope/B/file" || !first.HasMore {
		t.Fatalf("one-item page lost new anchor: %+v %+v", first, firstCursor)
	}
	second := filesChangesRead(t, s, f, firstCursor, 1)
	if len(second.Items) != 1 || second.Items[0].Type != "remove" || !second.HasMore {
		t.Fatalf("departure missing after page boundary: %+v", second)
	}
	// A durable matcher processes only its emitted prefix and preserves the anchor
	// for the next poll, even when the pinned head contains the ancestor departure.
	var subID string
	err := s.db.Pool().QueryRow(fixture.ctx, `insert into file_subscriptions(owner_id,principal_id,root_namespace,epoch,policy_fingerprint,mode,selector_id,kind,recursive,start_position,signal_position,anchor_path)
 values($1::uuid,$2,$3,$4::uuid,$5,'id',$6::uuid,'file',false,0,0,$7) returning id::text`, f.Principal.User.ID, filePrincipalID(f.Principal), f.ContentNamespace, c.Epoch, fmt.Sprintf("%x", filesChangePolicy(f)), mutationFileID, string(c.Anchor)).Scan(&subID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.filesLoadSubscription(fixture.ctx, f, subID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err = s.filesPollSubscription(fixture.ctx, f, sub)
	if err != nil {
		t.Fatal(err)
	}
	if sub.signalPosition != 1 || sub.signalAnchor != "/Scope/B/file" || sub.anchor != c.Anchor || sub.endedReason != "" {
		t.Fatalf("matcher skipped prefix/anchor: %+v", sub)
	}
	get := httptest.NewRequest(http.MethodGet, fc.Base+"/subscriptions/"+subID, nil)
	get.SetPathValue("id", subID)
	getW := httptest.NewRecorder()
	if err = s.handleFilesGetSubscription(getW, get, f); err != nil {
		t.Fatal(err)
	}
	var resource filesSubscription
	if err = json.Unmarshal(getW.Body.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	baseline, err := s.filesDecodeChangeCursor(f, resource.ChangesCursor)
	if err != nil || baseline.Anchor != c.Anchor || baseline.After != c.After {
		t.Fatalf("GET moved consumer baseline: %+v %v", baseline, err)
	}
	renew := httptest.NewRequest(http.MethodPost, fc.Base+"/subscriptions/"+subID+"/renew", nil)
	renew.SetPathValue("id", subID)
	renewW := httptest.NewRecorder()
	if err = s.handleFilesRenewSubscription(renewW, renew, f); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(renewW.Body.Bytes(), &resource); err != nil {
		t.Fatal(err)
	}
	baseline, err = s.filesDecodeChangeCursor(f, resource.ChangesCursor)
	if err != nil || baseline.Anchor != c.Anchor || baseline.After != c.After {
		t.Fatalf("renewal moved consumer baseline: %+v %v", baseline, err)
	}
	sub, err = s.filesLoadSubscription(fixture.ctx, f, subID)
	if err != nil {
		t.Fatal(err)
	}
	sub, err = s.filesPollSubscription(fixture.ctx, f, sub)
	if err != nil {
		t.Fatal(err)
	}
	if sub.signalPosition != 2 || sub.endedReason != "access_lost" {
		t.Fatalf("matcher lost new ancestor terminal: %+v", sub)
	}
}

func TestFilesChangesPostgresAbsentPathSubscription(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, _ := filesChangesPGOwner(t, fixture)
	f.Scope = "/"
	s.filesIndex = fixture.server().filesIndex
	driver := f.Driver.(*filesReadTestDriver)
	create := func(target fc.EntrySelector, kind string, recursive bool) (filesSubscription, error) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"target": target, "kind": kind, "recursive": recursive, "delivery": map[string]string{"type": "sse"}})
		w := httptest.NewRecorder()
		err := s.handleFilesCreateSubscription(w, httptest.NewRequest(http.MethodPost, "/api/files/subscriptions", bytes.NewReader(body)), f)
		var sub filesSubscription
		if err == nil {
			if w.Code != http.StatusCreated {
				t.Fatalf("unexpected create status: %d", w.Code)
			}
			if err = json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
				t.Fatal(err)
			}
		}
		return sub, err
	}
	sub, err := create(fc.EntrySelector{Path: "/future.txt"}, "file", false)
	if err != nil || sub.Filter.Target.Path != "/future.txt" || sub.ChangesCursor == "" {
		t.Fatalf("absent file subscription: %+v %v", sub, err)
	}
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "future-file-created", Kind: "created", After: filesChangesMetadata(mutationFileID, "/future.txt")})
	cursor, err := s.filesDecodeChangeCursor(f, sub.ChangesCursor)
	if err != nil {
		t.Fatal(err)
	}
	page := filesChangesRead(t, s, f, cursor, 10)
	if len(page.Items) != 1 || page.Items[0].Type != "upsert" || page.Items[0].After.Path != "/future.txt" {
		t.Fatalf("future path creation omitted: %+v", page)
	}
	folder, err := create(fc.EntrySelector{Path: "/future-folder"}, "folder", true)
	if err != nil {
		t.Fatal(err)
	}
	folderMetadata := filesChangesMetadata(mutationFolderID, "/future-folder")
	folderMetadata.Kind = "folder"
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "future-folder-created", Kind: "created", After: folderMetadata})
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "future-child-created", Kind: "created", After: filesChangesMetadata(mutationOtherID, "/future-folder/child")})
	cursor, err = s.filesDecodeChangeCursor(f, folder.ChangesCursor)
	if err != nil {
		t.Fatal(err)
	}
	if page = filesChangesRead(t, s, f, cursor, 10); len(page.Items) != 2 {
		t.Fatalf("declared future folder coverage lost: %+v", page)
	}
	for _, target := range []fc.EntrySelector{{ID: mutationOtherID}, {Path: "/unknown-parent/child"}} {
		if _, err = create(target, "file", false); filesProblem(err).Status != http.StatusNotFound {
			t.Fatalf("unknown target or parent disclosed: %+v %v", target, err)
		}
	}
	f.Scope = "/Scope"
	for _, target := range []fc.EntrySelector{{Path: "/Private/child"}, {Path: "/Scope"}, {Path: "/Scope/missing"}} {
		if _, err = create(target, "file", false); filesProblem(err).Status != http.StatusNotFound {
			t.Fatalf("unauthorized or absent parent accepted: %+v %v", target, err)
		}
	}
	if driver.statCalls != 0 || driver.listCalls != 0 || driver.openCalls != 0 {
		t.Fatalf("subscription admission called native storage: %+v", driver)
	}
}

func TestFilesChangesPostgresSubscriptionDurabilitySSEAndDelete(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, _ := filesChangesPGOwner(t, fixture)
	// Use the real indexed root for admission. No native file operations are needed.
	f.Scope = "/"
	// filesReadFixture has no nodes service, so use production service from fixture.
	s.filesIndex = fixture.server().filesIndex
	body := `{"target":{"path":"/"},"kind":"folder","recursive":true,"delivery":{"type":"sse"}}`
	w := httptest.NewRecorder()
	if err := s.handleFilesCreateSubscription(w, httptest.NewRequest(http.MethodPost, "/api/files/subscriptions", strings.NewReader(body)), f); err != nil {
		t.Fatal(err)
	}
	var sub filesSubscription
	if err := json.Unmarshal(w.Body.Bytes(), &sub); err != nil {
		t.Fatal(err)
	}
	if w.Code != 201 || sub.ID == "" || sub.ChangesCursor == "" || sub.EventsURL == "" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	restarted := &Server{cfg: s.cfg, db: s.db, filesIndex: s.filesIndex}
	get := httptest.NewRequest(http.MethodGet, "/api/files/subscriptions/"+sub.ID, nil)
	get.SetPathValue("id", sub.ID)
	if err := restarted.handleFilesGetSubscription(httptest.NewRecorder(), get, f); err != nil {
		t.Fatalf("durable subscription lost: %v", err)
	}
	filesChangesPGAppend(t, s, f, filechanges.Change{EffectKey: "wake", Kind: "created", After: filesChangesMetadata(mutationFileID, "/Scope/a")})
	event := httptest.NewRequest(http.MethodGet, sub.EventsURL, nil)
	event.SetPathValue("id", sub.ID)
	event.Header.Set("Last-Event-ID", "not-a-consumer-cursor")
	eventW := httptest.NewRecorder()
	if err := restarted.handleFilesSubscriptionEvents(eventW, event, f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(eventW.Body.String(), "changes_available") || strings.Contains(eventW.Body.String(), "cursor") || strings.Contains(eventW.Body.String(), "Scope") {
		t.Fatalf("SSE did not send isolated signal: %s", eventW.Body.String())
	}
	stored, err := restarted.filesLoadSubscription(fixture.ctx, f, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.notificationID == "" || stored.signalPosition <= stored.start {
		t.Fatal("SSE matcher state was not durable")
	}
	same, err := restarted.filesPollSubscription(fixture.ctx, f, stored)
	if err != nil || same.notificationID != stored.notificationID {
		t.Fatalf("unchanged head regenerated wake: %+v %v", same, err)
	}
	replayW := httptest.NewRecorder()
	if err = restarted.handleFilesSubscriptionEvents(replayW, event, f); err != nil || !strings.Contains(replayW.Body.String(), stored.notificationID) {
		t.Fatalf("reconnect did not replay durable notification: %v %s", err, replayW.Body.String())
	}
	quietCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	acknowledged := event.Clone(quietCtx)
	acknowledged.Header.Set("Last-Event-ID", stored.notificationID)
	quietW := httptest.NewRecorder()
	if err = restarted.handleFilesSubscriptionEvents(quietW, acknowledged, f); err != nil || strings.Contains(quietW.Body.String(), "changes_available") {
		t.Fatalf("acknowledged wake repeated: %v %s", err, quietW.Body.String())
	}
	pull := httptest.NewRequest(http.MethodGet, "/api/files/changes?cursor="+url.QueryEscape(sub.ChangesCursor), nil)
	pullW := httptest.NewRecorder()
	if err := restarted.handleFilesChanges(pullW, pull, f); err != nil {
		t.Fatal(err)
	}
	var page filesChangesPage
	if err := json.Unmarshal(pullW.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("wake advanced consumer cursor: %v %s", err, pullW.Body.String())
	}
	del := httptest.NewRequest(http.MethodDelete, "/api/files/subscriptions/"+sub.ID, nil)
	del.SetPathValue("id", sub.ID)
	if err := restarted.handleFilesDeleteSubscription(httptest.NewRecorder(), del, f); err != nil {
		t.Fatal(err)
	}
	if err := restarted.handleFilesGetSubscription(httptest.NewRecorder(), get, f); filesProblem(err).Status != 404 {
		t.Fatalf("deleted subscription returned: %v", err)
	}
}
