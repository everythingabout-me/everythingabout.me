package fileops

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

func legacyRoot() Root { return Root{CaseSensitive: false} }

// comparePath is the key behind the operation fence, the scope recheck and the
// stored comparison_path columns. It must be the reference pipeline the SQL
// uniqueness index applies, not a fold-only approximation of it.
func TestNameComparisonMatchesReference(t *testing.T) {
	nameprobe.CheckKey(t, "fileops.comparePath", nameprobe.ReferencePipeline, false, func(s string) string {
		return comparePath(legacyRoot(), storage.Path(s))
	})
}

// TestGuardConflictMergesIndexEqualSpellings is the fence consequence of the
// audit, now fixed: for every pair the uniqueness index merges, guardConflict
// must report the conflict, so two operations can never be admitted against one
// indexed sibling. It is explicit so a regression to the legacy pipeline fails
// here instead of at the unique index after native effects started.
func TestGuardConflictMergesIndexEqualSpellings(t *testing.T) {
	root := legacyRoot()
	for _, pair := range nameprobe.Pairs {
		if !nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B) {
			continue
		}
		if !guardConflict(root, Guard{Path: storage.Path(pair.A)}, Guard{Path: storage.Path(pair.B)}) {
			t.Errorf("%s: guardConflict reports no conflict for an index-equal pair", pair.ID)
		}
	}
}

// TestGuardConflictKeepsDistinctSpellingsApart is the other direction: the
// comparison must not become coarser than the index and lock two different
// siblings out of each other. A fold or normalization that merges an
// index-distinct pair would deny a valid write forever.
func TestGuardConflictKeepsDistinctSpellingsApart(t *testing.T) {
	root := legacyRoot()
	for _, pair := range nameprobe.Pairs {
		if nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B) {
			continue
		}
		if guardConflict(root, Guard{Path: storage.Path(pair.A)}, Guard{Path: storage.Path(pair.B)}) {
			t.Errorf("%s: guardConflict merges a pair the index keeps distinct", pair.ID)
		}
	}
}
