package snapshot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// fakeTree is one native tree in memory. Entries carry full tree paths, matching
// the driver contract, so the path mapping in Copy is exercised rather than assumed.
type fakeTree struct {
	files   map[storage.Path][]byte
	dirs    map[storage.Path]bool
	staged  map[string][]byte
	mkdirs  []storage.Path
	removed []storage.Path
}

func newFakeTree() *fakeTree {
	return &fakeTree{files: map[storage.Path][]byte{}, dirs: map[storage.Path]bool{}, staged: map[string][]byte{}}
}

func (f *fakeTree) put(p storage.Path, data string) *fakeTree {
	parts := strings.Split(string(p), "/")
	for i := 1; i < len(parts); i++ {
		f.dirs[storage.Path(strings.Join(parts[:i], "/"))] = true
	}
	f.files[p] = []byte(data)
	return f
}

func (f *fakeTree) dir(p storage.Path) *fakeTree { f.dirs[p] = true; return f }

func (f *fakeTree) Stat(_ context.Context, p storage.Path) (storage.Entry, error) {
	if data, ok := f.files[p]; ok {
		return storage.Entry{Path: p, Kind: storage.File, Size: int64(len(data))}, nil
	}
	if f.dirs[p] || p == "" {
		return storage.Entry{Path: p, Kind: storage.Folder}, nil
	}
	return storage.Entry{}, storage.NewError("not_found", p, nil)
}

func (f *fakeTree) Open(_ context.Context, p storage.Path, _ storage.ReadOptions) (storage.Read, error) {
	data, ok := f.files[p]
	if !ok {
		return storage.Read{}, storage.NewError("not_found", p, nil)
	}
	return storage.Read{Body: io.NopCloser(bytes.NewReader(data)), Entry: storage.Entry{Path: p, Kind: storage.File, Size: int64(len(data))}, Length: int64(len(data))}, nil
}

func (f *fakeTree) List(_ context.Context, dir storage.Path, _ storage.PageRequest) (storage.Page, error) {
	seen := map[string]storage.Entry{}
	add := func(p storage.Path, entry storage.Entry) {
		if dir != "" && !strings.HasPrefix(string(p), string(dir)+"/") {
			return
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(string(p), string(dir)), "/")
		if rest == "" {
			return
		}
		name := strings.Split(rest, "/")[0]
		child := storage.Path(name)
		if dir != "" {
			child = storage.Path(string(dir) + "/" + name)
		}
		if actual, ok := f.files[child]; ok {
			entry = storage.Entry{Path: child, Kind: storage.File, Size: int64(len(actual))}
		} else {
			entry = storage.Entry{Path: child, Kind: storage.Folder}
		}
		seen[name] = entry
	}
	for p := range f.files {
		add(p, storage.Entry{})
	}
	for p := range f.dirs {
		add(p, storage.Entry{})
	}
	var out []storage.Entry
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return storage.Page{Entries: out}, nil
}

func (f *fakeTree) Describe() storage.Descriptor { return storage.Descriptor{} }
func (f *fakeTree) Close() error                 { return nil }

func (f *fakeTree) Mkdir(_ context.Context, m storage.Mutation, _ storage.Checkpoint) (storage.MutationResult, error) {
	if f.dirs[m.Path] || f.files[m.Path] != nil {
		return storage.MutationResult{}, fmt.Errorf("mkdir %q: exists", m.Path)
	}
	f.dirs[m.Path] = true
	f.mkdirs = append(f.mkdirs, m.Path)
	return storage.MutationResult{}, nil
}

func (f *fakeTree) Stage(_ context.Context, in storage.WriteIntent, body io.Reader, _ storage.Checkpoint) (storage.Staged, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Staged{}, err
	}
	if in.Size != int64(len(data)) {
		return storage.Staged{}, fmt.Errorf("intent declares %d bytes, body has %d", in.Size, len(data))
	}
	f.staged[in.ID] = data
	return storage.Staged{Intent: in, Size: int64(len(data))}, nil
}

