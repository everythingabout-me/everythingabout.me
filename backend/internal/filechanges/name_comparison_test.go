package filechanges_test

import (
	"testing"

	"everythingabout.me/backend/internal/filechanges"
	"everythingabout.me/backend/internal/nameprobe"
)

// NameKey documents itself as matching identity's nfc-fold-v1. The audit
// confirms it does: NFC, full fold, NFC. It is not the implementation the
// change feed uses in production, though (api.filesPathKey is), so this test
// proves the intended mirror, not the wired one. See the api probe.
func TestNameComparisonMatchesIdentity(t *testing.T) {
	nameprobe.CheckKey(t, "filechanges.NameKey", nameprobe.ReferencePipeline, false, filechanges.NameKey(false))
}
