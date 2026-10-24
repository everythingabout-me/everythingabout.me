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
		// 只 trim 整条路径，中间的分段保留空格（和改造前一致）
		{"  照片 / 2024 / a.jpg  ", "照片 / 2024 / a.jpg"},
		// macOS 上传的是 NFD，归一化成 NFC 后才不会出现“同名两个文件”
		{"Cafe\u0301.jpg", "Café.jpg"},
	}
	for _, tc := range cases {
		got, err := Normalize(tc.input)
		if err != nil {
			t.Fatalf("Normalize(%q) 报错: %v", tc.input, err)
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
			t.Fatalf("Normalize(%q) 应该报错", input)
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
		t.Fatal("根目录下的文件不应该有祖先")
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
		t.Fatal("不属于该用户的 key 不应该被接受")
	}
}

func TestOwnerSegmentOf(t *testing.T) {
	owner := "11111111-2222-3333-4444-555555555555"
	if got := OwnerSegmentOf("drive/" + owner + "/a.jpg"); got != owner {
		t.Fatalf("OwnerSegmentOf = %q", got)
	}
	if got := OwnerSegmentOf("drive/Photos/a.jpg"); got != "" {
		t.Fatalf("普通路径不应该识别出 owner，得到 %q", got)
	}
	if got := OwnerSegmentOf("_trash/x"); got != "" {
		t.Fatalf("非 drive 前缀不应该识别出 owner，得到 %q", got)
	}
}
