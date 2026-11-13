package objectmutation

import (
	"context"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// countingPrefixTree is the read tree the object driver delegates an
// unmarked-prefix-folder observation to. It records how that delegation is used:
// the driver must not re-observe the two keys it has already proved absent, and
// it must not list a directory to observe one path.
type countingPrefixTree struct {
	*fakeTree
	probes int
	lists  int
}

func (t *countingPrefixTree) PrefixFolder(_ context.Context, path storage.Path) (storage.Entry, error) {
	t.probes++
	prefix := t.root + string(path) + "/"
	for key := range t.backend.current {
		if strings.HasPrefix(key, prefix) {
			return storage.Entry{Path: path, Kind: storage.Folder, NativeRef: []byte(t.root + string(path))}, nil
		}
	}
	return storage.Entry{}, fail("not_found", path)
}

func (t *countingPrefixTree) List(ctx context.Context, path storage.Path, request storage.PageRequest) (storage.Page, error) {
	t.lists++
	return t.fakeTree.List(ctx, path, request)
}

// TestPathObservationRequestCost pins what observing one path costs the object
// driver: byte-exact HEADs for both real kinds, and for an unmarked prefix folder
// exactly one delegation to the single-prefix probe. Before that probe existed,
// the driver re-entered the path-addressed Stat, which re-probed both absent keys
// (two more HEADs) and then re-observed the folder with a third.
func TestPathObservationRequestCost(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name    string
		objects map[string]Object
		path    storage.Path
		kind    storage.Kind
		heads   int
		probes  int
	}{
		{"file", map[string]Object{"drive/owner/file": {Key: "drive/owner/file", ETag: `"file"`, Size: 4}}, "file", storage.File, 2, 0},
		{"marked_folder", map[string]Object{"drive/owner/folder/": {Key: "drive/owner/folder/", ETag: `"marker"`}}, "folder", storage.Folder, 1, 0},
		{"prefix_folder", map[string]Object{"drive/owner/prefix/child": {Key: "drive/owner/prefix/child", ETag: `"child"`, Size: 1}}, "prefix", storage.Folder, 2, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newFake()
			for key, object := range test.objects {
				backend.current[key] = object
			}
			cfg := storage.Config{OwnerID: "owner", ConnectionID: "fixture", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "system/owner/"}
			options := Options{SpoolRoot: t.TempDir(), MaxPublishBytes: 1024, Admission: Admission{Version: AdmissionVersion, ConnectionID: cfg.ConnectionID, Endpoint: "https://fixture.invalid", Bucket: "bucket", Region: "region", CheckedAt: time.Now().UTC(), ProbeReference: "isolated-positive-negative-test", AtomicPublish: true, ConditionalCreate: true, SingleGateway: true}}
			tree := &countingPrefixTree{fakeTree: &fakeTree{backend: backend, root: cfg.ContentRoot}}
			driver, err := New(tree, cfg, backend, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { driver.Close() })
			backend.heads = 0
			entry, err := driver.Stat(ctx, test.path)
			if err != nil {
				t.Fatalf("Stat(%q) = %v", test.path, err)
			}
			if entry.Kind != test.kind {
				t.Fatalf("Stat(%q) kind = %s, want %s", test.path, entry.Kind, test.kind)
			}
			if backend.heads != test.heads || tree.probes != test.probes || tree.lists != 0 {
				t.Fatalf("Stat(%q) used %d HEADs, %d prefix probes, %d listings; want %d, %d, 0",
					test.path, backend.heads, tree.probes, tree.lists, test.heads, test.probes)
			}
			if test.kind == storage.Folder && test.name == "prefix_folder" && len(entry.Revision) != 0 {
				t.Fatalf("prefix folder carries revision %q; it has no marker object to condition on", entry.Revision)
			}
		})
	}
}
