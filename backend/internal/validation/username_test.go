package validation

import "testing"

func TestNormalizeUsername(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		want   string
		reject bool
	}{
		{name: "plain", input: "iloahz", want: "iloahz"},
		{name: "folded", input: "  IloAhz  ", want: "iloahz"},
		{name: "digits and hyphens", input: "eam-2024", want: "eam-2024"},
		{name: "minimum length", input: "abc", want: "abc"},
		{name: "maximum length", input: "abcdefghijabcdefghijabcdefghij", want: "abcdefghijabcdefghijabcdefghij"},
		{name: "too short", input: "ab", reject: true},
		{name: "too long", input: "abcdefghijabcdefghijabcdefghija", reject: true},
		{name: "leading hyphen", input: "-me", reject: true},
		{name: "trailing hyphen", input: "me-", reject: true},
		{name: "double hyphen", input: "me--me", reject: true},
		{name: "underscore", input: "me_me", reject: true},
		{name: "dot", input: "me.me", reject: true},
		{name: "slash", input: "me/me", reject: true},
		{name: "empty", input: "   ", reject: true},
		{name: "reserved route", input: "settings", reject: true},
		{name: "reserved share prefix", input: "s", reject: true},
		{name: "reserved entry prefix", input: "f", reject: true},
		{name: "reserved api", input: "api", reject: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeUsername(tc.input)
			if tc.reject {
				if err == nil {
					t.Fatalf("NormalizeUsername(%q) = %q, want an error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeUsername(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeUsername(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
