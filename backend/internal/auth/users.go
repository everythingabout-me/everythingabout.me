package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/store"
)

// UserRow is one row of the users table; password_hash may be null (the password is optional).
// Username is the account's current handle, joined from usernames (sql/058) — the address
// a person types, never an identity or a credential, so it is nullable and mutable.
type UserRow struct {
	ID             string
	Email          string
	PasswordHash   *string
	CreatedAt      string
	UpdatedAt      string
	EmailChangedAt *string
	LastLoginAt    *string
	Username       *string
}

// User is the shape exposed to the frontend; it never carries password_hash.
type User struct {
	ID             string  `json:"id"`
	Email          string  `json:"email"`
	Username       *string `json:"username"`
	HasPassword    bool    `json:"hasPassword"`
	CreatedAt      string  `json:"createdAt"`
	EmailChangedAt *string `json:"emailChangedAt"`
}

func ToUser(row *UserRow) *User {
	return &User{
		ID:             row.ID,
		Email:          row.Email,
		Username:       row.Username,
		HasPassword:    row.PasswordHash != nil,
		CreatedAt:      row.CreatedAt,
		EmailChangedAt: row.EmailChangedAt,
	}
}

// userColumns returns the select list. alias is for join queries (for example "u"), so
// that same-named columns like sessions.created_at do not collide.
func userColumns(alias string) string {
	column := func(name string) string {
		if alias == "" {
			return name
		}
		return alias + "." + name
	}
	iso := func(name string) string {
		return fmt.Sprintf(
			`to_char(%s at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`,
			column(name),
		)
	}
	return fmt.Sprintf(
		`%s::text, %s, %s, %s, %s,
		 case when %s is null then null else %s end,
		 case when %s is null then null else %s end`,
		column("id"), column("email"), column("password_hash"),
		iso("created_at"), iso("updated_at"),
		column("email_changed_at"), iso("email_changed_at"),
		column("last_login_at"), iso("last_login_at"),
	)
}

// userSelectFrom is the one read shape of an account: every users column plus the current
// handle, which lives in its own table because a handle is unique across current AND
// superseded names (a freed old name must stay reserved while its alias still redirects).
// Every account read appends its own where/join clause; there is no second spelling of
// "select an account" that could forget the handle.
var userSelectFrom = "select " + userColumns("u") + `, n.handle
	from users u
	left join usernames n on n.user_id = u.id and n.current`

func scanUserRow(row pgx.Row) (*UserRow, error) {
	var user UserRow
	err := row.Scan(
		&user.ID, &user.Email, &user.PasswordHash,
		&user.CreatedAt, &user.UpdatedAt, &user.EmailChangedAt, &user.LastLoginAt,
		&user.Username,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &user, nil
}

func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := s.db.Pool().QueryRow(ctx, `select count(*) from users`).Scan(&count)
	return count, err
}

func (s *Service) LegacyNodeCount(ctx context.Context) (int, error) {
	var count int
	err := s.db.Pool().QueryRow(ctx,
		`select count(*) from nodes where owner_id is null`).Scan(&count)
	return count, err
}

// FindUserByEmail matches email case-insensitively.
func (s *Service) FindUserByEmail(ctx context.Context, email string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		userSelectFrom+` where lower(u.email) = lower($1) limit 1`, email))
}

func (s *Service) FindUserByID(ctx context.Context, id string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		userSelectFrom+` where u.id = $1::uuid limit 1`, id))
}

// CreateUser inserts and reads the account back in one transaction: RETURNING cannot
// carry the handle join, and reading through userSelectFrom keeps one select list.
func (s *Service) CreateUser(ctx context.Context, email string, passwordHash *string) (*UserRow, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id string
	if err := tx.QueryRow(ctx,
		`insert into users (email, password_hash) values ($1, $2) returning id::text`,
		email, passwordHash).Scan(&id); err != nil {
		return nil, err
	}
	row, err := scanUserRow(tx.QueryRow(ctx, userSelectFrom+` where u.id = $1::uuid limit 1`, id))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errors.New("the account inserted a moment ago cannot be read back")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// SetUserPassword sets or clears the password and revokes the account's other sessions
// in the same transaction, so a failed revocation rolls the credential change back
// instead of leaving a new password guarded by cookies the owner meant to kill.
// currentToken names the session that stays; see revokeOtherSessions.
func (s *Service) SetUserPassword(ctx context.Context, id string, passwordHash *string, currentToken string) (*UserRow, int64, error) {
	return s.updateUserAndRevoke(ctx, id, currentToken,
		`update users set password_hash = $1, updated_at = now()
		  where id = $2::uuid
		  returning id::text`, passwordHash, id)
}

// UpdateUserEmail changes the address and revokes the account's other sessions in the
// same transaction. Changing the address already needs codes for both the old and the
// new mailbox; abandoning every other session is the companion step, because an
// attacker holding a session cookie was exactly what the codes cannot see.
func (s *Service) UpdateUserEmail(ctx context.Context, id, email, currentToken string) (*UserRow, int64, error) {
	return s.updateUserAndRevoke(ctx, id, currentToken,
		`update users set email = $1, updated_at = now(), email_changed_at = now()
		  where id = $2::uuid
		  returning id::text`, email, id)
}

// updateUserAndRevoke runs one users update and the session revocation together: the
// agent who steals a cookie is not stopped by a credential change they never had to
// present, so the two effects must not be separable by a crash between them.
func (s *Service) updateUserAndRevoke(ctx context.Context, id, currentToken, statement string, args ...any) (*UserRow, int64, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var updated string
	if err := tx.QueryRow(ctx, statement, args...).Scan(&updated); err != nil {
		return nil, 0, err
	}
	row, err := scanUserRow(tx.QueryRow(ctx, userSelectFrom+` where u.id = $1::uuid limit 1`, updated))
	if err != nil {
		return nil, 0, err
	}
	revoked, err := revokeOtherSessions(ctx, tx, id, currentToken)
	if err != nil {
		return nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, err
	}
	return row, revoked, nil
}

func (s *Service) TouchLastLogin(ctx context.Context, id string) error {
	_, err := s.db.Pool().Exec(ctx, `update users set last_login_at = now() where id = $1::uuid`, id)
	return err
}

// IsUniqueViolation is exposed for the account handlers.
var IsUniqueViolation = store.IsUniqueViolation
