package api

import (
	"net/http"
	"strings"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// fileRequest carries one verified principal and owner-bound native driver. The
// boundary closes Driver after the handler; content handlers must finish streams.
type fileRequest struct {
	Principal        *auth.Principal
	Source           fc.CredentialSource
	Driver           storage.Driver
	Scope            fc.Path
	Namespace        string // principal/root/config binding for opaque query cursors/revisions
	ContentNamespace string // stable owner/root/config identity, excludes credential principal
	CSRFToken        string // cookie bootstrap only; never a provider credential
}
type fileHandler func(http.ResponseWriter, *http.Request, *fileRequest) error

func (f *fileRequest) RequireRead(path fc.Path) error {
	if err := fc.ValidatePath(string(path)); err != nil {
		return err
	}
	key := func(s string) string { return norm.NFC.String(s) }
	if !f.Driver.Describe().Names.CaseSensitive {
		key = func(s string) string { return cases.Fold().String(norm.NFC.String(s)) }
	}
	if !fc.WithinScope(f.Scope, path, key) {
		return fc.NewProblem(404, "not_found", "The path was not found.")
	}
	if f.Principal.Token != nil && !f.Principal.Token.CanRead {
		return fc.NewProblem(403, "forbidden", "This credential does not permit file reads.")
	}
	return nil
}
func filePrincipalID(p *auth.Principal) string {
	if p.Token != nil {
		return "token:" + p.Token.ID
	}
	return "owner:" + p.User.ID
}
func isFileAPIPath(path string) bool { return path == fc.Base || strings.HasPrefix(path, fc.Base+"/") }
