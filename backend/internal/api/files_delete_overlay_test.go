package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/store"
	"everythingabout.me/backend/internal/testpg"
)

func TestPendingDeleteDriverHidesNormalizedSubtree(t *testing.T) {
	base := &filesReadTestDriver{entries: map[storage.Path]storage.Entry{
		"café":       {Path: "café", Kind: storage.Folder},
		"café/a":     {Path: "café/a", Kind: storage.File},
		"café-other": {Path: "café-other", Kind: storage.File},
	}, descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: true}}}
	d := &pendingDeleteDriver{Driver: base, roots: []string{"café"}}
	ctx := context.Background()
	for _, p := range []storage.Path{"café", "cafe\u0301/a"} {
		if _, err := d.Stat(ctx, p); !storage.IsCode(err, "not_found") {
			t.Fatalf("Stat(%q): %v", p, err)
		}
		if _, err := d.Open(ctx, p, storage.ReadOptions{}); !storage.IsCode(err, "not_found") {
			t.Fatalf("Open(%q): %v", p, err)
		}
		if _, err := d.List(ctx, p, storage.PageRequest{Limit: 10}); !storage.IsCode(err, "not_found") {
			t.Fatalf("List(%q): %v", p, err)
		}
	}
	if base.statCalls != 0 || base.listCalls != 0 {
		t.Fatal("hidden paths reached the native tree")
	}
	if _, err := d.Stat(ctx, "café-other"); err != nil {
		t.Fatalf("similarly named sibling was hidden: %v", err)
	}
}

func TestPendingDeleteDriverConsumesHiddenPagesAndPreservesCursor(t *testing.T) {
	base := &filesReadTestDriver{listOverride: func(_ storage.Path, request storage.PageRequest) (storage.Page, error) {
		if request.Limit != 7 {
			t.Fatalf("native page limit changed: %d", request.Limit)
		}
		switch request.Cursor {
		case "":
			return storage.Page{Entries: []storage.Entry{{Path: "gone"}}, NextCursor: "native-1"}, nil
		case "native-1":
			return storage.Page{Entries: []storage.Entry{{Path: "gone/a"}, {Path: "live"}}, NextCursor: "native-2"}, nil
		case "native-2":
			return storage.Page{Entries: []storage.Entry{{Path: "gone/b"}}}, nil
		default:
			t.Fatalf("unexpected native cursor: %q", request.Cursor)
			return storage.Page{}, nil
		}
	}}
	d := &pendingDeleteDriver{Driver: base, roots: []string{"gone"}}
	page, err := d.List(context.Background(), "", storage.PageRequest{Limit: 7})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Path != "live" || page.NextCursor != "native-2" || base.listCalls != 2 {
		t.Fatalf("filtered page lost native cursor: %#v, %v, calls=%d", page, err, base.listCalls)
	}
	page, err = d.List(context.Background(), "", storage.PageRequest{Limit: 7, Cursor: page.NextCursor})
	if err != nil || len(page.Entries) != 0 || page.NextCursor != "" {
		t.Fatalf("hidden last page: %#v, %v", page, err)
	}
}

func TestPendingDeleteDriverRejectsHiddenPageCursorCycle(t *testing.T) {
	base := &filesReadTestDriver{listOverride: func(_ storage.Path, _ storage.PageRequest) (storage.Page, error) {
		return storage.Page{Entries: []storage.Entry{{Path: "gone"}}, NextCursor: "cycle"}, nil
	}}
	d := &pendingDeleteDriver{Driver: base, roots: []string{"gone"}}
	if _, err := d.List(context.Background(), "", storage.PageRequest{Limit: 10}); !storage.IsCode(err, "invalid_cursor") {
		t.Fatalf("hidden pagination cycle was accepted: %v", err)
	}
}

