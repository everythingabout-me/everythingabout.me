package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
)

// TokenPrefix is the prefix for access tokens handed to external programs.
// The shape is eam_<12-char id>_<43-char secret>; only the secret's SHA-256 is stored.
const TokenPrefix = "eam_"

const tokenIDLength = 12

// Connection kinds. "app" is long-lived and handed over with a QR code; "token" is
// temporary and copied as a text block. Kind is a label, not a permission.
const (
	KindApp   = "app"
	KindToken = "token"
)

const (
	minTokenTTL = time.Minute
	maxAppTTL   = 365 * 24 * time.Hour
	maxTokenTTL = 7 * 24 * time.Hour
)

// Token is the shape exposed for a token (never the secret).
type Token struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"`
	Prefix        string  `json:"prefix"`
	CanRead       bool    `json:"read"`
	CanWrite      bool    `json:"write"`
	CanEvents     bool    `json:"events"`
	CanChanges    bool    `json:"changes"`
	CanSubscribe  bool    `json:"subscribe"`
	PolicyVersion int64   `json:"policyVersion"`
	CreatedAt     string  `json:"createdAt"`
	LastUsedAt    *string `json:"lastUsedAt"`
	ExpiresAt     *string `json:"expiresAt"`
	Revoked       bool    `json:"revoked"`
	RevokedAt     *string `json:"revokedAt"`

	userID string
}

// Principal is the identity of one request: either a cookie session (Token is nil, full
// access) or a restricted API token.
type Principal struct {
	User  *User
	Token *Token
}

// FullAccess reports that this is the user's own session, so no prefix limit applies.
func (p *Principal) FullAccess() bool { return p.Token == nil }

func (p *Principal) RequireRead(path string) error {
	if p.FullAccess() {
		return nil
	}
	if !p.Token.CanRead {
		return httpx.Forbidden("this token has no read permission")
	}
	if !paths.IsInside(path, p.Token.Prefix) {
		return httpx.Forbidden("this token can only access " + p.Token.Prefix)
	}
	return nil
}

// CanWrite reports whether this identity may write a given directory. Listing uses it to
// tell the client where it is allowed to write, so it does not have to probe with 403s.
func (p *Principal) CanWrite(path string) bool {
	if p.FullAccess() {
		return true
	}
	if !p.Token.CanWrite {
		return false
	}
	return paths.IsInside(path, p.Token.Prefix)
}

// RequireEvents reports whether this identity may read and write events.
// Events and files are two separate capability bits: an app may only submit, never touch files.
func (p *Principal) RequireEvents() error {
	if p.FullAccess() {
		return nil
	}
	if !p.Token.CanEvents {
		return httpx.Forbidden("this token cannot write events")
	}
	return nil
}

func (p *Principal) RequireWrite(path string) error {
	if p.FullAccess() {
		return nil
	}
	if !p.Token.CanWrite {
		return httpx.Forbidden("this token is read-only")
	}
	if !paths.IsInside(path, p.Token.Prefix) {
		return httpx.Forbidden("this token can only write " + p.Token.Prefix)
	}
	return nil
}

// ParseToken splits out token_id and secret; a malformed token returns ok=false.
func ParseToken(plain string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(plain), TokenPrefix)
	if !found {
		return "", "", false
	}
	// The secret is base64url, so it may contain "_"; split on the first underscore only.
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 || len(parts[0]) != tokenIDLength || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newTokenID() (string, error) {
	buf := make([]byte, tokenIDLength/2)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// NewTokenInput is the input for creating a token.
type NewTokenInput struct {
	UserID       string
	Name         string
	Kind         string
	Prefix       string
	CanRead      bool
	CanWrite     bool
	CanEvents    bool
	CanChanges   bool
	CanSubscribe bool
	Expiry       *time.Time
}

// normalizeKind maps an empty kind to "app", which is the historical CLI default.
func normalizeKind(kind string) (string, error) {
	switch strings.TrimSpace(kind) {
	case "", KindApp:
		return KindApp, nil
	case KindToken:
		return KindToken, nil
	default:
		return "", httpx.BadRequest("kind must be app or token")
	}
}

// validateLifetime enforces the two product lifetimes. An app may be permanent; a token
// must expire, and within a week. A small minimum keeps "expires immediately" impossible.
func validateLifetime(kind string, expiry *time.Time, now time.Time) error {
	if expiry == nil {
		if kind == KindToken {
			return httpx.BadRequest("a token needs an expiry")
		}
		return nil
	}
	if !expiry.After(now.Add(minTokenTTL)) {
		return httpx.BadRequest("the expiry must be at least 1 minute away")
	}
	if kind == KindApp && expiry.After(now.Add(maxAppTTL)) {
		return httpx.BadRequest("an app can last at most 365 days")
	}
	if kind == KindToken && expiry.After(now.Add(maxTokenTTL)) {
		return httpx.BadRequest("a token can last at most 7 days")
	}
	return nil
}

// normalizeName trims and validates the display name. An app may start nameless: the page
// only mints the credential and the app names itself through PATCH /me after scanning. A
// token is named by whoever mints it, because it is handed over by hand.
func normalizeName(kind, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" && kind != KindApp {
		return "", httpx.BadRequest("a token needs a name")
	}
	if utf8.RuneCountInString(name) > 80 {
		return "", httpx.BadRequest("the name is too long")
	}
	return name, nil
}

