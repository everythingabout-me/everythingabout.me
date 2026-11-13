package treescan

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

// The scan's duplicate check first rejects NFC-equal spellings and then
// fold-equal ones, which is the fold-only equivalence. It therefore refuses a
// native tree containing a pair the index would also merge, but not the four
// post-fold cases.
func TestCollectorCaseConflictUsesLegacyKey(t *testing.T) {
	nameprobe.CheckPair(t, "treescan.case_conflict", nameprobe.LegacyPipeline, false, func(a, b string) bool {
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

// The reserved-name check folds without normalizing either side. Production
// reserved names are ASCII today, so this is latent rather than live; it is
// recorded because the convergence must cover it too.
func TestReservedNameUsesFoldOnly(t *testing.T) {
	nameprobe.CheckPair(t, "treescan.reservedName", nameprobe.FoldOnlyPipeline, false, func(a, b string) bool {
		c, err := newCollector(Options{Names: storage.NameRules{CaseSensitive: false, ReservedNames: []string{a}}})
		if err != nil {
			t.Fatal(err)
		}
		return c.validate(storage.Entry{Path: storage.Path(b), Kind: storage.File}) != nil
	})
}
