package filecontract_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

func assertProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	if status == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	var p *fc.Problem
	if !errors.As(err, &p) {
		t.Fatalf("error = %v; want Problem %d %s", err, status, code)
	}
	if p.Status != status || p.Code != code {
		t.Fatalf("Problem = %d %s; want %d %s", p.Status, p.Code, status, code)
	}
}

func TestPathPreservesExactSpellingAndBoundaryConversion(t *testing.T) {
	for _, path := range []string{"/", "/Photos/a.jpg", "/ leading /trailing ", "/cafe\u0301/naïve.txt", "/café.txt", "/照片/旅行.jpg", "/a+b#c?d%20.txt", "/%2e%2e/%2F", "/Case/CASE", "/zero\u200dwidth"} {
		t.Run(path, func(t *testing.T) {
			parsed, err := fc.ParsePath(path)
			assertProblem(t, err, 0, "")
			if string(parsed) != path {
				t.Fatalf("path spelling changed: %q -> %q", path, parsed)
			}
			relative := strings.TrimPrefix(path, "/")
			if parsed.Relative() != relative {
				t.Fatalf("relative = %q; want %q", parsed.Relative(), relative)
			}
			restored, err := fc.FromRelative(relative)
			assertProblem(t, err, 0, "")
			if string(restored) != path {
				t.Fatalf("round trip changed spelling: %q", restored)
			}
		})
	}
}

func TestInvalidPathsAreRejectedWithoutCleaning(t *testing.T) {
	for _, path := range []string{"", "relative", "//", "/a/", "/a//b", "/.", "/..", "/a/./b", "/a/../b", "/a\\b", "/a\x00b", "/a\nb", "/a\tb", "/a\x7fb", "/a\u0085b", "/bad\xff"} {
		t.Run(url.QueryEscape(path), func(t *testing.T) { assertProblem(t, fc.ValidatePath(path), 400, "invalid_path") })
	}
	if _, err := fc.FromRelative("/already-absolute"); err == nil {
		t.Fatal("accepted an already-absolute internal path")
	}
	if _, err := fc.FromRelative("../escape"); err == nil {
		t.Fatal("accepted native parent traversal")
	}
}

func TestQueryDecodesExactlyOnce(t *testing.T) {
	tests := []struct {
		query, want string
		status      int
		code        string
	}{
		{"path=%2FNotes%2Fa%2Bb%23c%3Fd%25.txt", "/Notes/a+b#c?d%.txt", 0, ""},
		{"path=%2F%252e%252e%2F%252F.txt", "/%2e%2e/%2F.txt", 0, ""},
		{"path=%2Fcafe%CC%81.txt", "/cafe\u0301.txt", 0, ""},
		{"path=%2Fa+b", "/a b", 0, ""},
		{"path=%2Fa%2Fb", "/a/b", 0, ""},
		{"path=%2Fa%2F..%2Fb", "", 400, "invalid_path"},
		{"path=%2Fbad%FF", "", 400, "invalid_path"},
		{"path=%2Fbad%00", "", 400, "invalid_path"},
		{"path=%ZZ", "", 400, "invalid_request"},
		{"path=%2Fok&ignored=%ZZ", "", 400, "invalid_request"},
		{"path=%2Fa&path=%2Fb", "", 400, "invalid_request"},
		{"", "", 400, "invalid_request"},
		{"path=", "", 400, "invalid_path"},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			got, err := fc.ParseQueryPath(test.query)
			assertProblem(t, err, test.status, test.code)
			if string(got) != test.want {
				t.Fatalf("path = %q; want %q", got, test.want)
			}
		})
	}
	original := "/a+b#c?d%2F/cafe\u0301"
	parsed, err := fc.ParseQueryPath(url.Values{"path": {original}}.Encode())
	assertProblem(t, err, 0, "")
	if string(parsed) != original {
		t.Fatalf("standard query builder did not preserve %q", original)
	}
}

func TestScopeUsesSegmentsAndExplicitNativeComparison(t *testing.T) {
	foldedNFC := func(s string) string { return cases.Fold().String(norm.NFC.String(s)) }
	tests := []struct {
		name        string
		scope, path fc.Path
		key         fc.SegmentKey
		want        bool
	}{
		{"root", "/", "/Photos/a", nil, true},
		{"same", "/Photos", "/Photos", nil, true},
		{"descendant", "/Photos", "/Photos/a", nil, true},
		{"prefix sibling", "/Photos", "/Photos-old/a", nil, false},
		{"scope is not rebased", "/Photos", "/a", nil, false},
		{"ancestor outside scope", "/Photos/a", "/Photos", nil, false},
		{"case-sensitive", "/Photos", "/photos/a", nil, false},
		{"case-folded backend", "/Photos", "/photos/a", foldedNFC, true},
		{"exact Unicode", "/café", "/cafe\u0301/a", nil, false},
		{"native NFC lookup", "/café", "/cafe\u0301/a", norm.NFC.String, true},
		{"invalid candidate", "/", "/Photos/../private", nil, false},
		{"invalid scope", "", "/Photos", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := test.path
			if got := fc.WithinScope(test.scope, test.path, test.key); got != test.want {
				t.Fatalf("WithinScope = %v; want %v", got, test.want)
			}
			if test.path != original {
				t.Fatal("scope comparison rewrote a path")
			}
		})
	}
	// Both endpoints require a scope check; a move changes the accessible path.
	if !fc.WithinScope("/Photos", "/Photos/a", nil) || fc.WithinScope("/Photos", "/Archive/a", nil) {
		t.Fatal("source/destination scope fixture is incorrect")
	}
}

func TestRootBindingIdentityExcludesCredentialRotation(t *testing.T) {
	original := fc.RootBinding{OwnerID: "owner-a", ConnectionID: "connection-a", Generation: 7, ContentRoot: "drive/owner-a/", SystemRoot: "system/owner-a/"}
	// Credentials are intentionally absent from the comparable identity. These
	// fixtures define binding equality, not actual ownership or root isolation.
	rotated := original
	if rotated != original {
		t.Fatal("unchanged root identity differs")
	}
	changes := map[string]func(*fc.RootBinding){
		"owner":        func(b *fc.RootBinding) { b.OwnerID = "owner-b" },
		"connection":   func(b *fc.RootBinding) { b.ConnectionID = "connection-b" },
		"generation":   func(b *fc.RootBinding) { b.Generation++ },
		"content root": func(b *fc.RootBinding) { b.ContentRoot = "drive/new/" },
		"system root":  func(b *fc.RootBinding) { b.SystemRoot = "system/new/" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := original
			change(&changed)
			if changed == original {
				t.Fatal("root identity did not change")
			}
		})
	}
}
