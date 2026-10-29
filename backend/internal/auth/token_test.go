package auth

import (
	"strings"
	"testing"
	"time"
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

func TestSaveLinkCode(t *testing.T) {
	service := &Service{linkKey: []byte("test-key")}
	userID := "7f3a9c2b-0000-0000-0000-000000000000"
	linkID := "abcd2345"

	code := service.saveLinkCode(userID, linkID)
	if len(code) != saveLinkLen {
		t.Fatalf("code length = %d, want %d", len(code), saveLinkLen)
	}
	if code[:saveLinkIDLen] != linkID {
		t.Fatalf("code should start with the link id: %q", code)
	}

	gotID, gotMAC, ok := parseSaveLinkCode(code)
	if !ok {
		t.Fatalf("generated code should parse: %q", code)
	}
	if gotID != linkID || gotMAC != code[saveLinkIDLen:] {
		t.Fatalf("parsed (%q, %q), want (%q, %q)", gotID, gotMAC, linkID, code[saveLinkIDLen:])
	}
	if again := service.saveLinkCode(userID, linkID); again != code {
		t.Fatalf("code should be deterministic: %q != %q", again, code)
	}
	if other := service.saveLinkCode(userID, "abcd2346"); other == code {
		t.Fatal("different link ids should produce different codes")
	}
}

func TestParseSaveLinkCode(t *testing.T) {
	cases := []struct {
		name string
		code string
		ok   bool
	}{
		{"valid", "abcd2345efgh2345", true},
		{"uppercase is normalized", "ABCD2345EFGH2345", true},
		{"from a bearer token", "eam_abcd2345efgh_6789", false},
		{"too short", "abcd", false},
		{"invalid character", "abcd2345efgh6780", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, ok := parseSaveLinkCode(tc.code)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}

func TestNormalizeKind(t *testing.T) {
	cases := map[string]string{"": KindApp, "app": KindApp, "token": KindToken}
	for input, want := range cases {
		got, err := normalizeKind(input)
		if err != nil || got != want {
			t.Fatalf("normalizeKind(%q) = (%q, %v), want %q", input, got, err, want)
		}
	}
	if _, err := normalizeKind("agent"); err == nil {
		t.Fatal("an unknown kind should be rejected")
	}
}

func TestNormalizeName(t *testing.T) {
	name, err := normalizeName(KindApp, "   ")
	if err != nil || name != "" {
		t.Fatalf("an app may start nameless: (%q, %v)", name, err)
	}
	if _, err := normalizeName(KindToken, "  "); err == nil {
		t.Fatal("a token needs a name")
	}
	name, err = normalizeName(KindToken, "  Claude Code  ")
	if err != nil || name != "Claude Code" {
		t.Fatalf("a name should be trimmed: (%q, %v)", name, err)
	}
	if _, err := normalizeName(KindApp, strings.Repeat("界", 81)); err == nil {
		t.Fatal("a name longer than 80 runes should be rejected")
	}
	if _, err := normalizeName(KindApp, strings.Repeat("界", 80)); err != nil {
		t.Fatalf("80 runes should be allowed regardless of byte size: %v", err)
	}
}

func TestValidateLifetime(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	hour := now.Add(time.Hour)
	week := now.Add(7 * 24 * time.Hour)
	year := now.Add(366 * 24 * time.Hour)

	if err := validateLifetime(KindApp, nil, now); err != nil {
		t.Fatalf("an app may be permanent: %v", err)
	}
	if err := validateLifetime(KindToken, nil, now); err == nil {
		t.Fatal("a token must expire")
	}
	if err := validateLifetime(KindToken, &hour, now); err != nil {
		t.Fatalf("a one-hour token should be allowed: %v", err)
	}
	if err := validateLifetime(KindToken, &week, now); err != nil {
		t.Fatalf("a seven-day token should be allowed: %v", err)
	}
	if err := validateLifetime(KindToken, &year, now); err == nil {
		t.Fatal("a token longer than seven days should be rejected")
	}
	if err := validateLifetime(KindApp, &year, now); err == nil {
		t.Fatal("an app longer than 365 days should be rejected")
	}
	past := now.Add(-time.Minute)
	if err := validateLifetime(KindApp, &past, now); err == nil {
		t.Fatal("an expiry in the past should be rejected")
	}
}
