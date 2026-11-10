package objecttree

import (
	"context"
	"fmt"
	"io"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

type limitBackend struct {
	calls    int
	entries  int
	infinite bool
}

func (b *limitBackend) Head(context.Context, string) (Object, error) { panic("unexpected HEAD") }
func (b *limitBackend) Get(context.Context, string, string, string) (Response, error) {
	panic("unexpected GET")
}
func (b *limitBackend) List(_ context.Context, prefix, cursor string) (NativePage, error) {
	b.calls++
	if b.infinite {
		return NativePage{Prefixes: []string{prefix + "folder/"}, Truncated: true, Next: fmt.Sprint(b.calls)}, nil
	}
	page := NativePage{}
	count := b.entries
	if count > 1000 {
		count = 1000
	}
	for i := 0; i < count; i++ {
		page.Objects = append(page.Objects, Object{Key: fmt.Sprintf("%s%09d", prefix, (b.calls-1)*1000+i), Size: 1})
	}
	b.entries -= count
	if b.entries > 0 {
		page.Truncated = true
		page.Next = fmt.Sprint(b.calls)
	}
	return page, nil
}
func TestDirectoryAndNativePageBudgetsFailWithoutPartialResult(t *testing.T) {
	for _, test := range []struct {
		name     string
		backend  *limitBackend
		maxCalls int
	}{{"directory_entries", &limitBackend{entries: maxDirectoryEntries + 1}, maxDirectoryEntries/1000 + 1}, {"advancing_native_cursor", &limitBackend{infinite: true}, maxNativePages}} {
		t.Run(test.name, func(t *testing.T) {
			tree, err := Bind(test.backend, storage.Config{OwnerID: "alice", ConnectionID: "test", Generation: 1, ContentRoot: "drive/alice", SystemRoot: "system/alice"}, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			page, err := tree.List(context.Background(), "", storage.PageRequest{Limit: 1})
			if !storage.IsCode(err, "resource_limit") || len(page.Entries) != 0 || page.NextCursor != "" {
				t.Fatalf("budget returned partial result: %+v %v", page, err)
			}
			if test.backend.calls > test.maxCalls {
				t.Fatalf("native page budget exceeded: %d", test.backend.calls)
			}
		})
	}
}
func TestExactBodyDetectsExtraAndShortNativeBytes(t *testing.T) {
	for _, test := range []struct {
		name, data string
		declared   int64
	}{{"extra", "abc", 2}, {"short", "a", 2}} {
		t.Run(test.name, func(t *testing.T) {
			body := &exactBody{body: io.NopCloser(&stringReader{value: test.data}), remaining: test.declared}
			_, err := io.ReadAll(body)
			if err == nil {
				t.Fatal("incorrect native body length was accepted")
			}
		})
	}
}

type stringReader struct{ value string }

func (r *stringReader) Read(p []byte) (int, error) {
	if r.value == "" {
		return 0, io.EOF
	}
	n := copy(p, r.value)
	r.value = r.value[n:]
	return n, nil
}

// A listing's ETag is native evidence about the entry it belongs to, and it is not
// this tree's Revision: a read driver's revision names a retained version, and a
// caller may hand a revision back as ReadOptions.PinnedVersion. Reporting the ETag as
// a revision would make that round trip fail with "unsupported"; reporting it as
// evidence lets the mutation driver build its own revision without a HEAD per child.
type etagBackend struct{ objects []Object }

func (b *etagBackend) Head(context.Context, string) (Object, error) { panic("unexpected HEAD") }
func (b *etagBackend) Get(context.Context, string, string, string) (Response, error) {
	panic("unexpected GET")
}
func (b *etagBackend) List(_ context.Context, prefix, _ string) (NativePage, error) {
	page := NativePage{}
	for _, object := range b.objects {
		page.Objects = append(page.Objects, object)
	}
	return page, nil
}

func TestListCarriesTheNativeETagAsEvidenceNotAsRevision(t *testing.T) {
	backend := &etagBackend{objects: []Object{{Key: "drive/alice/a.txt", Size: 3, ETag: `"etag-a"`}, {Key: "drive/alice/folder/", Size: 0, ETag: `"etag-folder"`}}}
	tree, err := Bind(backend, storage.Config{OwnerID: "alice", ConnectionID: "test", Generation: 1, ContentRoot: "drive/alice", SystemRoot: "system/alice"}, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	page, err := tree.List(context.Background(), "", storage.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("entries=%+v", page.Entries)
	}
	for _, entry := range page.Entries {
		if len(entry.Revision) != 0 {
			t.Fatalf("%s invented revision %q", entry.Path, entry.Revision)
		}
		if len(entry.NativeETag) == 0 || entry.NativeETag[0] != '"' {
			t.Fatalf("%s lost its native ETag: %q", entry.Path, entry.NativeETag)
		}
	}
}
