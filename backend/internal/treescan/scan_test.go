package treescan

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

type memoryTree struct {
	entries map[storage.Path]storage.Entry
	pages   map[string]storage.Page
	list    func(context.Context, storage.Path, storage.PageRequest) (storage.Page, error)
	stat    func(context.Context, storage.Path) (storage.Entry, error)
	calls   int
}

func file(p string) storage.Entry {
	return storage.Entry{Path: storage.Path(p), Kind: storage.File, Size: 7}
}
func folder(p string) storage.Entry {
	return storage.Entry{Path: storage.Path(p), Kind: storage.Folder}
}
func tree(entries ...storage.Entry) *memoryTree {
	m := &memoryTree{entries: map[storage.Path]storage.Entry{"": folder("")}, pages: map[string]storage.Page{}}
	for _, e := range entries {
		m.entries[e.Path] = e
		p := parent(e.Path)
		page := m.pages[string(p)+"|"]
		page.Entries = append(page.Entries, e)
		m.pages[string(p)+"|"] = page
	}
	return m
}
func (m *memoryTree) Stat(ctx context.Context, p storage.Path) (storage.Entry, error) {
	m.calls++
	if m.stat != nil {
		return m.stat(ctx, p)
	}
	e, ok := m.entries[p]
	if !ok {
		return e, fmt.Errorf("missing %q", p)
	}
	return e, nil
}
func (m *memoryTree) List(ctx context.Context, p storage.Path, r storage.PageRequest) (storage.Page, error) {
	m.calls++
	if m.list != nil {
		return m.list(ctx, p, r)
	}
	return m.pages[string(p)+"|"+r.Cursor], nil
}
func (m *memoryTree) Open(context.Context, storage.Path, storage.ReadOptions) (storage.Read, error) {
	panic("scan must never open bytes")
}
func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("got %v; want scan code %s", err, code)
	}
}

func TestScanPreservesExactNamesAndPaginates(t *testing.T) {
	names := []string{" empty ", "folder", "folder/ e\u0301 +#?%.txt ", "folder/zero"}
	m := tree(folder(names[0]), folder(names[1]), file(names[2]), storage.Entry{Path: storage.Path(names[3]), Kind: storage.File})
	root := m.pages["|"]
	m.pages["|"] = storage.Page{Entries: root.Entries[:1], NextCursor: "next"}
	m.pages["|next"] = storage.Page{Entries: root.Entries[1:]}
	child := m.pages["folder|"]
	m.pages["folder|"] = storage.Page{Entries: child.Entries[:1], NextCursor: "second"}
	m.pages["folder|second"] = storage.Page{Entries: child.Entries[1:]}
	r, err := Scan(context.Background(), m, Options{PageSize: 1, Names: storage.NameRules{CaseSensitive: true}})
	if err != nil {
		t.Fatal(err)
	}
	records, err := r.Records()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, record := range records {
		got = append(got, string(record.Entry.Path))
	}
	if !reflect.DeepEqual(got, names) {
		t.Fatalf("names rewritten: got %q want %q", got, names)
	}
	if records[3].Entry.Size != 0 {
		t.Fatal("zero-byte file was lost")
	}
}

func TestScanRejectsFaultyTrees(t *testing.T) {
	tests := []struct {
		name, code string
		entries    []storage.Entry
	}{
		{"duplicate", "duplicate_path", []storage.Entry{file("a"), file("a")}},
		{"type", "type_conflict", []storage.Entry{file("a"), folder("a")}},
		{"case", "case_conflict", []storage.Entry{file("A"), file("a")}},
		{"NFC", "normalization_conflict", []storage.Entry{file("e\u0301"), file("é")}},
		{"root child", "outside_root", []storage.Entry{folder("")}},
		{"absolute", "invalid_name", []storage.Entry{file("/escaped")}},
		{"negative size", "invalid_size", []storage.Entry{{Path: "a", Kind: storage.File, Size: -1}}},
		{"nonempty marker", "invalid_size", []storage.Entry{{Path: "a", Kind: storage.Folder, Size: 1}}},
		{"unknown kind", "invalid_kind", []storage.Entry{{Path: "a", Kind: "symlink"}}},
		{"dot", "invalid_name", []storage.Entry{file("..")}},
		{"backslash", "invalid_name", []storage.Entry{file("a\\b")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := tree(test.entries...)
			if test.name == "absolute" {
				m.pages["|"] = storage.Page{Entries: test.entries}
			}
			result, err := Scan(context.Background(), m, Options{})
			if result != nil {
				t.Fatal("failure returned applicable result")
			}
			expectCode(t, err, test.code)
		})
	}
}

