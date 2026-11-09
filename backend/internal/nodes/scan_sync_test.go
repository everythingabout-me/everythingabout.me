package nodes

import (
	"testing"

	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/treescan"
	"golang.org/x/text/unicode/norm"
)

func scanned(paths ...string) []treescan.Record {
	records := make([]treescan.Record, 0, len(paths))
	for _, p := range paths {
		records = append(records, treescan.Record{Entry: storage.Entry{Path: storage.Path(p), Kind: storage.File}})
	}
	return records
}

func indexed(entries map[string]storage.Kind) []storage.Entry {
	list := make([]storage.Entry, 0, len(entries))
	for p, kind := range entries {
		list = append(list, storage.Entry{Path: storage.Path(p), Kind: kind})
	}
	// The caller compares sets; ordering here does not matter.
	return list
}

func pathsOf(entries []storage.Entry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, string(entry.Path))
	}
	return paths
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAbsentEntriesFindsOnlyWhatTheScanMissed(t *testing.T) {
	existing := indexed(map[string]storage.Kind{
		"":             storage.Folder,
		"a.txt":        storage.File,
		"dir":          storage.Folder,
		"dir/b.txt":    storage.File,
		"gone.txt":     storage.File,
		"old/nested":   storage.File,
		"old":          storage.Folder,
		"old/nested/2": storage.File,
	})
	absent := pathsOf(absentEntries(existing, scanned("a.txt", "dir", "dir/b.txt")))
	want := []string{"old/nested/2", "old/nested", "gone.txt", "old"}
	if !equal(absent, want) {
		t.Fatalf("absent = %v, want %v (deepest first, root skipped)", absent, want)
	}
}

func TestAbsentEntriesIsEmptyForACompleteIndex(t *testing.T) {
	existing := indexed(map[string]storage.Kind{"a.txt": storage.File, "dir": storage.Folder})
	if got := absentEntries(existing, scanned("a.txt", "dir")); len(got) != 0 {
		t.Fatalf("complete index reported absent entries: %v", pathsOf(got))
	}
}

// macOS stores NFD names; the index and the scan must agree through NFC or every
// sync would delete and re-add the same file.
func TestAbsentEntriesComparesNormalizedNames(t *testing.T) {
	composed := "caf\u00e9.txt"
	decomposed := norm.NFD.String(composed)
	existing := indexed(map[string]storage.Kind{composed: storage.File})
	if got := absentEntries(existing, scanned(decomposed)); len(got) != 0 {
		t.Fatalf("normalization difference treated as a removal: %v", pathsOf(got))
	}
}

func TestAbsentEntriesOrdersRemovalsDeterministically(t *testing.T) {
	existing := indexed(map[string]storage.Kind{
		"aa":  storage.File,
		"bb":  storage.File,
		"ccc": storage.File,
	})
	first := pathsOf(absentEntries(existing, nil))
	second := pathsOf(absentEntries(existing, nil))
	if !equal(first, second) {
		t.Fatalf("removal order is not stable: %v vs %v", first, second)
	}
	if !equal(first, []string{"ccc", "bb", "aa"}) {
		t.Fatalf("expected longest paths first, got %v", first)
	}
}