// validateTokenPermissions requires explicit grants for change history and subscriptions.
func validateTokenPermissions(read, write, events, changes, subscribe bool) error {
	if changes && !read {
		return httpx.BadRequest("changes permission requires read permission")
	}
	if subscribe && (!changes || !read) {
		return httpx.BadRequest("subscribe permission requires changes and read permissions")
	}
	if !read && !write && !events {
		return httpx.BadRequest("at least one of read, write or events is required")
	}
	return nil
}

// CreateToken mints a token and stores it, returning the plaintext (this once) and the
// display shape.
func (s *Service) CreateToken(ctx context.Context, input NewTokenInput) (string, *Token, error) {
	if err := validateTokenPermissions(input.CanRead, input.CanWrite, input.CanEvents, input.CanChanges, input.CanSubscribe); err != nil {
		return "", nil, err
	}

	kind, err := normalizeKind(input.Kind)
	if err != nil {
		return "", nil, err
	}
	name, err := normalizeName(kind, input.Name)
	if err != nil {
		return "", nil, err
	}
	if err := validateLifetime(kind, input.Expiry, time.Now()); err != nil {
		return "", nil, err
	}

	prefix, err := paths.Normalize(input.Prefix)
	if err != nil {
		return "", nil, err
	}

	id, err := newTokenID()
	if err != nil {
		return "", nil, err
	}
	secret, err := randomToken()
	if err != nil {
		return "", nil, err
	}

	var expiresAt any
	if input.Expiry != nil {
		expiresAt = *input.Expiry
	}

	var (
		token    Token
		lastUsed *string
		expires  *string
	)
	err = s.db.Pool().QueryRow(ctx,
		`insert into api_tokens (token_id, token_hash, user_id, name, kind, prefix, can_read, can_write, can_events, can_changes, can_subscribe, expires_at)
		 values ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 returning token_id, name, kind, prefix, can_read, can_write, can_events, can_changes, can_subscribe, policy_version,
		           `+isoUTC("created_at")+`,
		           case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		           case when expires_at is null then null else `+isoUTC("expires_at")+` end`,
		id, hashSecret(secret), input.UserID, name, kind, prefix, input.CanRead, input.CanWrite, input.CanEvents, input.CanChanges, input.CanSubscribe, expiresAt).
		Scan(&token.ID, &token.Name, &token.Kind, &token.Prefix, &token.CanRead, &token.CanWrite, &token.CanEvents, &token.CanChanges, &token.CanSubscribe, &token.PolicyVersion,
			&token.CreatedAt, &lastUsed, &expires)
	if err != nil {
		return "", nil, err
	}
	token.userID = input.UserID
	token.LastUsedAt = lastUsed
	token.ExpiresAt = expires

	return TokenPrefix + id + "_" + secret, &token, nil
}

