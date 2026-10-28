// Package emailcode manages email verification codes, shared by login, registration, and
// email changes. Only the scrypt hash of a code is stored, never the plaintext; codes expire
// after 10 minutes and allow at most 5 attempts.
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

// ErrIncorrect means the verification code did not match. Callers use it to swap in a
// message with context rather than comparing strings.
var ErrIncorrect = httpx.BadRequest("incorrect verification code")

type Service struct {
	db *store.DB
}

func New(db *store.DB) *Service {
	return &Service{db: db}
}

// generate uses a cryptographically secure random number, zero-padded to always be 6 digits.
func generate() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// Issue mints a new code; older codes for the same email and purpose are voided immediately.
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
	// Sweep records that expired more than a day ago while we are here
	_, _ = pool.Exec(ctx, `delete from email_codes where expires_at < now() - interval '1 day'`)
	return code, nil
}

// Revoke voids the code that was just minted when the mail fails to send, so no usable code
// is left behind that the user can never receive.
func (s *Service) Revoke(ctx context.Context, email, purpose string) error {
	_, err := s.db.Pool().Exec(ctx,
		`update email_codes set consumed_at = now()
		  where email = $1 and purpose = $2 and consumed_at is null`, email, purpose)
	return err
}

// VerifyLatest checks a code without consuming it and returns the record id, so the caller
// can consume both only after both codes have passed.
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

// ConsumeByID marks an already-verified code as used.
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

// Consume verifies a code and consumes it.
func (s *Service) Consume(ctx context.Context, email string, purposes []string, code string) error {
	id, err := s.VerifyLatest(ctx, email, purposes, code)
	if err != nil {
		return err
	}
	return s.ConsumeByID(ctx, id)
}
