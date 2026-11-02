package api

import (
	"context"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

func (s *Server) filesObservedContentRevision(ctx context.Context, f *fileRequest, entry storage.Entry) (*string, error) {
	if index, ok := s.filesIndex.(interface {
		LookupContentRevision(context.Context, string, string, storage.Entry) (*string, error)
	}); ok {
		return index.LookupContentRevision(ctx, f.Principal.User.ID, f.ContentNamespace, entry)
	}
	return filesContentRevision(f.ContentNamespace, entry), nil
}
func (f *fileRequest) RequireWrite(path fc.Path) error {
	if err := fc.ValidatePath(string(path)); err != nil {
		return err
	}
	if !fc.WithinScope(f.Scope, path, filesPathKey(f)) {
		return fc.NewProblem(404, "not_found", "The path was not found.")
	}
	if f.Principal.Token != nil && !f.Principal.Token.CanWrite {
		return fc.NewProblem(403, "forbidden", "This credential does not permit file writes.")
	}
	return nil
}
