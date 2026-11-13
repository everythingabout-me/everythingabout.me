package objecttree

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// costBackend is a counting native object store: it reports exactly how many
// HEAD, listing and GET requests one observation issued, and how much of a
// listing response a request consumed. It is not a performance model; the counts
// are the wire work the adapter asks for.
type costBackend struct {
	objects map[string]int64
	heads   int
	lists   int
	gets    int
	listed  int   // entries carried by listing responses
	payload int64 // listing response bytes: keys plus one line of metadata each
	page    int
}

func newCostBackend() *costBackend { return &costBackend{objects: map[string]int64{}, page: 1000} }

func (b *costBackend) put(key string, size int64) { b.objects[key] = size }

func (b *costBackend) reset() { b.heads, b.lists, b.gets, b.listed, b.payload = 0, 0, 0, 0, 0 }

func (b *costBackend) Head(_ context.Context, key string) (Object, error) {
	b.heads++
	if size, ok := b.objects[key]; ok {
		return Object{Key: key, Size: size, ETag: fmt.Sprintf("%q", fmt.Sprintf("etag-%d", size))}, nil
	}
	return Object{}, fail("not_found", "")
}

// List serves one page of a delimiter listing, like S3 ListObjectsV2 or the COS
// bucket listing: immediate objects plus common prefixes, paginated by marker.
func (b *costBackend) List(_ context.Context, prefix, cursor string) (NativePage, error) {
	b.lists++
	items := map[string]bool{}
	for key := range b.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		relative := strings.TrimPrefix(key, prefix)
		if at := strings.Index(relative, "/"); at >= 0 {
			items[key[:len(prefix)+at+1]] = true
		} else {
			items[key] = true
		}
	}
	ordered := make([]string, 0, len(items))
	for item := range items {
		ordered = append(ordered, item)
	}
	sort.Strings(ordered)
	start := sort.SearchStrings(ordered, cursor)
	if start < len(ordered) && ordered[start] == cursor {
		start++
	}
	page := NativePage{}
	end := min(start+b.page, len(ordered))
	for _, item := range ordered[start:end] {
		if size, ok := b.objects[item]; ok {
			page.Objects = append(page.Objects, Object{Key: item, Size: size, ETag: fmt.Sprintf("%q", fmt.Sprintf("etag-%d", size))})
		} else {
			page.Prefixes = append(page.Prefixes, item)
		}
		b.listed++
		b.payload += int64(len(item)) + 64
	}
	if end < len(ordered) {
		page.Truncated = true
		page.Next = ordered[end-1]
	}
	return page, nil
}

