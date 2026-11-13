package nameprobe

import "testing"

// CheckKey requires one implementation to compute exactly the declared
// pipeline for every corpus spelling. A test that declares the pipeline the
// implementation currently uses is a characterization test: it fails when the
// implementation changes, which is the signal to update the convergence plan
// rather than silently accept a new comparison rule.
func CheckKey(t *testing.T, site string, p Pipeline, caseSensitive bool, key func(string) string) {
	t.Helper()
	want := Key(p, caseSensitive)
	for _, pair := range Pairs {
		for _, spelling := range []string{pair.A, pair.B} {
			got, expected := key(spelling), want(spelling)
			if got != expected {
				t.Errorf("%s (%s): key(%q)=%q, want %q", site, p, spelling, got, expected)
			}
		}
	}
}

// CheckPair requires one implementation's same-name answer to match the
// declared pipeline for every corpus pair.
func CheckPair(t *testing.T, site string, p Pipeline, caseSensitive bool, same func(a, b string) bool) {
	t.Helper()
	for _, pair := range Pairs {
		got, want := same(pair.A, pair.B), Same(p, caseSensitive, pair.A, pair.B)
		if got != want {
			t.Errorf("%s (%s): %s same=%v, want %v (A=%q B=%q)", site, p, pair.ID, got, want, pair.A, pair.B)
		}
	}
}
