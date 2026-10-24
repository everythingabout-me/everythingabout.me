package auth

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/store"
)

// UserRow 是 users 表的一行；password_hash 可为空（密码是可选的）。
type UserRow struct {
	ID             string
	Email          string
	PasswordHash   *string
	CreatedAt      string
	UpdatedAt      string
	EmailChangedAt *string
	LastLoginAt    *string
}

// User 是暴露给前端的形状，永远不含 password_hash。
type User struct {
	ID             string  `json:"id"`
	Email          string  `json:"email"`
	HasPassword    bool    `json:"hasPassword"`
	CreatedAt      string  `json:"createdAt"`
	EmailChangedAt *string `json:"emailChangedAt"`
}

func ToUser(row *UserRow) *User {
	return &User{
		ID:             row.ID,
		Email:          row.Email,
		HasPassword:    row.PasswordHash != nil,
		CreatedAt:      row.CreatedAt,
		EmailChangedAt: row.EmailChangedAt,
	}
}

// userColumns 返回 select 列表。alias 用于 join 查询（例如 "u"），
// 避免和 sessions.created_at 这类同名列冲突。
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

func scanUserRow(row pgx.Row) (*UserRow, error) {
	var user UserRow
	err := row.Scan(
		&user.ID, &user.Email, &user.PasswordHash,
		&user.CreatedAt, &user.UpdatedAt, &user.EmailChangedAt, &user.LastLoginAt,
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
		`select count(*) from nodes where owner_id is null and deleted_at is null`).Scan(&count)
	return count, err
}

// FindUserByEmail 邮箱大小写不敏感。
func (s *Service) FindUserByEmail(ctx context.Context, email string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		`select `+userColumns("")+` from users where lower(email) = lower($1) limit 1`, email))
}

func (s *Service) FindUserByID(ctx context.Context, id string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		`select `+userColumns("")+` from users where id = $1::uuid limit 1`, id))
}

func (s *Service) CreateUser(ctx context.Context, email string, passwordHash *string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		`insert into users (email, password_hash) values ($1, $2)
		 returning `+userColumns(""), email, passwordHash))
}

func (s *Service) SetUserPassword(ctx context.Context, id string, passwordHash *string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		`update users set password_hash = $1, updated_at = now()
		  where id = $2::uuid
		  returning `+userColumns(""), passwordHash, id))
}

func (s *Service) UpdateUserEmail(ctx context.Context, id, email string) (*UserRow, error) {
	return scanUserRow(s.db.Pool().QueryRow(ctx,
		`update users set email = $1, updated_at = now(), email_changed_at = now()
		  where id = $2::uuid
		  returning `+userColumns(""), email, id))
}

func (s *Service) TouchLastLogin(ctx context.Context, id string) error {
	_, err := s.db.Pool().Exec(ctx, `update users set last_login_at = now() where id = $1::uuid`, id)
	return err
}

// IsUniqueViolation 暴露给账号相关 handler 使用。
var IsUniqueViolation = store.IsUniqueViolation
