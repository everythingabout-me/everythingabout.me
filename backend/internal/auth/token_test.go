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
		{"合法", "eam_" + id + "_" + secret, true, id, secret},
		{"前后空格", "  eam_" + id + "_" + secret + " ", true, id, secret},
		{"没有前缀", id + "_" + secret, false, "", ""},
		{"id 太短", "eam_abc_" + secret, false, "", ""},
		{"没有 secret", "eam_" + id + "_", false, "", ""},
		{"没有分隔符", "eam_" + id, false, "", ""},
		{"空串", "", false, "", ""},
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
		t.Fatal("同一个 secret 两次哈希结果不同")
	}
	if len(first) != 64 || strings.Contains(first, "hello") {
		t.Fatalf("哈希形状不对: %q", first)
	}
	if hashSecret("hello") == hashSecret("hello!") {
		t.Fatal("不同 secret 哈希撞了")
	}
}

func TestPrincipalScope(t *testing.T) {
	limited := &Principal{Token: &Token{Prefix: "Apps/badminton", CanRead: true, CanWrite: false}}

	if err := limited.RequireRead("Apps/badminton/a.json"); err != nil {
		t.Fatalf("前缀内读应该允许: %v", err)
	}
	if err := limited.RequireRead("Apps/badminton"); err != nil {
		t.Fatalf("前缀本身应该允许: %v", err)
	}
	if err := limited.RequireRead("Apps/other/a.json"); err == nil {
		t.Fatal("前缀外读应该被拒绝")
	}
	if err := limited.RequireWrite("Apps/badminton/a.json"); err == nil {
		t.Fatal("只读令牌写应该被拒绝")
	}

	writable := &Principal{Token: &Token{Prefix: "", CanRead: true, CanWrite: true}}
	if err := writable.RequireWrite("anywhere/x"); err != nil {
		t.Fatalf("空前缀应该代表整个盘: %v", err)
	}

	full := &Principal{}
	if err := full.RequireWrite("anywhere/x"); err != nil {
		t.Fatalf("cookie 会话应该全权限: %v", err)
	}
	if !full.FullAccess() {
		t.Fatal("cookie 会话应该 FullAccess")
	}
}
