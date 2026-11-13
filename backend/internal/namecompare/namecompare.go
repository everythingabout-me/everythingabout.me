// Package namecompare owns the one Go implementation of the canonical file
// name comparison versions declared by the stable identity contract
// (docs/design/file-identity.md): nfc-sensitive-v1 and nfc-fold-v1.
//
// The versions are a contract shared with SQL. file_identity_comparison builds
// the unique sibling key in Postgres, and the nodes trigger stores it as
// comparison_name. A second Go spelling of the same pipeline is a bug, not an
// implementation detail: it splits equivalence classes the index merges, so an
// admission fence or a scope check built on it disagrees with the unique index
// about how many siblings exist. internal/nameprobe records that failure (the
// four post-fold composition pairs) and its per-site tests are the guard.
//
// nfc-fold-v1 is NFC, full Unicode case folding, NFC. The trailing NFC is the
// step a hand-written "normalize then fold" misses: full folding can produce a
// base letter that then composes with a following combining mark, so without it
// two spellings the index treats as one name stay distinct in Go. nfc-sensitive-v1
// is NFC alone. There is no third variant, and callers select only by the root's
// native case-sensitivity.
package namecompare

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// KeyString returns the comparison key one native spelling compares under.
// Callers pass the root's own case-sensitivity; a case-sensitive root uses NFC
// exactly as SQL does, so the two implementations agree on that half too.
func KeyString(caseSensitive bool, value string) string {
	value = norm.NFC.String(value)
	if caseSensitive {
		return value
	}
	return norm.NFC.String(cases.Fold().String(value))
}

// Key returns KeyString as a function, for sites that compare many values under
// one root (segment keys, directory scans, reserved-name checks). The returned
// function is per-segment: on "/"-separated paths it agrees with applying the
// same pipeline to the whole path, because neither full folding nor canonical
// composition crosses a separator.
func Key(caseSensitive bool) func(string) string {
	return func(value string) string { return KeyString(caseSensitive, value) }
}