// SetTokenName renames one token the user owns. It is the only field a Bearer token may
// write about itself: an app created without a name reports one after scanning the QR.
func (s *Service) SetTokenName(ctx context.Context, userID, tokenID, raw string) (*Token, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, httpx.BadRequest("the name cannot be empty")
	}
	name, err := normalizeName(KindApp, raw)
	if err != nil {
		return nil, err
	}

	var (
		token    Token
		lastUsed *string
		expires  *string
	)
	err = s.db.Pool().QueryRow(ctx,
		`update api_tokens set name = $3
		  where user_id = $1::uuid and token_id = $2 and revoked_at is null
		 returning token_id, name, kind, prefix, can_read, can_write, can_events, can_changes, can_subscribe, policy_version,
		           `+isoUTC("created_at")+`,
		           case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		           case when expires_at is null then null else `+isoUTC("expires_at")+` end`,
		userID, tokenID, name).
		Scan(&token.ID, &token.Name, &token.Kind, &token.Prefix, &token.CanRead, &token.CanWrite, &token.CanEvents, &token.CanChanges, &token.CanSubscribe, &token.PolicyVersion,
			&token.CreatedAt, &lastUsed, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("token not found")
		}
		return nil, err
	}
	token.userID = userID
	token.LastUsedAt = lastUsed
	token.ExpiresAt = expires
	return &token, nil
}

// UpdateTokenInput is the mutable policy of a connection: everything except its name, kind
// and secret.
type UpdateTokenInput struct {
	Prefix       string
	CanRead      bool
	CanWrite     bool
	CanEvents    bool
	CanChanges   bool
	CanSubscribe bool
	Expiry       *time.Time // nil = never
}

// UpdateToken rewrites the policy of one token the user owns. The credential and the name
// are untouched; the kind is read from the row so the lifetime cap stays correct.
func (s *Service) UpdateToken(ctx context.Context, userID, tokenID string, input UpdateTokenInput) (*Token, error) {
	if err := validateTokenPermissions(input.CanRead, input.CanWrite, input.CanEvents, input.CanChanges, input.CanSubscribe); err != nil {
		return nil, err
	}
	prefix, err := paths.Normalize(input.Prefix)
	if err != nil {
		return nil, err
	}

	var kind string
	err = s.db.Pool().QueryRow(ctx,
		`select kind from api_tokens
		  where user_id = $1::uuid and token_id = $2 and revoked_at is null`, userID, tokenID).Scan(&kind)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("token not found")
		}
		return nil, err
	}
	if err := validateLifetime(kind, input.Expiry, time.Now()); err != nil {
		return nil, err
	}

	var expiresAt any
	if input.Expiry != nil {
		expiresAt = *input.Expiry
	}

	var (
		token    Token
		lastUsed *string
		expires  *string
	)
	err = s.db.Pool().QueryRow(ctx,
		`update api_tokens
		    set policy_version = policy_version + case when
                    row(prefix, can_read, can_write, can_events, can_changes, can_subscribe, expires_at)
                    is distinct from row($3::text, $4::boolean, $5::boolean, $6::boolean, $7::boolean, $8::boolean, $9::timestamptz)
                    then 1 else 0 end,
                prefix = $3, can_read = $4, can_write = $5, can_events = $6,
                can_changes = $7, can_subscribe = $8, expires_at = $9
		  where user_id = $1::uuid and token_id = $2 and revoked_at is null
		 returning token_id, name, kind, prefix, can_read, can_write, can_events, can_changes, can_subscribe, policy_version,
		           `+isoUTC("created_at")+`,
		           case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		           case when expires_at is null then null else `+isoUTC("expires_at")+` end`,
		userID, tokenID, prefix, input.CanRead, input.CanWrite, input.CanEvents, input.CanChanges, input.CanSubscribe, expiresAt).
		Scan(&token.ID, &token.Name, &token.Kind, &token.Prefix, &token.CanRead, &token.CanWrite, &token.CanEvents, &token.CanChanges, &token.CanSubscribe, &token.PolicyVersion,
			&token.CreatedAt, &lastUsed, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, httpx.NotFound("token not found")
		}
		return nil, err
	}
	token.userID = userID
	token.LastUsedAt = lastUsed
	token.ExpiresAt = expires
	return &token, nil
}

