package auth

import (
	"context"
	"errors"
	"strings"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// AuthorizeFileOperation rechecks durable creator privileges before a queued
// native effect. It never stores bearer secrets or attempts to impersonate HTTP
// sessions. First-party owner work is account-owned after authenticated admission.
func (s *Service) AuthorizeFileOperation(ctx context.Context, op fileops.Operation) error {
	var exists bool
	if err := s.db.Pool().QueryRow(ctx, `select exists(select 1 from users where id=$1::uuid)`, op.Root.OwnerID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fileops.E("forbidden", nil)
	}
	if op.PrincipalID == "owner:"+op.Root.OwnerID {
		return nil
	}
	id, ok := strings.CutPrefix(op.PrincipalID, "token:")
	if !ok || id == "" {
		return fileops.E("forbidden", nil)
	}
	var scope string
	var read, write bool
	if link, ok := strings.CutPrefix(id, SaveLinkTokenPrefix); ok {
		if err := s.db.Pool().QueryRow(ctx, `select exists(select 1 from save_links where user_id=$1::uuid and link_id=$2 and revoked_at is null)`, op.Root.OwnerID, link).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fileops.E("forbidden", nil)
		}
		scope = ""
		read = true
		write = true
	} else {
		err := s.db.Pool().QueryRow(ctx, `select prefix,can_read,can_write from api_tokens where user_id=$1::uuid and token_id=$2 and revoked_at is null and (expires_at is null or expires_at>now())`, op.Root.OwnerID, id).Scan(&scope, &read, &write)
		if errors.Is(err, pgx.ErrNoRows) {
			return fileops.E("forbidden", nil)
		}
		if err != nil {
			return err
		}
	}
	if !write || ((op.Kind == "copy" || op.Kind == "move") && !read) || storage.ValidatePath(storage.Path(scope)) != nil {
		return fileops.E("forbidden", nil)
	}
	key := func(value string) string { return norm.NFC.String(value) }
	if !op.Root.CaseSensitive {
		key = func(value string) string { return cases.Fold().String(norm.NFC.String(value)) }
	}
	scoped := key(scope)
	for _, guard := range op.Guards {
		p := key(string(guard.Path))
		if scoped != "" && p != scoped && !strings.HasPrefix(p, scoped+"/") {
			return fileops.E("forbidden", nil)
		}
	}
	return nil
}
