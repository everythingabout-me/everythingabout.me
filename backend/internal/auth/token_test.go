package auth

import (
	"strings"
	"testing"
)

func TestParseToken(t *testing.T) {
	id := "7f3a9c2b1d4e"
	secret := "abc_DEF-123"

	cases := []struct {
		name       string
		input      string
		wantOK     bool
		wantID     string
		wantSecret string
	}{
		{"valid", "eam_" + id + "_" + secret, true, id, secret},
		{"surrounding spaces", "  eam_" + id + "_" + secret + " ", true, id, secret},
		{"no prefix", id + "_" + secret, false, "", ""},
		{"id too short", "eam_abc_" + secret, false, "", ""},
		{"no secret", "eam_" + id + "_", false, "", ""},
		{"no separator", "eam_" + id, false, "", ""},
		{"empty string", "", false, "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotID, gotSecret, ok := ParseToken(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if gotID != tc.wantID || gotSecret != tc.wantSecret {
				t.Fatalf("got (%q, %q), want (%q, %q)", gotID, gotSecret, tc.wantID, tc.wantSecret)
			}
		})
	}
}

func TestHashSecretIsStable(t *testing.T) {
	first := hashSecret("hello")
	second := hashSecret("hello")
	if first != second {
		t.Fatal("hashing the same secret twice gives different results")
	}
	if len(first) != 64 || strings.Contains(first, "hello") {
		t.Fatalf("unexpected hash shape: %q", first)
	}
	if hashSecret("hello") == hashSecret("hello!") {
		t.Fatal("different secrets collided")
	}
}

func TestPrincipalScope(t *testing.T) {
	limited := &Principal{Token: &Token{Prefix: "Apps/badminton", CanRead: true, CanWrite: false}}

	if err := limited.RequireRead("Apps/badminton/a.json"); err != nil {
		t.Fatalf("read inside the prefix should be allowed: %v", err)
	}
	if err := limited.RequireRead("Apps/badminton"); err != nil {
		t.Fatalf("the prefix itself should be allowed: %v", err)
	}
	if err := limited.RequireRead("Apps/other/a.json"); err == nil {
		t.Fatal("read outside the prefix should be denied")
	}
	if err := limited.RequireWrite("Apps/badminton/a.json"); err == nil {
		t.Fatal("write with a read-only token should be denied")
	}

	writable := &Principal{Token: &Token{Prefix: "", CanRead: true, CanWrite: true}}
	if err := writable.RequireWrite("anywhere/x"); err != nil {
		t.Fatalf("an empty prefix means the whole drive: %v", err)
	}

	full := &Principal{}
	if err := full.RequireWrite("anywhere/x"); err != nil {
		t.Fatalf("a cookie session should have full access: %v", err)
	}
	if !full.FullAccess() {
		t.Fatal("a cookie session should be FullAccess")
	}
}
