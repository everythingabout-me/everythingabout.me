package fileops

import (
	"context"
	"errors"
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

// A capture is bounded by entries, never by round trips: one year/month tree of 1,017
// folders and 4,096 entries was refused by a flat 1,000-page budget, which made the
// recursive delete of it fail, retry, and fail again forever. Two thousand sibling
// folders are a legal tree and must capture in full.
func TestSnapshotActionTreeCapturesManyFolders(t *testing.T) {
	native := newWalkNative()
	const folders = 2000
	for i := 0; i < folders; i++ {
		native.addFolder("", fmt.Sprintf("f%04d", i))
	}
	entries, err := snapshotActionTree(context.Background(), native, "", true)
	if err != nil {
		t.Fatalf("capture of %d sibling folders: %v", folders, err)
	}
	if want := folders + 1; len(entries) != want {
		t.Fatalf("captured %d entries, want %d (the root and every folder)", len(entries), want)
	}
}

// flatNative answers one folder with `total` generated children, 1,000 per page, so a
// capture can be driven past the entry limit without holding the tree in memory.
type flatNative struct {
	walkNative
	total int
}

func (f *flatNative) List(_ context.Context, p storage.Path, q storage.PageRequest) (storage.Page, error) {
	if p != "" {
		return storage.Page{}, nil
	}
	offset := 0
	if q.Cursor != "" {
		if _, err := fmt.Sscanf(q.Cursor, "%d", &offset); err != nil {
			return storage.Page{}, fmt.Errorf("test cursor %q: %w", q.Cursor, err)
		}
	}
	end := min(offset+1000, f.total)
	page := storage.Page{}
	for i := offset; i < end; i++ {
		page.Entries = append(page.Entries, storage.Entry{Path: storage.Path(fmt.Sprintf("n%06d", i)), Kind: storage.File, Size: 1, Revision: []byte("rev")})
	}
	if end < f.total {
		page.NextCursor = fmt.Sprint(end)
	}
	return page, nil
}

// The entry limit is the bound that remains, and it must still fire.
func TestSnapshotActionTreeBoundsEntries(t *testing.T) {
	native := &flatNative{walkNative: *newWalkNative(), total: maxActionEntries + 1}
	_, err := snapshotActionTree(context.Background(), native, "", true)
	if err == nil || !IsCode(err, "request_too_large") {
		t.Fatalf("capture past the entry limit: %v", err)
	}
	// The refusal must say what it refused: the code alone sent the last person who hit
	// it looking for an oversized request body. The code is what the client sees; the
	// cause is what the log and the operation problem carry.
	var cause error
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		cause = unwrapped
	}
	if cause == nil || !strings.Contains(cause.Error(), "100000") {
		t.Fatalf("entry-limit refusal does not name the limit: %v", err)
	}
}

// pagingNative hands back an empty page that still asks for the next one. No legitimate
// driver does that, and treating it as "nothing more" would silently truncate a capture.
type pagingNative struct{ walkNative }

func (p *pagingNative) List(_ context.Context, _ storage.Path, q storage.PageRequest) (storage.Page, error) {
	return storage.Page{NextCursor: q.Cursor + "x"}, nil
}

func TestSnapshotActionTreeRefusesPagingWithoutProgress(t *testing.T) {
	native := &pagingNative{walkNative: *newWalkNative()}
	_, err := snapshotActionTree(context.Background(), native, "", true)
	if err == nil || !IsCode(err, "invalid_native_result") {
		t.Fatalf("paging without progress: %v", err)
	}
}

// countingWalk records what one capture and its consistency re-read actually ask the
// provider for, one call at a time.
type countingWalk struct {
	*walkNative
	stats, lists int
}

func (c *countingWalk) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	c.stats++
	return c.walkNative.Stat(ctx, p)
}
func (c *countingWalk) List(ctx context.Context, p storage.Path, q storage.PageRequest) (storage.Page, error) {
	c.lists++
	return c.walkNative.List(ctx, p, q)
}

func countingTree(t *testing.T, files int) *countingWalk {
	t.Helper()
	native := &countingWalk{walkNative: newWalkNative()}
	for f := 0; f < 3; f++ {
		folder := native.addFolder("", fmt.Sprintf("f%d", f))
		for i := 0; i < files; i++ {
			p := storage.Path(string(folder) + fmt.Sprintf("/n%d", i))
			native.tree[folder] = append(native.tree[folder], storage.Entry{Path: p, Kind: storage.File, Size: 10, Revision: []byte("native-etag:\"e" + string(p) + "\"")})
		}
	}
	return native
}

// The consistency re-read already compares every captured source against a fresh walk,
// and that walk carries each entry's ETag. A per-source Stat on top of it is a second
// pass over the same subtree answering the same question: one HEAD per entry of every
// recursive delete, which is what made a 103-entry delete take 24 seconds to answer.
func TestValidateCaptureReReadsTheWalkWithoutStattingEverySource(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		mutate  func(*countingWalk)
		wantErr string
	}{
		{name: "unchanged", mutate: func(*countingWalk) {}},
		{
			name: "one file changed",
			mutate: func(native *countingWalk) {
				folder := storage.Path("f1")
				native.tree[folder][2] = storage.Entry{Path: native.tree[folder][2].Path, Kind: storage.File, Size: 10, Revision: []byte(`native-etag:"rewritten"`)}
			},
			wantErr: "target_changed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			native := countingTree(t, 4)
			req := ActionRequest{Kind: "delete", From: "", Recursive: true, Root: Root{CaseSensitive: true}, Actor: Actor{ID: "owner:t", CanWrite: true}}
			binding, manifest, _, err := captureAction(ctx, req, native)
			if err != nil {
				t.Fatal(err)
			}
			native.stats, native.lists = 0, 0
			test.mutate(native)
			err = validateCapture(ctx, req, native, binding, manifest)
			if test.wantErr != "" {
				if !IsCode(err, test.wantErr) {
					t.Fatalf("err=%v want %s", err, test.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// One Stat for the walk's own root, plus the captured anchor chain. A Stat
			// per source is what this test exists to keep out of the re-read.
			if limit := len(binding.Anchors) + 1; native.stats > limit {
				t.Fatalf("re-read issued %d Stats, want at most %d (anchors plus the walk root)", native.stats, limit)
			}
			if native.lists == 0 {
				t.Fatal("re-read did not walk the tree")
			}
		})
	}
}
