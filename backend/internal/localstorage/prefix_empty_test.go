package localstorage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestPrefixEmptyReadsTheDirectory(t *testing.T) {
	f, cfg := fixture(t, Options{})
	driver := bind(t, f, cfg)
	writeFixture(t, cfg.ContentRoot, "full/a.txt", "x")
	if err := os.MkdirAll(filepath.Join(cfg.ContentRoot, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	probe, ok := driver.(storage.PrefixProbe)
	if !ok {
		t.Fatal("the local driver does not answer prefix emptiness")
	}
	for _, test := range []struct {
		name string
		path storage.Path
		want bool
	}{
		{"a folder with a child", "full", false},
		{"an empty folder", "empty", true},
		{"a folder that does not exist", "missing", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			empty, err := probe.PrefixEmpty(context.Background(), test.path)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if empty != test.want {
				t.Fatalf("PrefixEmpty(%q) = %v, want %v", test.path, empty, test.want)
			}
		})
	}
}

// The admitted mutation driver is the type the coordinator actually receives, so
// the probe has to survive that wrapper.
func TestMutationDriverPrefixEmpty(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	writeFixture(t, cfg.ContentRoot, "full/a.txt", "x")
	empty, err := w.PrefixEmpty(context.Background(), "full")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if empty {
		t.Fatal("a folder with a child answered empty")
	}
	empty, err = w.PrefixEmpty(context.Background(), "missing")
	if err != nil || !empty {
		t.Fatalf("missing folder = %v, %v; want empty", empty, err)
	}
}
