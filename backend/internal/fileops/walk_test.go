package fileops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// walkNative serves one in-memory tree to snapshotActionTree: Stat answers an entry's own
// identity and List answers a folder's immediate children.
type walkNative struct {
	NativeActions
	tree map[storage.Path][]storage.Entry
}

func newWalkNative() *walkNative { return &walkNative{tree: map[storage.Path][]storage.Entry{}} }

func (n *walkNative) StorageNameRules() storage.NameRules {
	return storage.NameRules{CaseSensitive: true}
}

func (n *walkNative) Stat(_ context.Context, p storage.Path) (storage.Entry, error) {
	if p == "" {
		return storage.Entry{Path: p, Kind: storage.Folder, Revision: []byte("rev:")}, nil
	}
	for _, children := range n.tree {
		for _, child := range children {
			if child.Path == p {
				return child, nil
			}
		}
	}
	return storage.Entry{}, storage.NewError("not_found", p, nil)
}

func (n *walkNative) List(_ context.Context, p storage.Path, q storage.PageRequest) (storage.Page, error) {
	offset := 0
	if q.Cursor != "" {
		if _, err := fmt.Sscanf(q.Cursor, "%d", &offset); err != nil {
			return storage.Page{}, fmt.Errorf("test cursor %q: %w", q.Cursor, err)
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	children := n.tree[p]
	if offset >= len(children) {
		return storage.Page{}, nil
	}
	end := min(offset+limit, len(children))
	page := storage.Page{Entries: append([]storage.Entry(nil), children[offset:end]...)}
	if end < len(children) {
		page.NextCursor = fmt.Sprint(end)
	}
	return page, nil
}

func (n *walkNative) addFolder(parent storage.Path, name string) storage.Path {
	p := storage.Path(strings.Trim(string(parent)+"/"+name, "/"))
	children := append(n.tree[parent], storage.Entry{Path: p, Kind: storage.Folder, Revision: []byte("rev:" + string(p))})
	sort.Slice(children, func(i, j int) bool { return children[i].Path < children[j].Path })
	n.tree[parent] = children
	if _, ok := n.tree[p]; !ok {
		n.tree[p] = nil
	}
	return p
}

// A wide folder must capture every child exactly once, in the order a sequential walk
// produces: the root, then each level in discovery order with siblings sorted.
func TestSnapshotActionTreeWalksEveryLevelOnce(t *testing.T) {
	native := newWalkNative()
	a := native.addFolder("", "a")
	b := native.addFolder("", "b")
	for i := 0; i < 12; i++ {
		native.addFolder(a, fmt.Sprintf("a%02d", i))
	}
	for i := 0; i < 12; i++ {
		native.addFolder(b, fmt.Sprintf("b%02d", i))
	}

	entries, err := snapshotActionTree(context.Background(), native, "", true)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	want := []storage.Path{"", a, b}
	for i := 0; i < 12; i++ {
		want = append(want, storage.Path(fmt.Sprintf("%s/a%02d", a, i)))
	}
	for i := 0; i < 12; i++ {
		want = append(want, storage.Path(fmt.Sprintf("%s/b%02d", b, i)))
	}
	if len(entries) != len(want) {
		t.Fatalf("captured %d entries, want %d", len(entries), len(want))
	}
	for i, p := range want {
		if entries[i].Path != p {
			t.Fatalf("entry %d is %q, want %q", i, entries[i].Path, p)
		}
	}
}

// A non-recursive capture of a non-empty folder is a refusal, not a truncated walk.
func TestSnapshotActionTreeRefusesNonRecursiveNonEmpty(t *testing.T) {
	native := newWalkNative()
	native.addFolder("", "only")
	if _, err := snapshotActionTree(context.Background(), native, "", false); err == nil || !IsCode(err, "directory_not_empty") {
		t.Fatalf("non-recursive capture of a non-empty tree: %v", err)
	}
}

// The page limit is what bounds a recursive capture; it must still fire now that sibling
// folders are listed concurrently.
func TestSnapshotActionTreeBoundsPages(t *testing.T) {
	native := newWalkNative()
	for i := 0; i < maxActionPages+2; i++ {
		native.addFolder("", fmt.Sprintf("f%04d", i))
	}
	if _, err := snapshotActionTree(context.Background(), native, "", true); err == nil || !IsCode(err, "request_too_large") {
		t.Fatalf("wide capture past the page limit: %v", err)
	}
}
