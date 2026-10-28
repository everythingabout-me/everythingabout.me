// Package validation does loose "does it look right" checks only: whether an address can
// actually receive mail is settled by the verification code flow.
package validation

import (
	"regexp"
	"strings"

	"everythingabout.me/backend/internal/httpx"
)

var (
	emailRe = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]{2,}$`)
	codeRe  = regexp.MustCompile(`^\d{6}$`)
)

func NormalizeEmail(value string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(value))
	if email == "" {
		return "", httpx.BadRequest("email is required")
	}
	if len(email) > 254 {
		return "", httpx.BadRequest("email is too long")
	}
	if !emailRe.MatchString(email) {
		return "", httpx.BadRequest("invalid email address")
	}
	return email, nil
}

func Password(value string) (string, error) {
	if value == "" {
		return "", httpx.BadRequest("password is required")
	}
	if len(value) < 8 {
		return "", httpx.BadRequest("password must be at least 8 characters")
	}
	if len(value) > 128 {
		return "", httpx.BadRequest("password must be at most 128 characters")
	}
	return value, nil
}

// VerificationCode checks a 6-digit numeric verification code.
func VerificationCode(value string) (string, error) {
	code := strings.TrimSpace(value)
	if !codeRe.MatchString(code) {
		return "", httpx.BadRequest("a 6-digit verification code is required")
	}
	return code, nil
}
