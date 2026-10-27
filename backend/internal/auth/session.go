package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/store"
)

const SessionCookie = "eam_session"

// Service 收敛账号、密码和会话的全部数据库操作。
type Service struct {
	db  *store.DB
	cfg *config.Config
	// linkKey 只用于派生保存链接里的 MAC。它不落库，改了之后旧链接会失效。
	linkKey []byte
}

func New(db *store.DB, cfg *config.Config) *Service {
	// 复用部署时已经存在的密钥做派生；没有 COS 密钥的纯数据库命令用库地址兜底。
	seed := cfg.COS.SecretKey
	if seed == "" {
		seed = cfg.DatabaseURL
	}
	sum := sha256.Sum256([]byte("eam/save-link/v1:" + seed))
	return &Service{db: db, cfg: cfg, linkKey: sum[:]}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// CreateSession 生成 token，库里只存它的 SHA-256。
func (s *Service) CreateSession(ctx context.Context, userID, userAgent, ip string) (string, time.Time, error) {
	token, err := randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt := time.Now().Add(s.cfg.SessionTTL())

	var uaArg, ipArg any
	if userAgent != "" {
		uaArg = userAgent
	}
	if ip != "" {
		ipArg = ip
	}

	_, err = s.db.Pool().Exec(ctx,
		`insert into sessions (token_hash, user_id, expires_at, user_agent, ip)
		 values ($1, $2::uuid, $3, $4, $5)`,
		hashToken(token), userID, expiresAt, uaArg, ipArg)
	if err != nil {
		return "", time.Time{}, err
	}
	// 顺手清掉过期会话，避免表无限增长
	_, _ = s.db.Pool().Exec(ctx, `delete from sessions where expires_at < now()`)
	return token, expiresAt, nil
}

func (s *Service) sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// SessionUser 返回当前登录用户；未登录返回 (nil, nil)。
func (s *Service) SessionUser(ctx context.Context, r *http.Request) (*User, error) {
	token := s.sessionToken(r)
	if token == "" {
		return nil, nil
	}
	row, err := scanUserRow(s.db.Pool().QueryRow(ctx,
		`select `+userColumns("u")+`
		   from sessions s
		   join users u on u.id = s.user_id
		  where s.token_hash = $1
		    and s.expires_at > now()
		  limit 1`, hashToken(token)))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}
	return ToUser(row), nil
}

// RequireUser 是接口层的登录闸门。
func (s *Service) RequireUser(ctx context.Context, r *http.Request) (*User, error) {
	user, err := s.SessionUser(ctx, r)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, httpx.Unauthorized("not signed in")
	}
	return user, nil
}

func (s *Service) SetSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.CookieSecure,
		Expires:  expiresAt,
	})
}

func (s *Service) ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.CookieSecure,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
	})
}

func (s *Service) DestroySession(ctx context.Context, token string) error {
	_, err := s.db.Pool().Exec(ctx, `delete from sessions where token_hash = $1`, hashToken(token))
	return err
}

// Logout 删库里的会话并清 cookie。
func (s *Service) Logout(ctx context.Context, r *http.Request, w http.ResponseWriter) error {
	if token := s.sessionToken(r); token != "" {
		if err := s.DestroySession(ctx, token); err != nil {
			return err
		}
	}
	s.ClearSessionCookie(w)
	return nil
}
