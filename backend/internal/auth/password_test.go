package auth

import "testing"

// 这个哈希串是改造前 Node 版本生成的（node:crypto scrypt，同样参数），
// 用来保证迁移后老账号还能用原密码登录。
const nodeGeneratedHash = "scrypt$32768$8$1$s163ra+//Jr/xapHVQBItQ==$1m7Hhu/9baIFdr0Pze6LZp18RIDIt+aity38wMIAYslA4mXeXyav1MZkrLCP/ojqd7wEpjE5xkqjE9sF8HHa5Q=="

func TestVerifyPasswordAcceptsNodeGeneratedHash(t *testing.T) {
	if !VerifyPassword("test12345", nodeGeneratedHash) {
		t.Fatal("Node 生成的 scrypt 哈希在 Go 里验证失败")
	}
	if VerifyPassword("wrong-password", nodeGeneratedHash) {
		t.Fatal("错误密码不应该通过")
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !VerifyPassword("correct horse battery staple", hash) {
		t.Fatal("自己生成的哈希验证失败")
	}
	if VerifyPassword("Correct horse battery staple", hash) {
		t.Fatal("大小写不同的密码不应该通过")
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	cases := []string{
		"",
		"plain-text",
		"bcrypt$1$2$3$4$5",
		"scrypt$notanumber$8$1$c2FsdA==$aGFzaA==",
		"scrypt$32768$8$1$!!!$aGFzaA==",
		"scrypt$32768$8$1$c2FsdA==",
		// 参数被篡改成超大值，应该直接拒绝而不是把内存打爆
		"scrypt$999999999$8$1$c2FsdA==$aGFzaA==",
	}
	for _, stored := range cases {
		if VerifyPassword("anything", stored) {
			t.Fatalf("畸形哈希不应该通过: %q", stored)
		}
	}
}

func TestRandomTokenIsURLSafe(t *testing.T) {
	token, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(token) != 43 {
		t.Fatalf("token 长度应为 43，实际 %d", len(token))
	}
	for _, r := range token {
		valid := r == '-' || r == '_' ||
			(r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		if !valid {
			t.Fatalf("token 含有非 base64url 字符: %q", r)
		}
	}
}
