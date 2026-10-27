// Package emailcode 管理邮箱验证码：登录、注册、改邮箱共用。
// 只存验证码的 scrypt 哈希，不存明文；10 分钟过期，最多尝试 5 次。
package emailcode

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/store"
)

const (
	TTL         = 10 * time.Minute
	maxAttempts = 5
)

// ErrIncorrect 表示验证码不匹配。调用方靠它换一句带上下文的话，
// 而不是去比字符串。
var ErrIncorrect = httpx.BadRequest("incorrect verification code")

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service {
	return &Service{db: db}
}

// generate 用密码学安全的随机数，补零保证永远是 6 位。
func generate() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Issue 发一个新验证码；同一邮箱同一用途的旧码立即作废。
func (s *Service) Issue(ctx context.Context, email, purpose, ip string) (string, error) {
	code, err := generate()
	if err != nil {
		return "", err
	}
	codeHash, err := auth.HashPassword(code)
	if err != nil {
		return "", err
	}
	expiresAt := time.Now().Add(TTL)

	var ipArg any
	if ip != "" {
		ipArg = ip
	}

	pool := s.db.Pool()
	if _, err := pool.Exec(ctx,
		`update email_codes set consumed_at = now()
		  where email = $1 and purpose = $2 and consumed_at is null`,
		email, purpose); err != nil {
		return "", err
	}
	if _, err := pool.Exec(ctx,
		`insert into email_codes (email, purpose, code_hash, expires_at, ip)
		 values ($1, $2, $3, $4, $5)`,
		email, purpose, codeHash, expiresAt, ipArg); err != nil {
		return "", err
	}
	// 顺手清理一天前的过期记录
	_, _ = pool.Exec(ctx, `delete from email_codes where expires_at < now() - interval '1 day'`)
	return code, nil
}

// Revoke 在邮件发送失败时把刚生成的码作废，
// 避免留下一个用户永远收不到的可用码。
func (s *Service) Revoke(ctx context.Context, email, purpose string) error {
	_, err := s.db.Pool().Exec(ctx,
		`update email_codes set consumed_at = now()
		  where email = $1 and purpose = $2 and consumed_at is null`, email, purpose)
	return err
}

// VerifyLatest 校验验证码但不消费，返回记录 id，
// 供调用方在“两个码都验证通过”后再一起消费。
func (s *Service) VerifyLatest(ctx context.Context, email string, purposes []string, code string) (string, error) {
	var (
		id        string
		codeHash  string
		attempts  int
		expiresAt time.Time
	)
	err := s.db.Pool().QueryRow(ctx,
		`select id::text, code_hash, attempts, expires_at
		   from email_codes
		  where email = $1 and purpose = any($2::text[]) and consumed_at is null
		  order by created_at desc
		  limit 1`, email, purposes,
	).Scan(&id, &codeHash, &attempts, &expiresAt)
	if err == pgx.ErrNoRows {
		return "", httpx.BadRequest("verification code is no longer valid; request a new one")
	}
	if err != nil {
		return "", err
	}

	if !expiresAt.After(time.Now()) {
		_, _ = s.db.Pool().Exec(ctx, `update email_codes set consumed_at = now() where id = $1::uuid`, id)
		return "", httpx.BadRequest("verification code expired; request a new one")
	}
	if attempts >= maxAttempts {
		_, _ = s.db.Pool().Exec(ctx, `update email_codes set consumed_at = now() where id = $1::uuid`, id)
		return "", httpx.TooManyRequests("too many verification attempts; request a new code")
	}
	if !auth.VerifyPassword(code, codeHash) {
		_, _ = s.db.Pool().Exec(ctx,
			`update email_codes set attempts = attempts + 1 where id = $1::uuid`, id)
		return "", ErrIncorrect
	}
	return id, nil
}

// ConsumeByID 把已经验证通过的验证码标记为已使用。
func (s *Service) ConsumeByID(ctx context.Context, id string) error {
	tag, err := s.db.Pool().Exec(ctx,
		`update email_codes set consumed_at = now()
		  where id = $1::uuid and consumed_at is null`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return httpx.BadRequest("verification code is no longer valid; request a new one")
	}
	return nil
}

// Consume 校验并消费验证码。
func (s *Service) Consume(ctx context.Context, email string, purposes []string, code string) error {
	id, err := s.VerifyLatest(ctx, email, purposes, code)
	if err != nil {
		return err
	}
	return s.ConsumeByID(ctx, id)
}
