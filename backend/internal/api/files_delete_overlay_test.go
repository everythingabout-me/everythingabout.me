package api

import (
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
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
