package api

// Postgres-backed probes for the fail-closed ID→path resolution described in
// docs/design/file-identity.md §"The cached-path fallback". They build the audited state
// with the real index (a moved ancestor row removed while the child keeps its cached path)
// and then ask the public entry points, because that is where the wrong answer used to be
// observable: a scope match, a mutation binding, and a NULL cache.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
)

func TestFilesUnresolvedAncestryFailsClosed(t *testing.T) {
	f := newFileWorkerFixture(t)
	h := newUploadsHTTPFixture(t, f)
	jsonBody := http.Header{"Content-Type": {"application/json"}}
	photos := uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Photos"}`), jsonBody), 201)["id"].(string)
	uploadsHTTPJSON(t, h.request("POST", "/api/files", h.bearer, []byte(`{"kind":"folder","path":"/Photos/2024"}`), jsonBody), 201)
	created := uploadsHTTPJSON(t, h.request("PUT", "/api/files/content?path=%2FPhotos%2F2024%2Fx.jpg", h.bearer, []byte("old"), http.Header{"Content-Type": {"text/plain"}}), 201)
	photo := created["id"].(string)

	// The audit's shape: the folder relinks, the child keeps the pre-move cached path, and
	// removing the moved row leaves the child with no ancestry.
	if _, err := f.db.Pool().Exec(f.ctx, `update nodes set key=$3,path='Media/Photos',parent='Media',name='Photos'
		where owner_id=$1::uuid and id=$2::uuid`, h.root.OwnerID, photos, "drive/"+h.root.OwnerID+"/Media/Photos/"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool().Exec(f.ctx, `delete from nodes where owner_id=$1::uuid and id=$2::uuid`, h.root.OwnerID, photos); err != nil {
		t.Fatal(err)
	}
	var cached *string
	if err := f.db.Pool().QueryRow(f.ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid`, h.root.OwnerID, photo).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if cached == nil || *cached != "Photos/2024/x.jpg" {
		t.Fatalf("probe setup: cached path %v, want the pre-move spelling", cached)
	}

	// A scope the entry has left must not admit the stale cache.
	scoped := &fileRequest{
		Principal: &auth.Principal{User: &auth.User{ID: h.root.OwnerID}},
		Scope:     "/Photos",
		Driver:    &filesReadTestDriver{descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: true}}},
	}
	if _, err := h.server.filesResolveSelector(f.ctx, scoped, fc.EntrySelector{ID: photo}); err == nil || filesProblem(err).Status != 404 {
		t.Fatalf("broken chain admitted by a scope it left: %v", err)
	}

	bind := func() error {
		intent, err := json.Marshal(fileops.ActionRequest{Kind: "delete", EntryID: photo, From: "Photos/2024/x.jpg"})
		if err != nil {
			t.Fatal(err)
		}
		return h.server.filesCheckIdentityBindings(f.ctx, fileops.Operation{Kind: "delete", Root: h.root, Intent: intent})
	}

	// A mutation must not bind to the path the cache still spells, even though that string
	// equals the path the operation captured.
	if err := bind(); !fileops.IsCode(err, "target_changed") {
		t.Fatalf("mutation binding accepted a broken chain: %v", err)
	}
	uploadsHTTPProblem(t, h.request("GET", "/api/files?id="+photo, h.bearer, nil, nil), 404, "not_found")
	uploadsHTTPProblem(t, h.request("DELETE", "/api/files?id="+photo, h.bearer, nil, nil), 404, "not_found")

	// With the cache gone the view returns NULL. Scanning that used to be an internal
	// error; it stays the same named not-found.
	if _, err := f.db.Pool().Exec(f.ctx, `update nodes set path=null,parent=null,key=null,size=size+1
		where owner_id=$1::uuid and id=$2::uuid`, h.root.OwnerID, photo); err != nil {
		t.Fatal(err)
	}
	cached = nil
	if err := f.db.Pool().QueryRow(f.ctx, `select path from file_entries where owner_id=$1::uuid and id=$2::uuid`, h.root.OwnerID, photo).Scan(&cached); err != nil {
		t.Fatal(err)
	}
	if cached != nil {
		t.Fatalf("cache was not nulled: %q", *cached)
	}
	if err := bind(); !fileops.IsCode(err, "target_changed") {
		t.Fatalf("NULL-cache binding: %v", err)
	}
	uploadsHTTPProblem(t, h.request("GET", "/api/files?id="+photo, h.bearer, nil, nil), 404, "not_found")
	uploadsHTTPProblem(t, h.request("DELETE", "/api/files?id="+photo, h.bearer, nil, nil), 404, "not_found")
}

func TestFilesChangeSnapshotsRejectUnresolvedTarget(t *testing.T) {
	db := changeSnapshotDatabase(t)
	fx := newChangeSnapshotFixture(t, db)
	folder := fx.seed(t, "Photos", true)
	fx.seed(t, "Photos/2024", true)
	photo := fx.seed(t, "Photos/2024/x.jpg", false)
	snapshot := fx.create(t, filesChangeFilter{Target: fc.EntrySelector{ID: photo}, Kind: "file"})

	// Break the chain after the baseline exists: the snapshot page must not keep serving a
	// target whose current location cannot be verified.
	if _, err := db.Pool().Exec(context.Background(), `delete from nodes where owner_id=$1::uuid and id=$2::uuid`, fx.f.Principal.User.ID, folder); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(filesChangeFilter{Target: fc.EntrySelector{ID: photo}, Kind: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fx.call(t, http.MethodPost, "", "", string(body)); err == nil || filesProblem(err).Status != 404 || filesProblem(err).Code != "not_found" {
		t.Fatalf("snapshot target with broken ancestry: %v", err)
	}
	if _, err = fx.call(t, http.MethodGet, snapshot.ID, "", ""); err == nil || filesProblem(err).Status != 409 || filesProblem(err).Code != "target_changed" {
		t.Fatalf("saved snapshot target with broken ancestry: %v", err)
	}
}

func TestFilesChangesRejectUnresolvedWatchTarget(t *testing.T) {
	fixture := newFileWorkerFixture(t)
	s, f, c := filesChangesPGOwner(t, fixture)
	var fileID string
	if err := fixture.db.Pool().QueryRow(fixture.ctx, `insert into nodes(owner_id,key,path,parent,name,is_dir,size,mime)
		values($1::uuid,$2,'Scope/a','Scope','a',false,3,'text/plain') returning id::text`,
		f.Principal.User.ID, "drive/"+f.Principal.User.ID+"/Scope/a").Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	c.Filter = filesChangeFilter{Target: fc.EntrySelector{ID: fileID}, Kind: "file"}
	c.Anchor = "/Scope/a"
	if _, err := s.filesReadChanges(fixture.ctx, f, c, 10); err != nil {
		t.Fatalf("watch target did not resolve before the break: %v", err)
	}
	if _, err := fixture.db.Pool().Exec(fixture.ctx, `delete from nodes where owner_id=$1::uuid and path='Scope' and is_dir`, f.Principal.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.filesReadChanges(fixture.ctx, f, c, 10); err == nil || filesProblem(err).Status != 409 || filesProblem(err).Code != "watch_access_lost" {
		t.Fatalf("watch target with broken ancestry: %v", err)
	}
}
