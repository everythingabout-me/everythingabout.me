package treescan

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

// The scan's duplicate check must use the canonical key, so a native tree
// containing two spellings the index would merge is reported instead of
// imported and rejected later by the unique sibling index.
func TestCollectorCaseConflictUsesReferenceKey(t *testing.T) {
	nameprobe.CheckPair(t, "treescan.case_conflict", nameprobe.ReferencePipeline, false, func(a, b string) bool {
		c, err := newCollector(Options{Names: storage.NameRules{CaseSensitive: false}})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.add(Record{Entry: storage.Entry{Path: storage.Path(a), Kind: storage.File}}); err != nil {
			t.Fatalf("add %q: %v", a, err)
		}
		return c.add(Record{Entry: storage.Entry{Path: storage.Path(b), Kind: storage.File}}) != nil
	})
}

// The reserved-name check compares under the same key. Production reserved
// names are ASCII today, so the four post-fold pairs are latent rather than
// live; they are recorded because the convergence covers them too.
func TestReservedNameUsesReferenceKey(t *testing.T) {
	nameprobe.CheckPair(t, "treescan.reservedName", nameprobe.ReferencePipeline, false, func(a, b string) bool {
		c, err := newCollector(Options{Names: storage.NameRules{CaseSensitive: false, ReservedNames: []string{a}}})
		if err != nil {
			t.Fatal(err)
		}
		return c.validate(storage.Entry{Path: storage.Path(b), Kind: storage.File}) != nil
	})
}
