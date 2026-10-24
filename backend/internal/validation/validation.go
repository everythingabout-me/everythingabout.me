// Package validation 只做“像不像”的宽松校验：
// 邮箱能不能收信，由验证码流程确认。
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
		return "", httpx.BadRequest("请输入邮箱")
	}
	if len(email) > 254 {
		return "", httpx.BadRequest("邮箱过长")
	}
	if !emailRe.MatchString(email) {
		return "", httpx.BadRequest("邮箱格式不正确")
	}
	return email, nil
}

func Password(value string) (string, error) {
	if value == "" {
		return "", httpx.BadRequest("请输入密码")
	}
	if len(value) < 8 {
		return "", httpx.BadRequest("密码至少 8 位")
	}
	if len(value) > 128 {
		return "", httpx.BadRequest("密码最长 128 位")
	}
	return value, nil
}

// VerificationCode 校验 6 位数字验证码。
func VerificationCode(value string) (string, error) {
	code := strings.TrimSpace(value)
	if !codeRe.MatchString(code) {
		return "", httpx.BadRequest("请输入 6 位验证码")
	}
	return code, nil
}
