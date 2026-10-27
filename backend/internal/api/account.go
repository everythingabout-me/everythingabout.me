package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/emailcode"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/validation"
)

type emailBody struct {
	Email string `json:"email"`
}

type verifyBody struct {
	Email string `json:"email"`
	Code  string `json:"code"`
}

type loginBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type passwordBody struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"currentPassword"`
}

type changeEmailBody struct {
	Email   string `json:"email"`
	OldCode string `json:"oldCode"`
	Code    string `json:"code"`
}

const (
	hour      = time.Hour
	quarter   = 15 * time.Minute
	oneMinute = time.Minute
)

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	user, err := s.currentUser(r)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"user": user})
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	legacyCount, err := s.auth.LegacyNodeCount(r.Context())
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"user": user, "legacyCount": legacyCount})
}

/** 登录 / 注册共用的验证码发送接口 */
func (s *Server) handleSendCode(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	if s.cfg.IsProduction() && !s.mail.Configured() {
		return httpx.ServiceUnavailable("email service is not configured; cannot send verification codes")
	}

	ip := httpx.ClientIP(r)
	if err := s.limiter.Enforce("signin-code:ip:"+ip, 30, hour); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[emailBody](r)
	if err != nil {
		return err
	}
	email, err := validation.NormalizeEmail(body.Email)
	if err != nil {
		return err
	}

	existing, err := s.auth.FindUserByEmail(r.Context(), email)
	if err != nil {
		return err
	}
	if existing == nil && !s.cfg.RegistrationOpen {
		return httpx.Forbidden("sign-up is closed")
	}

	if err := s.limiter.Enforce("signin-code:email:"+email, 5, hour); err != nil {
		return err
	}
	// 同一邮箱 60 秒内只能发一次；界面上也会倒计时
	if err := s.limiter.Enforce("signin-code:cooldown:"+email, 1, oneMinute); err != nil {
		return err
	}

	code, err := s.codes.Issue(r.Context(), email, "signin", ip)
	if err != nil {
		return err
	}
	devCode, err := s.mail.SendVerificationCode(r.Context(), email, code)
	if err != nil {
		// 没发出去就别留一个用户永远收不到的可用验证码
		_ = s.codes.Revoke(r.Context(), email, "signin")
		return err
	}
	payload := map[string]any{"expiresIn": int(emailcode.TTL.Seconds())}
	if devCode != "" {
		payload["devCode"] = devCode
	}
	return s.ok(w, payload)
}

/** 验证码登录；邮箱不存在且注册开放时，自动创建一个无密码账号 */
func (s *Server) handleVerifyCode(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	ip := httpx.ClientIP(r)
	if err := s.limiter.Enforce("verify:ip:"+ip, 30, quarter); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[verifyBody](r)
	if err != nil {
		return err
	}
	email, err := validation.NormalizeEmail(body.Email)
	if err != nil {
		return err
	}
	code, err := validation.VerificationCode(body.Code)
	if err != nil {
		return err
	}
	if err := s.limiter.Enforce("verify:email:"+email, 10, quarter); err != nil {
		return err
	}

	user, err := s.auth.FindUserByEmail(r.Context(), email)
	if err != nil {
		return err
	}
	existing := user != nil
	if user == nil && !s.cfg.RegistrationOpen {
		return httpx.Forbidden("sign-up is closed")
	}

	// 兼容旧流程发出去的 register 验证码
	if err := s.codes.Consume(r.Context(), email, []string{"signin", "register"}, code); err != nil {
		return err
	}

	if user == nil {
		user, err = s.auth.CreateUser(r.Context(), email, nil)
		if err != nil {
			if !auth.IsUniqueViolation(err) {
				return err
			}
			user, err = s.auth.FindUserByEmail(r.Context(), email)
			if err != nil {
				return err
			}
		}
	}
	if user == nil {
		return httpx.Internal("failed to create account; please retry")
	}

	if err := s.auth.TouchLastLogin(r.Context(), user.ID); err != nil {
		return err
	}
	if err := s.startSession(w, r, user.ID); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"user": auth.ToUser(user), "created": !existing})
}

func (s *Server) handlePasswordLogin(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	ip := httpx.ClientIP(r)
	if err := s.limiter.Enforce("login:"+ip, 30, quarter); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[loginBody](r)
	if err != nil {
		return err
	}
	email, err := validation.NormalizeEmail(body.Email)
	if err != nil {
		return err
	}
	password := body.Password
	if password == "" {
		return httpx.BadRequest("password is required")
	}
	if len(password) > 128 {
		return httpx.Unauthorized("incorrect email or password")
	}
	if err := s.limiter.Enforce("login:"+ip+":"+email, 10, quarter); err != nil {
		return err
	}

	user, err := s.auth.FindUserByEmail(r.Context(), email)
	if err != nil {
		return err
	}
	if user == nil {
		auth.BurnPasswordCheck(password)
		return httpx.Unauthorized("incorrect email or password")
	}
	if user.PasswordHash == nil {
		return httpx.Unauthorized("this account has no password; sign in with an email code")
	}
	if !auth.VerifyPassword(password, *user.PasswordHash) {
		return httpx.Unauthorized("incorrect email or password")
	}

	if err := s.auth.TouchLastLogin(r.Context(), user.ID); err != nil {
		return err
	}
	if err := s.startSession(w, r, user.ID); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"user": auth.ToUser(user)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	if err := s.auth.Logout(r.Context(), r, w); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"ok": true})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID string) error {
	token, expiresAt, err := s.auth.CreateSession(
		r.Context(), userID, r.UserAgent(), httpx.ClientIP(r),
	)
	if err != nil {
		return err
	}
	s.auth.SetSessionCookie(w, token, expiresAt)
	return nil
}

/** 设置或修改密码；账号已有密码时必须先验证当前密码 */
func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	current, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.limiter.Enforce("password:"+current.ID, 10, hour); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[passwordBody](r)
	if err != nil {
		return err
	}
	password, err := validation.Password(body.Password)
	if err != nil {
		return err
	}

	row, err := s.auth.FindUserByID(r.Context(), current.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return httpx.Unauthorized("not signed in")
	}

	if row.PasswordHash != nil {
		if body.CurrentPassword == "" {
			return httpx.BadRequest("current password is required")
		}
		if len(body.CurrentPassword) > 128 ||
			!auth.VerifyPassword(body.CurrentPassword, *row.PasswordHash) {
			return httpx.Unauthorized("current password is incorrect")
		}
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	updated, err := s.auth.SetUserPassword(r.Context(), row.ID, &hash)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"user": auth.ToUser(updated)})
}

/** 移除密码，之后只能靠邮箱验证码登录；需要当前密码 */
func (s *Server) handleRemovePassword(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	current, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.limiter.Enforce("password-remove:"+current.ID, 10, hour); err != nil {
		return err
	}

	row, err := s.auth.FindUserByID(r.Context(), current.ID)
	if err != nil {
		return err
	}
	if row == nil {
		return httpx.Unauthorized("not signed in")
	}
	if row.PasswordHash == nil {
		return httpx.BadRequest("this account has no password set")
	}

	body, err := httpx.ReadJSON[passwordBody](r)
	if err != nil {
		return err
	}
	if body.CurrentPassword == "" {
		return httpx.BadRequest("current password is required")
	}
	if len(body.CurrentPassword) > 128 ||
		!auth.VerifyPassword(body.CurrentPassword, *row.PasswordHash) {
		return httpx.Unauthorized("current password is incorrect")
	}

	updated, err := s.auth.SetUserPassword(r.Context(), row.ID, nil)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"user": auth.ToUser(updated)})
}

/** 给「当前邮箱」发验证码，用于改邮箱前的身份确认 */
func (s *Server) handleCurrentEmailCode(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	current, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if s.cfg.IsProduction() && !s.mail.Configured() {
		return httpx.ServiceUnavailable("email service is not configured; cannot send verification codes")
	}

	ip := httpx.ClientIP(r)
	if err := s.limiter.Enforce("email-change-old:ip:"+ip, 30, hour); err != nil {
		return err
	}
	if err := s.limiter.Enforce("email-change-old:user:"+current.ID, 10, hour); err != nil {
		return err
	}
	if err := s.limiter.Enforce("email-change-old:email:"+current.Email, 5, hour); err != nil {
		return err
	}
	if err := s.limiter.Enforce("email-change-old:cooldown:"+current.Email, 1, oneMinute); err != nil {
		return err
	}

	code, err := s.codes.Issue(r.Context(), current.Email, "change_email_old", ip)
	if err != nil {
		return err
	}
	devCode, err := s.mail.SendVerificationCode(r.Context(), current.Email, code)
	if err != nil {
		_ = s.codes.Revoke(r.Context(), current.Email, "change_email_old")
		return err
	}
	payload := map[string]any{"expiresIn": int(emailcode.TTL.Seconds())}
	if devCode != "" {
		payload["devCode"] = devCode
	}
	return s.ok(w, payload)
}

/** 给「新邮箱」发验证码；要求已登录 */
func (s *Server) handleNewEmailCode(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	current, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if s.cfg.IsProduction() && !s.mail.Configured() {
		return httpx.ServiceUnavailable("email service is not configured; cannot send verification codes")
	}

	ip := httpx.ClientIP(r)
	if err := s.limiter.Enforce("email-change-code:ip:"+ip, 30, hour); err != nil {
		return err
	}
	if err := s.limiter.Enforce("email-change-code:user:"+current.ID, 10, hour); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[emailBody](r)
	if err != nil {
		return err
	}
	email, err := validation.NormalizeEmail(body.Email)
	if err != nil {
		return err
	}
	if email == current.Email {
		return httpx.BadRequest("the new email is the same as the current one")
	}
	existing, err := s.auth.FindUserByEmail(r.Context(), email)
	if err != nil {
		return err
	}
	if existing != nil {
		return httpx.Conflict("this email is already used by another account")
	}

	if err := s.limiter.Enforce("email-change-code:email:"+email, 5, hour); err != nil {
		return err
	}
	if err := s.limiter.Enforce("email-change-code:cooldown:"+email, 1, oneMinute); err != nil {
		return err
	}

	code, err := s.codes.Issue(r.Context(), email, "change_email", ip)
	if err != nil {
		return err
	}
	devCode, err := s.mail.SendVerificationCode(r.Context(), email, code)
	if err != nil {
		_ = s.codes.Revoke(r.Context(), email, "change_email")
		return err
	}
	payload := map[string]any{"expiresIn": int(emailcode.TTL.Seconds())}
	if devCode != "" {
		payload["devCode"] = devCode
	}
	return s.ok(w, payload)
}

/** 修改邮箱：当前邮箱和新邮箱的两个验证码都必须正确 */
func (s *Server) handleChangeEmail(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	current, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.limiter.Enforce("email-change:"+current.ID, 10, hour); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[changeEmailBody](r)
	if err != nil {
		return err
	}
	email, err := validation.NormalizeEmail(body.Email)
	if err != nil {
		return err
	}
	oldCode := strings.TrimSpace(body.OldCode)
	code := strings.TrimSpace(body.Code)
	if !isSixDigits(oldCode) {
		return httpx.BadRequest("current email verification code is required")
	}
	if !isSixDigits(code) {
		return httpx.BadRequest("new email verification code is required")
	}
	if email == current.Email {
		return httpx.BadRequest("the new email is the same as the current one")
	}
	existing, err := s.auth.FindUserByEmail(r.Context(), email)
	if err != nil {
		return err
	}
	if existing != nil {
		return httpx.Conflict("this email is already used by another account")
	}

	// 先都验证通过，再一起消费：不会因为其中一个填错而把另一个作废
	oldID, err := s.codes.VerifyLatest(r.Context(), current.Email, []string{"change_email_old"}, oldCode)
	if err != nil {
		return labelCodeError(err, "current email")
	}
	newID, err := s.codes.VerifyLatest(r.Context(), email, []string{"change_email"}, code)
	if err != nil {
		return labelCodeError(err, "new email")
	}
	if err := s.codes.ConsumeByID(r.Context(), oldID); err != nil {
		return err
	}
	if err := s.codes.ConsumeByID(r.Context(), newID); err != nil {
		return err
	}

	updated, err := s.auth.UpdateUserEmail(r.Context(), current.ID, email)
	if err != nil {
		if auth.IsUniqueViolation(err) {
			return httpx.Conflict("this email is already used by another account")
		}
		return err
	}
	return s.ok(w, map[string]any{"user": auth.ToUser(updated)})
}

func isSixDigits(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func labelCodeError(err error, label string) error {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) && apiErr == emailcode.ErrIncorrect {
		return httpx.BadRequest(label + ": incorrect verification code")
	}
	return err
}