// ListTokens returns every token for a user, revoked ones included for auditing.
func (s *Service) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select token_id, name, kind, prefix, can_read, can_write, can_events, can_changes, can_subscribe, policy_version,
		        `+isoUTC("created_at")+`,
		        case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		        case when expires_at is null then null else `+isoUTC("expires_at")+` end,
		        revoked_at is not null,
		        case when revoked_at is null then null else `+isoUTC("revoked_at")+` end
		   from api_tokens
		  where user_id = $1::uuid
		  order by created_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]Token, 0, 8)
	for rows.Next() {
		var (
			token     Token
			lastUsed  *string
			expires   *string
			revoked   bool
			revokedAt *string
		)
		if err := rows.Scan(&token.ID, &token.Name, &token.Kind, &token.Prefix,
			&token.CanRead, &token.CanWrite, &token.CanEvents, &token.CanChanges, &token.CanSubscribe, &token.PolicyVersion, &token.CreatedAt,
			&lastUsed, &expires, &revoked, &revokedAt); err != nil {
			return nil, err
		}
		token.LastUsedAt = lastUsed
		token.ExpiresAt = expires
		token.Revoked = revoked
		token.RevokedAt = revokedAt
		list = append(list, token)
	}
	return list, rows.Err()
}

// CountActiveTokens counts unrevoked, unexpired connections, for the create cap.
func (s *Service) CountActiveTokens(ctx context.Context, userID string) (int, error) {
	var count int
	err := s.db.Pool().QueryRow(ctx,
		`select count(*) from api_tokens
		  where user_id = $1::uuid
		    and revoked_at is null
		    and (expires_at is null or expires_at > now())`, userID).Scan(&count)
	return count, err
}

// RevokeToken revokes a token: it immediately stops granting any permission.
func (s *Service) RevokeToken(ctx context.Context, userID, tokenID string) error {
	tag, err := s.db.Pool().Exec(ctx,
		`update api_tokens set revoked_at = now(), policy_version = policy_version + 1
		  where user_id = $1::uuid and token_id = $2 and revoked_at is null`,
		userID, tokenID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.NotFound("token not found")
	}
	return nil
}

// VerifyToken checks a plaintext token; an invalid one returns (nil, nil).
func (s *Service) VerifyToken(ctx context.Context, plain string) (*Token, error) {
	id, secret, ok := ParseToken(plain)
	if !ok {
		return nil, nil
	}

	var (
		token    Token
		hash     string
		lastUsed *string
		expires  *string
	)
	err := s.db.Pool().QueryRow(ctx,
		`select user_id::text, token_id, name, kind, prefix, can_read, can_write, can_events, can_changes, can_subscribe, policy_version,
		        `+isoUTC("created_at")+`,
		        case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		        case when expires_at is null then null else `+isoUTC("expires_at")+` end,
		        token_hash
		   from api_tokens
		  where token_id = $1
		    and revoked_at is null
		    and (expires_at is null or expires_at > now())
		  limit 1`, id).
		Scan(&token.userID, &token.ID, &token.Name, &token.Kind, &token.Prefix,
			&token.CanRead, &token.CanWrite, &token.CanEvents, &token.CanChanges, &token.CanSubscribe, &token.PolicyVersion, &token.CreatedAt,
			&lastUsed, &expires, &hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(secret))) != 1 {
		return nil, nil
	}
	token.LastUsedAt = lastUsed
	token.ExpiresAt = expires

	// Update at most once a minute so every request does not hit the database.
	_, _ = s.db.Pool().Exec(ctx,
		`update api_tokens set last_used_at = now()
		  where token_id = $1
		    and (last_used_at is null or last_used_at < now() - interval '1 minute')`, id)

	return &token, nil
}

// Authenticate tries Authorization: Bearer first, then falls back to the cookie session.
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (*Principal, error) {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		plain, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			return nil, httpx.Unauthorized("the Authorization header only supports Bearer")
		}
		plain = strings.TrimSpace(plain)
		token, err := s.VerifyToken(ctx, plain)
		if err != nil {
			return nil, err
		}
		if token == nil {
			// Not an eam_-prefixed token, so try it as a save-link short code instead.
			link, err := s.VerifySaveLink(ctx, plain)
			if err != nil {
				return nil, err
			}
			if link == nil {
				return nil, httpx.Unauthorized("token is invalid, expired, or revoked")
			}
			return link, nil
		}
		row, err := s.FindUserByID(ctx, token.userID)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, httpx.Unauthorized("the account behind this token does not exist")
		}
		return &Principal{User: ToUser(row), Token: token}, nil
	}

	user, err := s.SessionUser(ctx, r)
	if err != nil {
		return nil, err
	}
	if user == nil {
		return nil, httpx.Unauthorized("not signed in")
	}
	return &Principal{User: user}, nil
}

// isoUTC builds the same ISO-8601 timestamp string that the nodes package uses.
func isoUTC(column string) string {
	return fmt.Sprintf(`to_char(%s at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`, column)
}