func TestScanRejectsFaultyPagination(t *testing.T) {
	tests := []struct {
		name, code string
		pages      map[string]storage.Page
	}{
		{"same cursor", "nonadvancing_cursor", map[string]storage.Page{"|": {Entries: []storage.Entry{file("a")}, NextCursor: "x"}, "|x": {Entries: []storage.Entry{file("b")}, NextCursor: "x"}}},
		{"cursor cycle", "nonadvancing_cursor", map[string]storage.Page{"|": {Entries: []storage.Entry{file("a")}, NextCursor: "x"}, "|x": {Entries: []storage.Entry{file("b")}, NextCursor: "y"}, "|y": {Entries: []storage.Entry{file("c")}, NextCursor: "x"}}},
		{"empty continuation", "nonadvancing_cursor", map[string]storage.Page{"|": {NextCursor: "x"}}},
		{"repeat across pages", "duplicate_path", map[string]storage.Page{"|": {Entries: []storage.Entry{file("a")}, NextCursor: "x"}, "|x": {Entries: []storage.Entry{file("a")}}}},
		{"grandchild", "outside_root", map[string]storage.Page{"|": {Entries: []storage.Entry{file("a/b")}}}},
		{"sibling escape", "outside_root", map[string]storage.Page{"|": {Entries: []storage.Entry{folder("a")}}, "a|": {Entries: []storage.Entry{file("b")}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := tree(folder("a"))
			m.pages = test.pages
			r, err := Scan(context.Background(), m, Options{})
			if r != nil {
				t.Fatal("invalid scan returned a result")
			}
			expectCode(t, err, test.code)
		})
	}
}

func TestScanBoundsCancellationAndFailures(t *testing.T) {
	t.Run("invalid options before calls", func(t *testing.T) {
		m := tree()
		_, err := Scan(context.Background(), m, Options{MaxEntries: -1})
		expectCode(t, err, "invalid_options")
		if m.calls != 0 {
			t.Fatal("invalid options called backend")
		}
	})
	t.Run("entry budget", func(t *testing.T) {
		_, err := Scan(context.Background(), tree(file("a"), file("b")), Options{MaxEntries: 1})
		expectCode(t, err, "limit_exceeded")
	})
	t.Run("page budget", func(t *testing.T) {
		_, err := Scan(context.Background(), tree(folder("a")), Options{MaxPages: 1})
		expectCode(t, err, "limit_exceeded")
	})
	t.Run("depth budget", func(t *testing.T) {
		_, err := Scan(context.Background(), tree(folder("a"), file("a/b")), Options{MaxDepth: 1})
		expectCode(t, err, "limit_exceeded")
	})
	t.Run("page size", func(t *testing.T) {
		_, err := Scan(context.Background(), tree(file("a"), file("b")), Options{PageSize: 1})
		expectCode(t, err, "invalid_page")
	})
	t.Run("cancel during list", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		m := tree()
		m.list = func(context.Context, storage.Path, storage.PageRequest) (storage.Page, error) {
			cancel()
			return storage.Page{Entries: []storage.Entry{file("a")}}, nil
		}
		r, err := Scan(ctx, m, Options{})
		if r != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v %v", r, err)
		}
	})
	t.Run("list failure", func(t *testing.T) {
		m := tree()
		failure := errors.New("partial listing failed")
		m.list = func(context.Context, storage.Path, storage.PageRequest) (storage.Page, error) {
			return storage.Page{Entries: []storage.Entry{file("a")}}, failure
		}
		r, err := Scan(context.Background(), m, Options{})
		if r != nil || !errors.Is(err, failure) {
			t.Fatalf("got %v %v", r, err)
		}
	})
	t.Run("root identity", func(t *testing.T) {
		m := tree()
		m.entries[""] = folder("foreign")
		_, err := Scan(context.Background(), m, Options{})
		expectCode(t, err, "outside_root")
	})
	t.Run("version changed", func(t *testing.T) {
		m := tree()
		calls := 0
		m.stat = func(context.Context, storage.Path) (storage.Entry, error) {
			calls++
			e := folder("")
			e.Revision = []byte(fmt.Sprint(calls))
			return e, nil
		}
		_, err := Scan(context.Background(), m, Options{})
		expectCode(t, err, "changed")
	})
}

