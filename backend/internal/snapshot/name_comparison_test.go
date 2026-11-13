package snapshot

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

// reservedName compares folded spellings without NFC, like the scan check.
func TestReservedNameUsesFoldOnly(t *testing.T) {
	nameprobe.CheckPair(t, "snapshot.reservedName", nameprobe.FoldOnlyPipeline, false, func(a, b string) bool {
		return reservedName(b, storage.NameRules{CaseSensitive: false, ReservedNames: []string{a}})
	})
}
