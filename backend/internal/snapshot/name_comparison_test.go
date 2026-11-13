package snapshot

import (
	"testing"

	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

// reservedName must compare with the destination's canonical name key, so a
// snapshot cannot write a spelling the destination index would treat as one
// name with a reserved entry.
func TestReservedNameUsesReferenceKey(t *testing.T) {
	nameprobe.CheckPair(t, "snapshot.reservedName", nameprobe.ReferencePipeline, false, func(a, b string) bool {
		return reservedName(b, storage.NameRules{CaseSensitive: false, ReservedNames: []string{a}})
	})
}
