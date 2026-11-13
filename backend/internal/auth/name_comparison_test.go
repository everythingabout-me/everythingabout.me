package auth

import (
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

// AuthorizeFileOperation rechecks a queued effect's paths against the token
// scope with the canonical comparison key, so a token created for one spelling
// also covers an index-equal sibling.
func TestTokenScopeUsesReferenceKey(t *testing.T) {
	ctx, _, service, owner := tokenPostgresService(t)
	root := fileops.Root{OwnerID: owner, CaseSensitive: false}
	nameprobe.CheckPair(t, "auth.AuthorizeFileOperation", nameprobe.ReferencePipeline, false, func(a, b string) bool {
		_, token, err := service.CreateToken(ctx, NewTokenInput{UserID: owner, Name: "probe", Prefix: a, CanRead: true, CanWrite: true})
		if err != nil {
			t.Fatal(err)
		}
		op := fileops.Operation{
			Root:        root,
			PrincipalID: "token:" + token.ID,
			Kind:        "upload_commit",
			Guards:      []fileops.Guard{{Path: storage.Path(b)}},
		}
		return service.AuthorizeFileOperation(ctx, op) == nil
	})
}
