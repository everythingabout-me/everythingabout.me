package treescan_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/objecttree"
	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
)

func scanPaths(t *testing.T, driver storage.Driver) []string {
	t.Helper()
	result, err := treescan.Scan(context.Background(), driver, treescan.Options{PageSize: 1, Names: driver.Describe().Names})
	if err != nil {
		t.Fatal(err)
	}
	records, err := result.Records()
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range records {
		paths = append(paths, string(r.Entry.Path))
	}
	return paths
}

func TestScanLocalNativeTreeWithoutDatabase(t *testing.T) {
	base := t.TempDir()
	content := filepath.Join(base, "content")
	system := filepath.Join(base, "system")
	other := filepath.Join(base, "other-owner")
	for _, dir := range []string{content, system, other, filepath.Join(content, " folder "), filepath.Join(content, "empty")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	name := " folder /raw +#?%.txt "
	data := []byte{0, 1, 2, '\n', 255}
	if err := os.WriteFile(filepath.Join(content, filepath.FromSlash(name)), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "private"), []byte("other owner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(system, "private"), []byte("system state"), 0600); err != nil {
		t.Fatal(err)
	}
	factory, err := localstorage.NewFactory(localstorage.Options{})
	if storage.IsCode(err, "unsupported") {
		t.Skip("safe local native reads are unsupported on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), storage.Config{OwnerID: "owner", ConnectionID: "local-test", Generation: 1, ContentRoot: content, SystemRoot: system})
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	want := []string{" folder ", name, "empty"}
	if got := scanPaths(t, driver); !reflect.DeepEqual(got, want) {
		t.Fatalf("local scan changed names or crossed roots: got %q want %q", got, want)
	}
	if got := scanPaths(t, driver); !reflect.DeepEqual(got, want) {
		t.Fatalf("repeat local scan differs: %q", got)
	}
	actual, err := os.ReadFile(filepath.Join(content, filepath.FromSlash(name)))
	if err != nil || !reflect.DeepEqual(actual, data) {
		t.Fatalf("native content changed: %v %v", actual, err)
	}
}

type nativeInventory struct{}

func (nativeInventory) Head(context.Context, string) (objecttree.Object, error) {
	panic("scan must not HEAD file bytes")
}
func (nativeInventory) Get(context.Context, string, string, string) (objecttree.Response, error) {
	panic("scan must not read file bytes")
}
func (nativeInventory) List(_ context.Context, prefix, cursor string) (objecttree.NativePage, error) {
	switch prefix {
	case "drive/owner/":
		if cursor == "" {
			return objecttree.NativePage{Objects: []objecttree.Object{{Key: prefix, Size: 0}}, Prefixes: []string{prefix + "empty/"}, Next: "next", Truncated: true}, nil
		}
		if cursor == "next" {
			return objecttree.NativePage{Objects: []objecttree.Object{{Key: prefix + "plain", Size: 0}, {Key: prefix + "empty/", Size: 0}}, Prefixes: []string{prefix + " folder /"}}, nil
		}
	case "drive/owner/ folder /":
		return objecttree.NativePage{Objects: []objecttree.Object{{Key: prefix + "raw +#?%.txt ", Size: 5}}}, nil
	case "drive/owner/empty/":
		return objecttree.NativePage{}, nil
	}
	panic("scan crossed owner or system root: " + prefix)
}

func TestScanObjectPrefixTreeWithoutDatabase(t *testing.T) {
	secret := make([]byte, 32)
	driver, err := objecttree.Bind(nativeInventory{}, storage.Config{OwnerID: "owner", ConnectionID: "object-test", Generation: 1, ContentRoot: "drive/owner/", SystemRoot: "__eam_system__/owner/"}, secret)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	want := []string{" folder ", " folder /raw +#?%.txt ", "empty", "plain"}
	if got := scanPaths(t, driver); !reflect.DeepEqual(got, want) {
		t.Fatalf("object scan changed names or lost implicit folders: got %q want %q", got, want)
	}
}
