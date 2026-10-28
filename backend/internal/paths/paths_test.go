package paths

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"/", ""},
		{"Photos/2024/a.jpg", "Photos/2024/a.jpg"},
		{"/Photos//2024///a.jpg/", "Photos/2024/a.jpg"},
		{"./Photos/./a.jpg", "Photos/a.jpg"},
		{`Photos\2024\a.jpg`, "Photos/2024/a.jpg"},
		{"  Photos/a.jpg  ", "Photos/a.jpg"},
		// Only the whole path is trimmed; interior segments keep their spaces (as before the rewrite)
		{"  照片 / 2024 / a.jpg  ", "照片 / 2024 / a.jpg"},
		// macOS uploads NFD; only after normalizing to NFC do we avoid "two files with the same name"
		{"Cafe\u0301.jpg", "Café.jpg"},
	}
	for _, tc := range cases {
		got, err := Normalize(tc.input)
		if err != nil {
			t.Fatalf("Normalize(%q) returned an error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Fatalf("Normalize(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestNormalizeRejectsBadPaths(t *testing.T) {
	cases := []string{
		"..",
		"../etc/passwd",
		"Photos/../../secret",
		"a\x00b",
		"a\nb",
		strings.Repeat("d/", MaxPathDepth+1) + "x",
		strings.Repeat("长", MaxNameLength+1),
	}
	for _, input := range cases {
		if _, err := Normalize(input); err == nil {
			t.Fatalf("Normalize(%q) should return an error", input)
		}
	}
}

func TestParentAndName(t *testing.T) {
	if got := ParentOf("a/b/c.jpg"); got != "a/b" {
		t.Fatalf("ParentOf = %q", got)
	}
	if got := ParentOf("a.jpg"); got != "" {
		t.Fatalf("ParentOf = %q", got)
	}
	if got := NameOf("a/b/c.jpg"); got != "c.jpg" {
		t.Fatalf("NameOf = %q", got)
	}
	if got := NameOf("c.jpg"); got != "c.jpg" {
		t.Fatalf("NameOf = %q", got)
	}
}

func TestAncestors(t *testing.T) {
	got := AncestorsOf("a/b/c/d.jpg")
	want := []string{"a", "a/b", "a/b/c"}
	if len(got) != len(want) {
		t.Fatalf("AncestorsOf = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AncestorsOf = %v, want %v", got, want)
		}
	}
	if len(AncestorsOf("a.jpg")) != 0 {
		t.Fatal("a file at the root should have no ancestors")
	}
}

func TestKeys(t *testing.T) {
	owner := "11111111-2222-3333-4444-555555555555"
	if got := KeyFor("Photos/a.jpg", owner); got != "drive/"+owner+"/Photos/a.jpg" {
		t.Fatalf("KeyFor = %q", got)
	}
	if got := DirKeyFor("Photos", owner); got != "drive/"+owner+"/Photos/" {
		t.Fatalf("DirKeyFor = %q", got)
	}
	if got := DirKeyFor("", owner); got != "drive/"+owner+"/" {
		t.Fatalf("DirKeyFor root = %q", got)
	}

	path, ok := PathFromKey("drive/"+owner+"/Photos/a.jpg", owner)
	if !ok || path != "Photos/a.jpg" {
		t.Fatalf("PathFromKey = %q, %v", path, ok)
	}
	path, ok = PathFromKey("drive/"+owner+"/Photos/", owner)
	if !ok || path != "Photos" {
		t.Fatalf("PathFromKey dir = %q, %v", path, ok)
	}
	if _, ok := PathFromKey("drive/other/Photos/a.jpg", owner); ok {
		t.Fatal("a key owned by another user should be rejected")
	}
}

func TestOwnerSegmentOf(t *testing.T) {
	owner := "11111111-2222-3333-4444-555555555555"
	if got := OwnerSegmentOf("drive/" + owner + "/a.jpg"); got != owner {
		t.Fatalf("OwnerSegmentOf = %q", got)
	}
	if got := OwnerSegmentOf("drive/Photos/a.jpg"); got != "" {
		t.Fatalf("a plain path should have no owner, got %q", got)
	}
	if got := OwnerSegmentOf("_inventory/x"); got != "" {
		t.Fatalf("a non-drive prefix should have no owner, got %q", got)
	}
}
