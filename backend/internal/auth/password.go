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

// The parameters live in the hash string, so raising the cost later does not break
// existing logins. Kept identical to the pre-rewrite Node version so passwords on
// existing accounts verify as-is.
const (
	scryptN      = 32768
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 64
	scryptMaxMem = 128 * 1024 * 1024
)

// HashPassword produces a hash string in scrypt$N$r$p$salt$hash form.
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

// VerifyPassword checks a password. A malformed hash string, an unknown algorithm, or a
// length mismatch all return false rather than an error, so a dirty row in the database
// never turns into a 500.
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
	// Keep tampered parameters from blowing up memory: N capped at 2^20, r/p at 32.
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

// BurnPasswordCheck still runs scrypt when the user does not exist, so response
// timing cannot be used to enumerate email addresses.
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

// randomToken returns a 32-byte URL-safe token (same as Node's base64url).
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
