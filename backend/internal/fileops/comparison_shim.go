package fileops

// One-release read shim for sql/052 (name comparison backfill).
//
// sql/052 rewrites every stored comparison_path to the canonical key, which is
// the pre-backfill key with one NFC applied. `eam migrate` runs before the
// service restart, so for a few seconds the old binary is still writing rows
// with the pre-backfill spelling: NFC then full case fold, without the final
// NFC. A read that only asked for the canonical spelling would miss such a row
// and could, for example, open a second upload session for a path that already
// has one, or fail a delete settlement with credit_conflict.
//
// Rows written by the old binary cannot be located by recomputing its key for
// the spelling this request happens to use: two spellings the canonical key
// merges have different legacy keys, so a legacy row for one of them would not
// be found from the other. Matching the column itself is the only lookup that
// covers the whole equivalence class, which is why comparisonShim wraps the
// stored value rather than passing two expected spellings.
//
// comparisonShim is not a second comparison rule: no decision compares two
// names through it. It only locates rows a differently keyed writer may have
// created. Wrapping the column costs the primary-key lookup on
// file_credit_claims, whose read runs once per settled entry, so sql/052
// creates a matching expression index for the release that carries this shim.
//
// Rows the old binary writes during the window are the only rows that can still
// carry the legacy spelling afterwards, and sql/052 will never touch them again.
// The successor release must therefore re-run the same normalize updates once
// more (they match a handful of rows, if any), drop the expression index, and
// then DELETE THIS FILE AND EVERY comparisonShim CALL SITE.

// comparisonShim returns the SQL expression that matches one stored
// comparison_path column against a canonical key while the backfill shim is in
// place. column is the qualified column name (for example "u.comparison_path").
func comparisonShim(column string) string {
	return "normalize(" + column + ", NFC)"
}
