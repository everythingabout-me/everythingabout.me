package auth

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"everythingabout.me/backend/internal/store"
)

// ErrUsernameTaken is the one business failure of SetUsername: the handle already belongs
// to another account, either as its current address or as an alias its old links still
// resolve through. Handlers map it to 409; the address is not disclosed beyond that.
var ErrUsernameTaken = errors.New("username is already taken")

// SetUsername claims a handle for an account that has none, or renames the account's
// current one. The old handle is kept as a superseded row so links minted earlier keep
// resolving (see sql/058 for why this is one table and one primary key).
//
// The target row is locked before the current handle is superseded: two accounts claiming
// the same name serialize on it, and a rename into a name someone else is mid-claiming
// either sees their committed row and refuses or wins the insert, never both.
func (s *Service) SetUsername(ctx context.Context, userID, handle string) (*UserRow, error) {
	tx, err := s.db.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var owner string
	missing := false
	err = tx.QueryRow(ctx,
		`select user_id::text from usernames where handle = $1 for update`, handle).Scan(&owner)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		missing = true
	case err != nil:
		return nil, err
	case owner != userID:
		return nil, ErrUsernameTaken
	}

	// Free the one-current-per-account slot before taking the target: the partial unique
	// index refuses a second current row, so the old handle has to step aside first. A
	// failed claim below rolls this update back with it.
	if _, err := tx.Exec(ctx,
		`update usernames set current = false, superseded_at = now()
		  where user_id = $1::uuid and current and handle <> $2`, userID, handle); err != nil {
		return nil, err
	}

	switch {
	case missing:
		if _, err := tx.Exec(ctx,
			`insert into usernames (handle, user_id) values ($1, $2::uuid)`, handle, userID); err != nil {
			if store.IsUniqueViolation(err) {
				return nil, ErrUsernameTaken
			}
			return nil, err
		}
	default:
		// Renaming back to a handle this account used to hold: the alias row exists and a
		// second insert would collide with it, so revive it instead.
		if _, err := tx.Exec(ctx,
			`update usernames set current = true, superseded_at = null where handle = $1`, handle); err != nil {
			return nil, err
		}
	}

	row, err := scanUserRow(tx.QueryRow(ctx, userSelectFrom+` where u.id = $1::uuid limit 1`, userID))
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errors.New("account disappeared while setting its username")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// ResolveOwnUsername answers "did this handle ever mean me?" for the sign-in owner only.
// Aliases are not a directory: resolving another account's handle is not a lookup this
// service offers, so an unknown or foreign name both answer "".
func (s *Service) ResolveOwnUsername(ctx context.Context, userID, candidate string) (string, error) {
	var handle string
	err := s.db.Pool().QueryRow(ctx,
		`select cur.handle
		   from usernames mine
		   join usernames cur on cur.user_id = mine.user_id and cur.current
		  where mine.user_id = $1::uuid and mine.handle = $2`, userID, candidate).Scan(&handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return handle, err
}