// Get answers one full or ranged transfer. Open's request cost is what this
// measures, not its bytes; the adapter validates length and range itself.
func (b *costBackend) Get(_ context.Context, key, byteRange, _ string) (Response, error) {
	b.gets++
	size, ok := b.objects[key]
	if !ok {
		return Response{}, fail("not_found", "")
	}
	start, end := int64(0), size-1
	status := 200
	contentRange := ""
	if byteRange != "" {
		status = 206
		var last string
		if _, err := fmt.Sscanf(byteRange, "bytes=%d-%s", &start, &last); err != nil {
			return Response{}, fail("invalid_range", "")
		}
		if last != "" {
			if _, err := fmt.Sscanf(last, "%d", &end); err != nil {
				return Response{}, fail("invalid_range", "")
			}
		}
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	}
	length := end - start + 1
	return Response{Object: Object{Key: key, Size: length, ETag: fmt.Sprintf("%q", fmt.Sprintf("etag-%d", size))}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", int(length)))), Status: status, ContentRange: contentRange, Length: length}, nil
}

// observeCost runs one observation and returns the wire work it caused.
type observeCost struct {
	calls    int
	lists    int
	entries  int
	response int64
}

func (b *costBackend) costOf(run func()) observeCost {
	b.reset()
	run()
	return observeCost{calls: b.heads + b.lists + b.gets, lists: b.lists, entries: b.listed, response: b.payload}
}

func newCostTree(t *testing.T, paths []string) (*Tree, *costBackend) {
	t.Helper()
	backend := newCostBackend()
	for _, path := range paths {
		backend.put(path, 7)
	}
	backend.put("system/alice/", 0)
	tree, err := Bind(backend, storage.Config{OwnerID: "alice", ConnectionID: "cost", Generation: 1, ContentRoot: "drive/alice", SystemRoot: "system/alice"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return tree, backend
}

// TestStatObservationCost reports what one path observation costs natively. The
// numbers are the point: a parent listing makes each Stat scale with its sibling
// count, while a byte-exact single-object observation does not.
func TestStatObservationCost(t *testing.T) {
	ctx := context.Background()
	scenarios := []struct {
		name     string
		dir      string
		siblings int
	}{
		{"one_sibling", "one", 1},
		{"hundred_siblings", "hundred", 100},
		{"full_listing_page", "page", 1000},
		{"five_listing_pages", "fivepages", 5000},
	}
	for _, scenario := range scenarios {
		paths := []string{"drive/alice/" + scenario.dir + "/target"}
		for i := 0; i < scenario.siblings; i++ {
			paths = append(paths, fmt.Sprintf("drive/alice/%s/sibling-%05d", scenario.dir, i))
		}
		paths = append(paths, "drive/alice/marked/", "drive/alice/marked/child", "drive/alice/prefix/child")
		tree, backend := newCostTree(t, paths)
		dir := "drive/alice/" + scenario.dir

		file := backend.costOf(func() {
			if _, err := tree.Stat(ctx, storage.Path(scenario.dir+"/target")); err != nil {
				t.Error(err)
			}
		})
		missing := backend.costOf(func() {
			if _, err := tree.Stat(ctx, storage.Path(scenario.dir+"/absent")); !storage.IsCode(err, "not_found") {
				t.Error(err)
			}
		})
		marked := backend.costOf(func() {
			if _, err := tree.Stat(ctx, "marked"); err != nil {
				t.Error(err)
			}
		})
		prefix := backend.costOf(func() {
			if _, err := tree.Stat(ctx, "prefix"); err != nil {
				t.Error(err)
			}
		})
		list := backend.costOf(func() {
			if _, err := tree.List(ctx, storage.Path(scenario.dir), storage.PageRequest{Limit: 100}); err != nil {
				t.Error(err)
			}
		})
		open := backend.costOf(func() {
			read, err := tree.Open(ctx, storage.Path(scenario.dir+"/target"), storage.ReadOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			read.Body.Close()
		})
		statRef := backend.costOf(func() {
			if _, err := tree.StatReference(ctx, storage.Path(scenario.dir+"/target"), []byte(dir+"/target")); err != nil {
				t.Error(err)
			}
		})
		openRef := backend.costOf(func() {
			read, err := tree.OpenReference(ctx, storage.Path(scenario.dir+"/target"), []byte(dir+"/target"), storage.ReadOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			read.Body.Close()
		})
		t.Logf("siblings=%-5d Stat(file)=%dreq/%dlist/%dent/%dB  Stat(missing)=%dreq/%dlist  Stat(marked folder)=%dreq/%dlist  Stat(prefix folder)=%dreq/%dlist  List=%dreq/%dlist/%dent  Open=%dreq/%dlist  StatReference=%dreq  OpenReference=%dreq",
			scenario.siblings, file.calls, file.lists, file.entries, file.response, missing.calls, missing.lists, marked.calls, marked.lists, prefix.calls, prefix.lists, list.calls, list.lists, list.entries, open.calls, open.lists, statRef.calls, openRef.calls)
	}
}

// TestPathDepthObservationCost reports the same counts for paths of increasing
// depth, because a per-segment walk multiplies every read by the tree's depth.
func TestPathDepthObservationCost(t *testing.T) {
	ctx := context.Background()
	for _, depth := range []int{1, 3, 6} {
		var path string
		for i := 0; i < depth; i++ {
			path += fmt.Sprintf("level-%d/", i)
		}
		paths := []string{"drive/alice/" + path + "target"}
		tree, backend := newCostTree(t, paths)
		stat := backend.costOf(func() {
			if _, err := tree.Stat(ctx, storage.Path(path+"target")); err != nil {
				t.Error(err)
			}
		})
		open := backend.costOf(func() {
			read, err := tree.Open(ctx, storage.Path(path+"target"), storage.ReadOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			read.Body.Close()
		})
		t.Logf("depth=%d Stat(file)=%dreq/%dlist  Open(file)=%dreq/%dlist", depth, stat.calls, stat.lists, open.calls, open.lists)
	}
}

// TestPathObservationDoesNotScaleWithTheDirectory pins the cost that the parent
// listing used to impose: observing one path is one byte-exact probe (or two, or
// the bounded prefix probe), whatever the directory holds and however deep the
// path is. This is the regression guard for the measurement above.
func TestPathObservationDoesNotScaleWithTheDirectory(t *testing.T) {
	ctx := context.Background()
	for _, siblings := range []int{1, 1000, 5000} {
		for _, depth := range []int{1, 6} {
			var dir string
			for i := 0; i < depth; i++ {
				dir += fmt.Sprintf("level-%d/", i)
			}
			paths := []string{"drive/alice/" + dir + "target"}
			for i := 0; i < siblings; i++ {
				paths = append(paths, fmt.Sprintf("drive/alice/%ssibling-%05d", dir, i))
			}
			tree, backend := newCostTree(t, paths)
			stat := backend.costOf(func() {
				if _, err := tree.Stat(ctx, storage.Path(dir+"target")); err != nil {
					t.Fatal(err)
				}
			})
			open := backend.costOf(func() {
				read, err := tree.Open(ctx, storage.Path(dir+"target"), storage.ReadOptions{})
				if err != nil {
					t.Fatal(err)
				}
				read.Body.Close()
			})
			if stat.calls != 1 || stat.lists != 0 || stat.entries != 0 || stat.response != 0 {
				t.Fatalf("siblings=%d depth=%d: Stat(file) = %+v, want one HEAD and nothing else", siblings, depth, stat)
			}
			if open.calls != 2 || open.lists != 0 {
				t.Fatalf("siblings=%d depth=%d: Open(file) = %+v, want one HEAD and one GET", siblings, depth, open)
			}
		}
	}
	// A prefix folder has no object of its own, so its existence needs one listing
	// page; it must not need a second page or a parent listing.
	tree, backend := newCostTree(t, []string{"drive/alice/prefix/child"})
	probe := backend.costOf(func() {
		if _, err := tree.Stat(ctx, "prefix"); err != nil {
			t.Fatal(err)
		}
	})
	if probe.calls != 3 || probe.lists != 1 {
		t.Fatalf("Stat(prefix folder) = %+v, want two absent probes and one bounded listing", probe)
	}
	// One folder listing is its own prefix listing; it used to be this plus a
	// listing of its parent.
	tree, backend = newCostTree(t, []string{"drive/alice/folder/child"})
	list := backend.costOf(func() {
		if _, err := tree.List(ctx, "folder", storage.PageRequest{Limit: 100}); err != nil {
			t.Fatal(err)
		}
	})
	if list.calls != 1 || list.lists != 1 {
		t.Fatalf("List(folder) = %+v, want one listing", list)
	}
}
