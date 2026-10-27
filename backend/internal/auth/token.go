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

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
)

// TokenPrefix 是给外部程序用的访问令牌前缀。
// 形如 eam_<12 位 id>_<43 位 secret>；库里只存 secret 的 SHA-256。
const TokenPrefix = "eam_"

const tokenIDLength = 12

// Token 是对外暴露的令牌形状（永远不含 secret）。
type Token struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CanRead    bool    `json:"read"`
	CanWrite   bool    `json:"write"`
	CanEvents  bool    `json:"events"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	ExpiresAt  *string `json:"expiresAt"`
	Revoked    bool    `json:"revoked"`

	userID string
}

// Principal 是一次请求的身份：cookie 会话（Token 为 nil，全权限）
// 或者一个受限的 API token。
type Principal struct {
	User  *User
	Token *Token
}

// FullAccess 表示这是用户本人的会话，不受前缀限制。
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

// CanWrite 判断这个身份能不能写某个目录——列目录时用来告诉客户端
// "这个位置你可以落笔"，省得它靠撞 403 试错。
func (p *Principal) CanWrite(path string) bool {
	if p.FullAccess() {
		return true
	}
	if !p.Token.CanWrite {
		return false
	}
	return paths.IsInside(path, p.Token.Prefix)
}

// RequireEvents 判断这个身份能不能读写事件。
// 事件和文件是两个能力位：应用可能只投稿，不碰文件。
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

// ParseToken 拆出 token_id 和 secret；格式不对返回 ok=false。
func ParseToken(plain string) (id, secret string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(plain), TokenPrefix)
	if !found {
		return "", "", false
	}
	// secret 用 base64url，可能含 "_"，所以只切第一个下划线。
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

// NewTokenInput 是创建令牌的入参。
type NewTokenInput struct {
	UserID    string
	Name      string
	Prefix    string
	CanRead   bool
	CanWrite  bool
	CanEvents bool
	Expiry    *time.Time
}

// CreateToken 生成令牌并落库，返回明文（只此一次）和展示用的形状。
func (s *Service) CreateToken(ctx context.Context, input NewTokenInput) (string, *Token, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return "", nil, httpx.BadRequest("token name cannot be empty")
	}
	if len(name) > 80 {
		return "", nil, httpx.BadRequest("token name is too long")
	}
	if !input.CanRead && !input.CanWrite && !input.CanEvents {
		return "", nil, httpx.BadRequest("at least one of read, write or events is required")
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
		`insert into api_tokens (token_id, token_hash, user_id, name, prefix, can_read, can_write, can_events, expires_at)
		 values ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9)
		 returning token_id, name, prefix, can_read, can_write, can_events,
		           `+isoUTC("created_at")+`,
		           case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		           case when expires_at is null then null else `+isoUTC("expires_at")+` end`,
		id, hashSecret(secret), input.UserID, name, prefix, input.CanRead, input.CanWrite, input.CanEvents, expiresAt).
		Scan(&token.ID, &token.Name, &token.Prefix, &token.CanRead, &token.CanWrite, &token.CanEvents,
			&token.CreatedAt, &lastUsed, &expires)
	if err != nil {
		return "", nil, err
	}
	token.userID = input.UserID
	token.LastUsedAt = lastUsed
	token.ExpiresAt = expires

	return TokenPrefix + id + "_" + secret, &token, nil
}

// ListTokens 返回某个用户的所有令牌（含已撤销的，便于审计）。
func (s *Service) ListTokens(ctx context.Context, userID string) ([]Token, error) {
	rows, err := s.db.Pool().Query(ctx,
		`select token_id, name, prefix, can_read, can_write, can_events,
		        `+isoUTC("created_at")+`,
		        case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		        case when expires_at is null then null else `+isoUTC("expires_at")+` end,
		        revoked_at is not null
		   from api_tokens
		  where user_id = $1::uuid
		  order by created_at desc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []Token
	for rows.Next() {
		var (
			token    Token
			lastUsed *string
			expires  *string
			revoked  bool
		)
		if err := rows.Scan(&token.ID, &token.Name, &token.Prefix,
			&token.CanRead, &token.CanWrite, &token.CanEvents, &token.CreatedAt,
			&lastUsed, &expires, &revoked); err != nil {
			return nil, err
		}
		token.LastUsedAt = lastUsed
		token.ExpiresAt = expires
		token.Revoked = revoked
		list = append(list, token)
	}
	return list, rows.Err()
}

// RevokeToken 撤销一个令牌：立刻不能再换到任何权限。
func (s *Service) RevokeToken(ctx context.Context, userID, tokenID string) error {
	tag, err := s.db.Pool().Exec(ctx,
		`update api_tokens set revoked_at = now()
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

// VerifyToken 校验明文令牌；无效返回 (nil, nil)。
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
		`select user_id::text, token_id, name, prefix, can_read, can_write, can_events,
		        `+isoUTC("created_at")+`,
		        case when last_used_at is null then null else `+isoUTC("last_used_at")+` end,
		        case when expires_at is null then null else `+isoUTC("expires_at")+` end,
		        token_hash
		   from api_tokens
		  where token_id = $1
		    and revoked_at is null
		    and (expires_at is null or expires_at > now())
		  limit 1`, id).
		Scan(&token.userID, &token.ID, &token.Name, &token.Prefix,
			&token.CanRead, &token.CanWrite, &token.CanEvents, &token.CreatedAt,
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

	// 每分钟最多更新一次，避免每个请求都写库。
	_, _ = s.db.Pool().Exec(ctx,
		`update api_tokens set last_used_at = now()
		  where token_id = $1
		    and (last_used_at is null or last_used_at < now() - interval '1 minute')`, id)

	return &token, nil
}

// Authenticate 先看 Authorization: Bearer，再退回 cookie 会话。
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (*Principal, error) {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		plain, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			return nil, httpx.Unauthorized("the Authorization header only supports Bearer")
		}
		token, err := s.VerifyToken(ctx, strings.TrimSpace(plain))
		if err != nil {
			return nil, err
		}
		if token == nil {
			return nil, httpx.Unauthorized("token is invalid, expired, or revoked")
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

// isoUTC 生成和 nodes 包一致的 ISO-8601 时间字符串。
func isoUTC(column string) string {
	return fmt.Sprintf(`to_char(%s at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`, column)
}