func TestResultCannotBeForgedOrMutated(t *testing.T) {
	for _, r := range []*Result{nil, {}} {
		_, err := r.Records()
		expectCode(t, err, "incomplete_scan")
	}
	e := file("a")
	e.NativeRef = []byte("native")
	e.Revision = []byte("version")
	e.Digest = &storage.Digest{Algorithm: "sha256", Value: []byte("digest")}
	r, err := Scan(context.Background(), tree(e), Options{})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := r.Records()
	records[0].Entry.Path = "changed"
	records[0].Entry.NativeRef[0] = 'X'
	records[0].Entry.Revision[0] = 'X'
	records[0].Entry.Digest.Value[0] = 'X'
	again, _ := r.Records()
	if !reflect.DeepEqual(again[0].Entry, e) {
		t.Fatalf("result mutated: %#v", again[0])
	}
}

func TestCaseSensitiveTreePreservesDistinctNames(t *testing.T) {
	r, err := Scan(context.Background(), tree(file("A"), file("a")), Options{Names: storage.NameRules{CaseSensitive: true}})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := r.Records()
	if len(records) != 2 {
		t.Fatal("case-sensitive entries collapsed")
	}
}

func TestInventoryExactKeysImplicitParentsAndExplicitMarker(t *testing.T) {
	csv := "key,size,lastmodifieddate,etag,storageclass\ndrive/owner/ folder /e\u0301 +%2F?#.txt ,3,2020-01-02T03:04:05Z,opaque,STANDARD\ndrive/owner/ folder /,0,,,\ndrive/owner/empty/,0,,,\ndrive/owner/,0,,,\n"
	r, err := Inventory(context.Background(), strings.NewReader(csv), "drive/owner/", Options{Names: storage.NameRules{CaseSensitive: true}})
	if err != nil {
		t.Fatal(err)
	}
	records, _ := r.Records()
	if len(records) != 3 || records[0].Entry.Path != " folder " || records[0].Implicit || records[1].Entry.Path != " folder /e\u0301 +%2F?#.txt " || records[1].Entry.ModifiedAt.Year() != 2020 || *records[1].ETag != "opaque" {
		t.Fatalf("wrong records %#v", records)
	}
	r, err = Inventory(context.Background(), strings.NewReader("key,size\ndrive/owner/a/b/c,0\n"), "drive/owner/", Options{})
	if err != nil {
		t.Fatal(err)
	}
	records, _ = r.Records()
	if len(records) != 3 || !records[0].Implicit || !records[1].Implicit {
		t.Fatalf("implicit ancestors missing: %#v", records)
	}
}