func (f *fakeTree) Publish(_ context.Context, s storage.Staged, _ storage.Checkpoint) (storage.MutationResult, error) {
	data, ok := f.staged[s.Intent.ID]
	if !ok {
		return storage.MutationResult{}, fmt.Errorf("publish %q: no staged body", s.Intent.Target)
	}
	if _, exists := f.files[s.Intent.Target]; exists {
		return storage.MutationResult{}, fmt.Errorf("publish %q: exists", s.Intent.Target)
	}
	f.files[s.Intent.Target] = data
	delete(f.staged, s.Intent.ID)
	return storage.MutationResult{}, nil
}

func (f *fakeTree) Inspect(context.Context, storage.WriteIntent, []byte) (storage.Inspection, error) {
	return storage.Inspection{}, nil
}
func (f *fakeTree) Discard(context.Context, storage.WriteIntent, []byte) error { return nil }

func (f *fakeTree) Remove(_ context.Context, m storage.Mutation, _ storage.Checkpoint) (storage.MutationResult, error) {
	delete(f.files, m.Path)
	delete(f.dirs, m.Path)
	f.removed = append(f.removed, m.Path)
	return storage.MutationResult{}, nil
}

func TestCopyPreservesTheTreeUnderTheDate(t *testing.T) {
	src := newFakeTree().put("Photos/2026/trip.jpg", "jpeg").put("Notes/diary.md", "text").dir("Empty")
	dst := newFakeTree()
	result, err := Copy(context.Background(), src, "", dst, "2026-11-10")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if result.Files != 2 || result.Folders != 5 {
		t.Fatalf("result = %+v, want 2 files and 5 folders (the date, Empty, Photos, Photos/2026, Notes)", result)
	}
	for path, want := range map[storage.Path]string{
		"2026-11-10/Photos/2026/trip.jpg": "jpeg",
		"2026-11-10/Notes/diary.md":       "text",
	} {
		if got := string(dst.files[path]); got != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
	}
	if !dst.dirs["2026-11-10/Empty"] {
		t.Fatal("an empty folder was not copied")
	}
}

func TestCopyRefusesToRewriteAnExistingSnapshot(t *testing.T) {
	src := newFakeTree().put("a.txt", "v1")
	dst := newFakeTree()
	if _, err := Copy(context.Background(), src, "", dst, "2026-11-10"); err != nil {
		t.Fatal(err)
	}
	src.put("a.txt", "v2")
	if _, err := Copy(context.Background(), src, "", dst, "2026-11-10"); err == nil {
		t.Fatal("a second copy into the same date succeeded; history was rewritten")
	}
	if string(dst.files["2026-11-10/a.txt"]) != "v1" {
		t.Fatal("the existing snapshot changed")
	}
}

func TestPruneRemovesWholeDatesOnly(t *testing.T) {
	tree := newFakeTree()
	for _, date := range []string{"2026-11-08", "2026-11-09", "2026-11-10"} {
		tree.put(storage.Path(date+"/a.txt"), date)
	}
	rem, ok := any(tree).(storage.Remover)
	if !ok {
		t.Fatal("fake tree is not a Remover")
	}
	removed, err := Prune(context.Background(), tree, rem, "", 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "2026-11-08" {
		t.Fatalf("removed %v, want the oldest date only", removed)
	}
	for _, path := range []string{"2026-11-09/a.txt", "2026-11-10/a.txt"} {
		if _, ok := tree.files[storage.Path(path)]; !ok {
			t.Fatalf("%s was removed by a rotation that should have kept it", path)
		}
	}
	dates, err := Dates(context.Background(), tree, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(dates) != 2 || dates[0] != "2026-11-09" || dates[1] != "2026-11-10" {
		t.Fatalf("dates = %v", dates)
	}
}

func TestVerifyReportsWhatItCounted(t *testing.T) {
	tree := newFakeTree().put("2026-11-10/x/y.txt", "abc")
	result, err := Verify(context.Background(), tree, "")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if result.Files != 1 || result.Bytes != 3 || result.Folders != 2 {
		t.Fatalf("result = %+v", result)
	}
}
