package nodes

import (
	"testing"

	"everythingabout.me/backend/internal/namecompare"
	"everythingabout.me/backend/internal/nameprobe"
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
	absent := pathsOf(absentEntries(existing, scanned("a.txt", "dir", "dir/b.txt"), namecompare.Key(false)))
	want := []string{"old/nested/2", "old/nested", "gone.txt", "old"}
	if !equal(absent, want) {
		t.Fatalf("absent = %v, want %v (deepest first, root skipped)", absent, want)
	}
}

func TestAbsentEntriesIsEmptyForACompleteIndex(t *testing.T) {
	existing := indexed(map[string]storage.Kind{"a.txt": storage.File, "dir": storage.Folder})
	if got := absentEntries(existing, scanned("a.txt", "dir"), namecompare.Key(false)); len(got) != 0 {
		t.Fatalf("complete index reported absent entries: %v", pathsOf(got))
	}
}

// macOS stores NFD names; the index and the scan must agree through NFC or every
// sync would delete and re-add the same file.
func TestAbsentEntriesComparesNormalizedNames(t *testing.T) {
	composed := "caf\u00e9.txt"
	decomposed := norm.NFD.String(composed)
	existing := indexed(map[string]storage.Kind{composed: storage.File})
	if got := absentEntries(existing, scanned(decomposed), namecompare.Key(false)); len(got) != 0 {
		t.Fatalf("normalization difference treated as a removal: %v", pathsOf(got))
	}
}

func TestAbsentEntriesOrdersRemovalsDeterministically(t *testing.T) {
	existing := indexed(map[string]storage.Kind{
		"aa":  storage.File,
		"bb":  storage.File,
		"ccc": storage.File,
	})
	first := pathsOf(absentEntries(existing, nil, namecompare.Key(false)))
	second := pathsOf(absentEntries(existing, nil, namecompare.Key(false)))
	if !equal(first, second) {
		t.Fatalf("removal order is not stable: %v vs %v", first, second)
	}
	if !equal(first, []string{"ccc", "bb", "aa"}) {
		t.Fatalf("expected longest paths first, got %v", first)
	}
}

// TestAbsentEntriesUsesIndexNameComparison pins the sync consequence of the
// audit: an observed spelling that the uniqueness index treats as the same
// sibling must not be reported absent, or a complete scan deletes the row that
// carries exactly the file it just observed.
func TestAbsentEntriesUsesIndexNameComparison(t *testing.T) {
	key := namecompare.Key(false)
	for _, pair := range nameprobe.Pairs {
		if !nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B) {
			continue
		}
		existing := indexed(map[string]storage.Kind{pair.A: storage.File})
		if got := absentEntries(existing, scanned(pair.B), key); len(got) != 0 {
			t.Errorf("%s: index-equal observation reported absent: %v", pair.ID, pathsOf(got))
		}
	}
	for _, pair := range nameprobe.Pairs {
		if nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B) {
			continue
		}
		existing := indexed(map[string]storage.Kind{pair.A: storage.File})
		if got := absentEntries(existing, scanned(pair.B), key); len(got) != 1 {
			t.Errorf("%s: index-distinct observation should remove the missing row, got %v", pair.ID, pathsOf(got))
		}
	}
}

// TestPendingDeleteCoversUsesIndexNameComparison is the import fence: a pending
// purge must block a spelling the index treats as equal, or the scan resurrects
// a path the user deleted while its native objects were still disappearing.
func TestPendingDeleteCoversUsesIndexNameComparison(t *testing.T) {
	key := namecompare.Key(false)
	for _, pair := range nameprobe.Pairs {
		roots := []string{key("/" + pair.A)}
		got := pendingDeleteCovers(key("/"+pair.B), roots)
		want := nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B)
		if got != want {
			t.Errorf("%s: pendingDeleteCovers(%q under %q)=%v, want %v", pair.ID, pair.B, pair.A, got, want)
		}
	}
}