// A delete is final for the user the moment it is accepted: the index and native
// views must conceal it until the native purge settles, and reveal it again only when
// the operation ends without deleting anything.
func TestFilesDeleteOverlayHidesAcceptedDeletesPostgres(t *testing.T) {
	scratch := testpg.CreateDatabase(t, "eam_delete_overlay_")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := store.Open(ctx, scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	migrations, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(migrations) == 0 {
		t.Fatal("migrations unavailable", err)
	}
	for _, migration := range migrations {
		data, err := os.ReadFile(migration)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, string(data)); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{cfg: &config.Config{}, db: db}
	var owner, other string
	for i, target := range []*string{&owner, &other} {
		if err = db.Pool().QueryRow(ctx, `insert into users(email,password_hash) values($1,'') returning id::text`, fmt.Sprintf("overlay-%d@example.test", i)).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	namespace := owner + ":conn-1:7:content:system"
	insert := func(ownerID, state, path string, generation int) {
		t.Helper()
		// Marshal the real operation shape: the concealment query reads these JSON keys.
		snapshot, err := json.Marshal(fileops.Operation{
			State: fileops.State(state),
			Kind:  "delete",
			Root: fileops.Root{
				OwnerID: ownerID, ConnectionID: "conn-1", Generation: int64(generation),
				ContentRoot: "content", SystemRoot: "system",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var id string
		if err = db.Pool().QueryRow(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot)
			values(gen_random_uuid(),$1::uuid,$2,'delete',$3,$4::jsonb)
			returning id::text`, ownerID, "owner:"+ownerID, "fp-"+path, snapshot).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Pool().Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree)
			values($1::uuid,$2::uuid,0,$3,$3,true)`, id, ownerID, path); err != nil {
			t.Fatal(err)
		}
	}
	insert(owner, "running", "Photos/old.jpg", 7)
	insert(owner, "queued", "Photos/trip", 7)
	insert(owner, "outcome_unknown", "cafe\u0301", 7)
	insert(owner, "failed", "restored.txt", 7)
	insert(owner, "succeeded", "settled.txt", 7)
	insert(owner, "running", "other-generation.txt", 8)
	insert(other, "running", "other-owner.txt", 7)
	driver := &filesReadTestDriver{descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: true}}}
	wrapped, err := server.filesDeleteOverlay(ctx, owner, namespace, driver)
	if err != nil {
		t.Fatal(err)
	}
	roots := filesHiddenRoots(&fileRequest{Driver: wrapped})
	want := []string{"Photos/old.jpg", "Photos/trip", "café"}
	if len(roots) != len(want) {
		t.Fatalf("hidden roots = %q, want %q", roots, want)
	}
	for _, root := range want {
		found := false
		for _, got := range roots {
			if got == root {
				found = true
			}
		}
		if !found {
			t.Fatalf("hidden roots = %q, missing %q", roots, root)
		}
	}
	// A failure before the native effect keeps the entry: the path becomes visible again.
	if _, err = db.Pool().Exec(ctx, `update file_operations set snapshot=jsonb_set(snapshot,'{State}','"failed"') where snapshot->>'State'='queued'`); err != nil {
		t.Fatal(err)
	}
	wrapped, err = server.filesDeleteOverlay(ctx, owner, namespace, driver)
	if err != nil {
		t.Fatal(err)
	}
	if roots = filesHiddenRoots(&fileRequest{Driver: wrapped}); len(roots) != 2 {
		t.Fatalf("terminal delete still hidden: %q", roots)
	}
}

// referenceDeleteDriver exposes the reference-read capability, so the overlay wraps it
// one level deeper. Index reads and reference reads must both still see the hidden roots.
type referenceDeleteDriver struct{ *filesReadTestDriver }

func (d *referenceDeleteDriver) StatReference(ctx context.Context, p storage.Path, _ []byte) (storage.Entry, error) {
	return d.Stat(ctx, p)
}
func (d *referenceDeleteDriver) OpenReference(ctx context.Context, p storage.Path, _ []byte, opts storage.ReadOptions) (storage.Read, error) {
	return d.Open(ctx, p, opts)
}

func TestPendingDeleteReferenceOverlayKeepsHiddenRootsVisibleToIndex(t *testing.T) {
	base := &referenceDeleteDriver{filesReadTestDriver: &filesReadTestDriver{entries: map[storage.Path]storage.Entry{
		"gone":     {Path: "gone", Kind: storage.Folder},
		"gone/a":   {Path: "gone/a", Kind: storage.File},
		"live.txt": {Path: "live.txt", Kind: storage.File},
	}, descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: true}}}}
	overlay := &pendingDeleteReferenceDriver{pendingDeleteDriver: &pendingDeleteDriver{Driver: base, roots: []string{"gone"}}}
	f := &fileRequest{Driver: overlay}
	if roots := filesHiddenRoots(f); len(roots) != 1 || roots[0] != "gone" {
		t.Fatalf("reference overlay hid nothing from the index: %q", roots)
	}
	if _, err := overlay.StatReference(context.Background(), "gone/a", []byte("ref")); !storage.IsCode(err, "not_found") {
		t.Fatalf("reference read returned a pending deletion: %v", err)
	}
	if _, err := overlay.OpenReference(context.Background(), "gone/a", []byte("ref"), storage.ReadOptions{}); !storage.IsCode(err, "not_found") {
		t.Fatalf("reference content returned a pending deletion: %v", err)
	}
	if base.statCalls != 0 || base.openCalls != 0 {
		t.Fatal("hidden reference path reached the native tree")
	}
}

// overlayNativeDriver satisfies the native action set without a real tree; this test
// only asks whether the capability survives the concealment view.
type overlayNativeDriver struct{ fileops.NativeActions }

// A delete still in flight must hide its own path and nothing else. While the
// overlay removed the native capability, every write in the whole drive answered
// 422 unsupported_option — creating a folder in an unrelated directory failed
// because an old delete somewhere else had not settled yet.
func TestPendingDeleteOverlayKeepsNativeWriteCapability(t *testing.T) {
	base := &overlayNativeDriver{}
	driver := &pendingDeleteDriver{Driver: base, roots: []string{"gone"}}
	if _, ok := filesNativeDriver(&fileRequest{Driver: driver}); !ok {
		t.Fatal("the plain concealment view dropped the native write capability")
	}
	reference := &pendingDeleteReferenceDriver{pendingDeleteDriver: driver}
	if _, ok := filesNativeDriver(&fileRequest{Driver: reference}); !ok {
		t.Fatal("the reference concealment view dropped the native write capability")
	}
	if _, ok := filesNativeDriver(&fileRequest{Driver: base}); !ok {
		t.Fatal("the unwrapped driver lost the native write capability")
	}
}
