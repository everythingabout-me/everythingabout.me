package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/store"
)

const SessionCookie = "eam_session"

// Service centralizes every database operation for accounts, passwords, and sessions.
type Service struct {
	db  *store.DB
	cfg *config.Config
	// linkKey only derives the MAC inside save links. It is never stored, so changing it
	// invalidates existing links.
	linkKey []byte
}

func New(db *store.DB, cfg *config.Config) *Service {
	// Keep the derivation stable so an explicitly preserved legacy seed retains
	// existing links. Server startup validates APP_SECRET before creating services.
	sum := sha256.Sum256([]byte("eam/save-link/v1:" + cfg.AppSecret))
	return &Service{db: db, cfg: cfg, linkKey: sum[:]}
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum)
}

// CreateSession mints a token; only its SHA-256 is stored.
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
	// Sweep expired sessions while we are here so the table does not grow forever
	_, _ = s.db.Pool().Exec(ctx, `delete from sessions where expires_at < now()`)
	return token, expiresAt, nil
}

// SessionToken returns the raw session cookie value of the request, or "" when the
// request carries no session cookie. Callers that must name the session they are acting
// in (revoking the others) use it; the hash is what the database stores.
func (s *Service) SessionToken(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// SessionUser returns the signed-in user, or (nil, nil) when nobody is signed in.
func (s *Service) SessionUser(ctx context.Context, r *http.Request) (*User, error) {
	token := s.SessionToken(r)
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

// RequireUser is the login gate at the API layer.
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

// execer is the part of pgxpool.Pool and pgx.Tx one statement needs, so a credential
// change can revoke sessions inside the transaction that changes the credential.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// revokeOtherSessions deletes the account's login sessions except the one that made the
// current request, and returns how many rows it removed.
//
// The rows deleted are all rows of sessions with this user_id whose token_hash is not the
// SHA-256 of currentToken: the stolen or forgotten cookies, with their user_agent and ip.
// The row behind currentToken is exempt because it is the session that performed this
// change — the browser tab the owner is standing in and the only session they still
// demonstrably control. Deleting it would log them out of the action they just took,
// which teaches people to avoid password changes instead of protecting them.
//
// Why revoke at all: a stolen cookie is the credential an attacker keeps after the owner
// resets the password or moves the account to a new email address. The recovery action is
// only a recovery if it ends those sessions; otherwise the attacker stays signed in on
// the old token for the rest of SESSION_TTL_DAYS. An empty currentToken matches no stored
// hash, so it fails closed and revokes every session; the account handlers only reach
// here behind RequireUser, which already required a valid session cookie.
func revokeOtherSessions(ctx context.Context, q execer, userID, currentToken string) (int64, error) {
	tag, err := q.Exec(ctx,
		`delete from sessions
		  where user_id = $1::uuid
		    and token_hash <> $2`,
		userID, hashToken(currentToken))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Logout deletes the session row and clears the cookie.
func (s *Service) Logout(ctx context.Context, r *http.Request, w http.ResponseWriter) error {
	if token := s.SessionToken(r); token != "" {
		if err := s.DestroySession(ctx, token); err != nil {
			return err
		}
	}
	s.ClearSessionCookie(w)
	return nil
}
