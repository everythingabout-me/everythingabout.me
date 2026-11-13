package fileops

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

func legacyRoot() Root { return Root{CaseSensitive: false} }

// comparePath is the key behind the operation fence, the scope recheck and the
// stored comparison_path columns. It stops after folding, so it is finer than
// the identity key the database index uses.
func TestNameComparisonLegacyKey(t *testing.T) {
	nameprobe.CheckKey(t, "fileops.comparePath", nameprobe.LegacyPipeline, false, func(s string) string {
		return comparePath(legacyRoot(), storage.Path(s))
	})
}

// TestGuardConflictMissesIndexEqualSpellings is the fence consequence of the
// audit: for each pair the uniqueness index merges but the legacy key splits,
// guardConflict reports no conflict, so two operations can be admitted against
// one indexed sibling. Convergence flips this expectation; it is explicit so a
// partial fix cannot pass unnoticed.
func TestGuardConflictMissesIndexEqualSpellings(t *testing.T) {
	root := legacyRoot()
	for _, pair := range nameprobe.Pairs {
		merged := nameprobe.Same(nameprobe.ReferencePipeline, false, pair.A, pair.B)
		split := !nameprobe.Same(nameprobe.LegacyPipeline, false, pair.A, pair.B)
		if !merged || !split {
			continue
		}
		if guardConflict(root, Guard{Path: storage.Path(pair.A)}, Guard{Path: storage.Path(pair.B)}) {
			t.Errorf("%s: guardConflict already merges the pair; the audit finding is stale", pair.ID)
		}
	}
}
