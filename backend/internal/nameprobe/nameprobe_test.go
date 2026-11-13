package nameprobe

import "testing"

// TestKnownDivergentPairs fixes the audit's core finding: exactly these pairs
// are one name under the documented pipeline and two names under the fold-only
// Go pipeline. If the corpus stops separating the two, the probe has gone
// blind and the audit must be redone before anyone relies on it.
func TestKnownDivergentPairs(t *testing.T) {
	got := map[string]bool{}
	for _, pair := range Pairs {
		if Same(ReferencePipeline, false, pair.A, pair.B) != Same(LegacyPipeline, false, pair.A, pair.B) {
			got[pair.ID] = true
		}
	}
	if len(got) != len(Divergent) {
		t.Fatalf("divergent pair set changed: got %v, want %v", got, Divergent)
	}
	for _, id := range Divergent {
		if !got[id] {
			t.Fatalf("pair %q no longer separates the pipelines", id)
		}
	}
}

// TestReferenceCoarsensLegacy is the safety invariant behind the divergence.
// The documented key only merges equivalence classes the legacy key already
// merged (it applies one more canonical normalization), so scope checks that
// use the legacy key can deny an index-equal spelling but can never admit a
// spelling the index treats as a different name. A change that inverts this
// direction would widen authorization and must not be made.
func TestReferenceCoarsensLegacy(t *testing.T) {
	for _, pair := range Pairs {
		if Same(LegacyPipeline, false, pair.A, pair.B) && !Same(ReferencePipeline, false, pair.A, pair.B) {
			t.Fatalf("%s: legacy merges a pair the reference splits", pair.ID)
		}
	}
}

// TestCaseSensitivePipelinesAgree checks the other half of the contract:
// nfc-sensitive-v1 is NFC everywhere, so every implementation must agree on a
// case-sensitive root regardless of its fold pipeline.
func TestCaseSensitivePipelinesAgree(t *testing.T) {
	for _, p := range []Pipeline{ReferencePipeline, LegacyPipeline, NFCOnlyPipeline} {
		for _, pair := range Pairs {
			got := Same(p, true, pair.A, pair.B)
			want := Same(NFCOnlyPipeline, true, pair.A, pair.B)
			if got != want {
				t.Fatalf("%s disagrees on case-sensitive root for %s", p, pair.ID)
			}
		}
	}
}