func TestInventoryRejectsInvalidData(t *testing.T) {
	tests := []struct{ name, code, csv string }{
		{"no header", "invalid_inventory", "drive/owner/a,1\n"},
		{"ambiguous header", "invalid_inventory", "key,objectkey,size\na,b,0\n"},
		{"bad size", "invalid_size", "key,size\ndrive/owner/a,unknown\n"},
		{"negative size", "invalid_size", "key,size\ndrive/owner/a,-1\n"},
		{"bad time", "invalid_time", "key,size,lastmodified\ndrive/owner/a,0,not-a-date\n"},
		{"foreign owner", "outside_root", "key,size\ndrive/other/a,0\n"},
		{"ambiguous slashes", "invalid_name", "key,size\ndrive/owner/a//,0\n"},
		{"ambiguous root slashes", "invalid_name", "key,size\ndrive/owner//,0\n"},
		{"nonempty marker", "invalid_size", "key,size\ndrive/owner/a/,1\n"},
		{"nonempty root marker", "invalid_size", "key,size\ndrive/owner/,1\n"},
		{"duplicate key", "duplicate_path", "key,size\ndrive/owner/a,0\ndrive/owner/a,0\n"},
		{"type conflict", "type_conflict", "key,size\ndrive/owner/a/b,0\ndrive/owner/a,0\n"},
		{"NFC parent collision", "normalization_conflict", "key,size\ndrive/owner/e\u0301/a,0\ndrive/owner/é/b,0\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, err := Inventory(context.Background(), strings.NewReader(test.csv), "drive/owner/", Options{})
			if r != nil {
				t.Fatal("bad inventory became applicable")
			}
			expectCode(t, err, test.code)
		})
	}
	t.Run("truncated CSV", func(t *testing.T) {
		r, err := Inventory(context.Background(), strings.NewReader("key,size\n\"drive/owner/a,0\n"), "drive/owner/", Options{})
		if r != nil || err == nil {
			t.Fatalf("got %v %v", r, err)
		}
	})
	t.Run("bounded synthesized parents", func(t *testing.T) {
		_, err := Inventory(context.Background(), strings.NewReader("key,size\ndrive/owner/a/b/c,0\n"), "drive/owner/", Options{MaxEntries: 2})
		expectCode(t, err, "limit_exceeded")
	})
}

func TestInventoryBoundsAndCancellation(t *testing.T) {
	csv := "key,size,etag,storageclass\ndrive/owner/a,0,,\n"
	t.Run("exact byte budget", func(t *testing.T) {
		r, err := Inventory(context.Background(), strings.NewReader(csv), "drive/owner/", Options{MaxInventoryBytes: int64(len(csv))})
		if err != nil {
			t.Fatal(err)
		}
		records, _ := r.Records()
		if records[0].ETag != nil || records[0].StorageClass != nil {
			t.Fatal("missing optional metadata became an empty observed value")
		}
	})
	t.Run("budget exhausted", func(t *testing.T) {
		r, err := Inventory(context.Background(), strings.NewReader(csv), "drive/owner/", Options{MaxInventoryBytes: int64(len(csv) - 1)})
		if r != nil {
			t.Fatal("truncated inventory became applicable")
		}
		expectCode(t, err, "limit_exceeded")
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		r, err := Inventory(ctx, strings.NewReader(csv), "drive/owner/", Options{})
		if r != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v %v", r, err)
		}
	})
	t.Run("nil input", func(t *testing.T) {
		_, err := Inventory(context.Background(), nil, "drive/owner/", Options{})
		expectCode(t, err, "invalid_inventory")
	})
}

func TestRetainedIndexConflicts(t *testing.T) {
	r, err := Scan(context.Background(), tree(file("a")), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []storage.Entry{folder("a"), file("A"), file("a/retained")} {
		if err := r.CheckIndex([]storage.Entry{entry}); err == nil {
			t.Fatalf("retained conflict accepted: %#v", entry)
		}
	}
	if err := r.CheckIndex([]storage.Entry{file("a"), file("retained")}); err != nil {
		t.Fatal(err)
	}
}
