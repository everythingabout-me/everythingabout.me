package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/scrypt"
)

// 参数存在哈希串里，以后调高成本不影响老用户登录。
// 和改造前 Node 版本保持完全一致，已有账号的密码可以直接验证。
const (
	scryptN      = 32768
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 64
	scryptMaxMem = 128 * 1024 * 1024
)

// HashPassword 生成 scrypt$N$r$p$salt$hash 格式的哈希串。
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		"scrypt",
		strconv.Itoa(scryptN),
		strconv.Itoa(scryptR),
		strconv.Itoa(scryptP),
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key),
	}, "$"), nil
}

// VerifyPassword 校验密码。哈希串格式不对、算法未知、长度不符都返回 false，
// 不抛异常，避免把数据库里的脏数据变成 500。
func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}

	n, errN := strconv.Atoi(parts[1])
	r, errR := strconv.Atoi(parts[2])
	p, errP := strconv.Atoi(parts[3])
	if errN != nil || errR != nil || errP != nil {
		return false
	}
	// 防止被篡改的参数把内存打爆：N 上限 2^20，r/p 上限 32
	if n < 1024 || n > 1_048_576 || r < 1 || r > 32 || p < 1 || p > 32 {
		return false
	}

	salt, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	if len(salt) == 0 || len(expected) == 0 || len(expected) > 256 {
		return false
	}

	actual, err := scrypt.Key([]byte(password), salt, n, r, p, len(expected))
	if err != nil || len(actual) != len(expected) {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

var (
	dummyOnce sync.Once
	dummyHash string
	dummyErr  error
)

// BurnPasswordCheck 在用户不存在时也跑一次 scrypt，
// 避免通过响应时间枚举邮箱。
func BurnPasswordCheck(password string) {
	dummyOnce.Do(func() {
		random := make([]byte, 24)
		_, _ = rand.Read(random)
		dummyHash, dummyErr = HashPassword(base64.RawURLEncoding.EncodeToString(random))
	})
	if dummyErr == nil {
		_ = VerifyPassword(password, dummyHash)
	}
}

// randomToken 生成 32 字节的 URL-safe token（和 Node 的 base64url 一致）。
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
