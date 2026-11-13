// Package nameprobe is the executable record of the name-comparison audit.
//
// A case-insensitive root compares sibling names with nfc-fold-v1: NFC, full
// Unicode case folding, NFC. The SQL implementation (file_identity_comparison)
// and filechanges.NameKey apply that whole pipeline. Most Go call sites instead
// stop after folding, and the reserved-name checks skip the leading NFC. The
// difference is invisible for ordinary names because folding an NFC string
// rarely leaves decomposed text, but it is visible when a fold produces a base
// letter that then composes with a following combining mark.
//
// The corpus below contains pairs that separate the pipelines. The four pairs
// in Divergent are equal under the documented pipeline but different under the
// fold-only Go pipeline:
//
//	"ſ̂" (U+017F U+0302) vs "Ŝ" (U+015C)
//	"ẛ̣" (U+1E9B U+0323) vs "Ṩ" (U+1E68)
//	"ϵ̓" (U+03F5 U+0313) vs "Ἐ" (U+0395 U+0313)
//	"ẖ̈" (U+1E96 U+0308) vs "Ḧ̱" (U+1E26 U+0331)
//
// On a case-insensitive root the uniqueness index therefore treats one such
// pair as a single sibling while fileops.guardConflict, the token-scope check
// and the API path scope treat it as two. That is a real fence/authorization
// divergence, not just a byte-level difference.
//
// The audited sites are:
//
//	reference: SQL file_identity_comparison, filechanges.NameKey
//	legacy:    fileops.comparePath, auth token scope, api.filesPathKey,
//	           api.RequireRead, localstorage driver key, treescan case conflict
//	fold-only: treescan and snapshot reserved-name checks
//	NFC-only:  paths.Normalize, nodes scan pending/absent paths, browse path
//	           filters, API cursor validation, treescan normalization conflict
//
// This package is a test fixture. It is not imported by production code and
// must not grow a comparison implementation of its own. Converging means one
// shared Go key function, matching the SQL function, used at every site above.
// It also needs a backfill of the stored comparison_path columns, because they
// were written by the legacy key: file_operation_guards (write-only today),
// file_uploads and file_credit_claims. Applying NFC to those values upgrades
// the legacy key to the reference key; credit-claim primary-key collisions
// must be collapsed first. The backfill is why this audit stops short of the
// code change: it would rewrite existing rows, which the audit branch forbids.
//
// The per-package tests declare the pipeline they exercise. They are
// characterization tests: while the divergence is unfixed they assert the
// pipeline that is there, and the convergence change flips the declaration to
// ReferencePipeline. A failure means the declaration needs updating, not that
// the assertion should be loosened.
package nameprobe

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Pair is one probe case: two spellings of a name under a case-insensitive
// root. ID is stable so the report and tests can name a row.
type Pair struct {
	ID string
	A  string
	B  string
}

// Pairs covers NFC/NFD input, full-fold expansions that lower() cannot express,
// dotted/dotless I, final sigma, combining-mark order, compatibility variants
// that must stay distinct, and the four post-fold composition cases.
var Pairs = []Pair{
	{ID: "nfc-vs-nfd", A: "\u00e9", B: "e\u0301"},
	{ID: "turkish-dotted", A: "\u0130", B: "i\u0307"},
	{ID: "sharp-s", A: "\u00df", B: "ss"},
	{ID: "capital-sharp-s", A: "\u1e9e", B: "ss"},
	{ID: "greek-sigma", A: "\u03a3", B: "\u03c2"},
	{ID: "kelvin", A: "\u212a", B: "k"},
	{ID: "angstrom", A: "\u212b", B: "\u00e5"},
	{ID: "fullwidth", A: "\uff21", B: "A"},
	{ID: "long-s-caron", A: "\u017f\u0302", B: "\u015c"},
	{ID: "s-dot-above-below", A: "\u1e9b\u0323", B: "\u1e68"},
	{ID: "greek-epsilon", A: "\u03f5\u0313", B: "\u0395\u0313"},
	{ID: "h-line-diaeresis", A: "\u1e96\u0308", B: "\u1e26\u0331"},
	{ID: "combining-order", A: "a\u0301\u0323", B: "a\u0323\u0301"},
	{ID: "long-s-plain", A: "\u017f", B: "s"},
}

// Divergent lists the pair IDs where the two fold pipelines disagree. Keep it
// in sync with the corpus; TestKnownDivergentPairs fails otherwise.
var Divergent = []string{"long-s-caron", "s-dot-above-below", "greek-epsilon", "h-line-diaeresis"}

// Pipeline names one of the comparison pipelines found in the tree.
type Pipeline int

const (
	// ReferencePipeline is nfc-sensitive-v1 / nfc-fold-v1: NFC, full case
	// fold, NFC. SQL file_identity_comparison and filechanges.NameKey use it.
	ReferencePipeline Pipeline = iota
	// LegacyPipeline is NFC then full case fold, without the final NFC. It is
	// used by fileops.comparePath, auth's token scope, the API path scope and
	// the local driver.
	LegacyPipeline
	// FoldOnlyPipeline is full case fold with no NFC on either side. The
	// reserved-name checks use it.
	FoldOnlyPipeline
	// NFCOnlyPipeline is NFC with no case folding. Path ingestion, index
	// search and scan prefix checks that do not consult name rules use it.
	NFCOnlyPipeline
)

func (p Pipeline) String() string {
	switch p {
	case ReferencePipeline:
		return "reference (NFC→fold→NFC)"
	case LegacyPipeline:
		return "legacy (NFC→fold)"
	case FoldOnlyPipeline:
		return "fold-only"
	case NFCOnlyPipeline:
		return "NFC-only"
	}
	return "unknown"
}

// Key returns the exact key a pipeline computes. Case-sensitive roots use NFC
// for the fold pipelines, exactly as the SQL function does.
func Key(p Pipeline, caseSensitive bool) func(string) string {
	switch p {
	case ReferencePipeline:
		return func(s string) string {
			v := norm.NFC.String(s)
			if !caseSensitive {
				v = norm.NFC.String(cases.Fold().String(v))
			}
			return v
		}
	case LegacyPipeline:
		return func(s string) string {
			v := norm.NFC.String(s)
			if !caseSensitive {
				v = cases.Fold().String(v)
			}
			return v
		}
	case FoldOnlyPipeline:
		return func(s string) string {
			if caseSensitive {
				return s
			}
			return cases.Fold().String(s)
		}
	case NFCOnlyPipeline:
		return norm.NFC.String
	}
	return nil
}

// Same reports whether a pipeline treats the two spellings as one name.
func Same(p Pipeline, caseSensitive bool, a, b string) bool {
	key := Key(p, caseSensitive)
	return key(a) == key(b)
}
