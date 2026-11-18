package objecttree

import (
	"context"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// probeBackend answers one delimiter listing of whatever prefix it is asked for.
// The prefix it recorded is part of the contract: without the trailing slash the
// probe would answer for a sibling whose name merely starts the same way.
type probeBackend struct {
	calls   int
	prefix  string
	objects []Object
}

func (b *probeBackend) Head(context.Context, string) (Object, error) { panic("unexpected HEAD") }
func (b *probeBackend) Get(context.Context, string, string, string) (Response, error) {
	panic("unexpected GET")
}
func (b *probeBackend) List(_ context.Context, prefix, _ string) (NativePage, error) {
	b.calls++
	b.prefix = prefix
	page := NativePage{}
	for _, object := range b.objects {
		if strings.HasPrefix(object.Key, prefix) {
			page.Objects = append(page.Objects, object)
		}
	}
	return page, nil
}

func TestPrefixEmptyIsOneListingOfTheFolderPrefix(t *testing.T) {
	for _, test := range []struct {
		name string
		keys []string
		want bool
	}{
		{"nothing", nil, true},
		{"a sibling that starts the same way", []string{"drive/alice/tofu/x"}, true},
		{"a child object", []string{"drive/alice/to/a.txt"}, false},
		{"the folder marker itself", []string{"drive/alice/to/"}, false},
		{"a child of an empty-looking folder", []string{"drive/alice/to/deep/x"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &probeBackend{}
			for _, key := range test.keys {
				backend.objects = append(backend.objects, Object{Key: key, Size: 1})
			}
			tree, err := Bind(backend, storage.Config{OwnerID: "alice", ConnectionID: "test", Generation: 1, ContentRoot: "drive/alice", SystemRoot: "system/alice"}, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			empty, err := tree.PrefixEmpty(context.Background(), "to")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if empty != test.want {
				t.Fatalf("PrefixEmpty = %v, want %v", empty, test.want)
			}
			if backend.calls != 1 {
				t.Fatalf("probe made %d listing(s), want 1", backend.calls)
			}
			if backend.prefix != "drive/alice/to/" {
				t.Fatalf("probe listed %q, want the folder prefix %q", backend.prefix, "drive/alice/to/")
			}
		})
	}
}

// The tree root already ends in a slash, so the probe must not add a second one.
func TestPrefixEmptyAtTheTreeRoot(t *testing.T) {
	backend := &probeBackend{objects: []Object{{Key: "drive/alice/a.txt", Size: 1}}}
	tree, err := Bind(backend, storage.Config{OwnerID: "alice", ConnectionID: "test", Generation: 1, ContentRoot: "drive/alice", SystemRoot: "system/alice"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := tree.PrefixEmpty(context.Background(), "")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if empty {
		t.Fatal("the tree root answered empty while it holds an object")
	}
	if backend.prefix != "drive/alice/" {
		t.Fatalf("probe listed %q, want %q", backend.prefix, "drive/alice/")
	}
}
