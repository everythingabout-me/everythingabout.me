package api

import (
	"context"
	"encoding/json"
	"strings"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/storage"
	"net/http"
	"net/url"
	"path"

	fc "everythingabout.me/backend/internal/filecontract"
)

type FileIdentityIndex interface {
	FileResolveID(context.Context, string, string) (string, bool, error)
	FileIdentity(context.Context, string, string) (string, string, error)
}

func filesSelectorError() error {
	return fc.NewProblem(400, "invalid_selector", "Provide exactly one nonempty ID or path, or one valid destination.")
}
func (s *Server) filesResolveSelector(ctx context.Context, f *fileRequest, selector fc.EntrySelector) (fc.Path, error) {
	if err := selector.Validate(); err != nil {
		return "", err
	}
	p := selector.Path
	if selector.IsID() {
		index, ok := s.filesIndex.(FileIdentityIndex)
		if !ok {
			return "", fc.NewProblem(503, "temporarily_unavailable", "File identity lookup is unavailable.")
		}
		relative, found, err := index.FileResolveID(ctx, f.Principal.User.ID, selector.ID)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fc.NewProblem(404, "not_found", "The file was not found.")
		}
		p, err = fc.FromRelative(relative)
		if err != nil {
			return "", err
		}
	}
	if !fc.WithinScope(f.Scope, p, filesPathKey(f)) {
		return "", fc.NewProblem(404, "not_found", "The file was not found.")
	}
	for _, hidden := range filesHiddenRoots(f) {
		hp, err := fc.FromRelative(hidden)
		if err != nil {
			return "", err
		}
		if fc.WithinScope(hp, p, filesPathKey(f)) {
			return "", fc.NewProblem(404, "not_found", "The file was not found.")
		}
	}
	return p, nil
}
func (s *Server) filesResolveQuery(ctx context.Context, f *fileRequest, raw string, allowDefault bool) (fc.Path, error) {
	selector, err := fc.ParseSelectorQuery(raw, allowDefault)
	if err != nil {
		return "", err
	}
	if selector.ID == "" && selector.Path == "" {
		return f.Scope, nil
	}
	return s.filesResolveSelector(ctx, f, selector)
}
func (s *Server) filesResolveDestination(ctx context.Context, f *fileRequest, d fc.Destination) (fc.Path, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	if d.Path != "" {
		return s.filesResolveSelector(ctx, f, fc.EntrySelector{Path: d.Path})
	}
	parent, err := s.filesResolveSelector(ctx, f, *d.Parent)
	if err != nil {
		return "", err
	}
	// Folder identity resolution uses the index, without requiring read authority
	// from a credential that is allowed to create but not browse files.
	if s.filesIndex == nil {
		return "", fc.NewProblem(503, "temporarily_unavailable", "The file index is unavailable.")
	}
	record, err := s.filesIndex.FileBrowseMetadata(ctx, f.Principal.User.ID, f.ContentNamespace, parent.Relative(), filesHiddenRoots(f)...)
	if err != nil {
		return "", err
	}
	if record == nil {
		return "", fc.NewProblem(409, "parent_missing", "The destination parent does not exist.")
	}
	if !record.Row.IsDir {
		return "", fc.NewProblem(409, "not_a_folder", "The destination parent is not a folder.")
	}
	name := string(parent)
	if parent != "/" {
		name += "/"
	}
	return fc.ParsePath(name + d.Name)
}
func (s *Server) filesResolvePutQuery(ctx context.Context, f *fileRequest, r *http.Request) (fc.Path, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", fc.NewProblem(400, "invalid_request", "The query encoding is invalid.")
	}
	for _, v := range q {
		if len(v) != 1 {
			return "", filesSelectorError()
		}
	}
	_, pi := q["parentId"]
	_, pp := q["parentPath"]
	_, name := q["name"]
	if pi || pp || name {
		if pi == pp || !name || q.Has("id") || q.Has("path") || q.Get("name") == "" || r.Header.Get("If-Match") != "" {
			return "", filesSelectorError()
		}
		parent := fc.EntrySelector{ID: q.Get("parentId"), Path: fc.Path(q.Get("parentPath"))}
		return s.filesResolveDestination(ctx, f, fc.Destination{Parent: &parent, Name: q.Get("name")})
	}
	selector, err := fc.ParseSelectorQuery(r.URL.RawQuery, false)
	if err != nil {
		return "", err
	}
	if selector.IsID() && r.Header.Get("If-Match") == "" {
		return "", fc.NewProblem(428, "precondition_required", "Replacing a file by ID requires If-Match.")
	}
	return s.filesResolveSelector(ctx, f, selector)
}

// An optional identity index keeps existing injected read-only fixtures useful;
// production always wires nodes.Service with persistent owner-scoped identity.
func (s *Server) filesSelectionBinding(ctx context.Context, f *fileRequest, p fc.Path, mode string) (string, string, error) {
	index, ok := s.filesIndex.(FileIdentityIndex)
	if !ok {
		return "", "", nil
	}
	id, parent, err := index.FileIdentity(ctx, f.Principal.User.ID, p.Relative())
	if err != nil {
		return "", "", err
	}
	if id == "" && p != "/" {
		parentPath, err := fc.ParsePath(path.Dir(string(p)))
		if err != nil {
			return "", "", err
		}
		parent, _, err = index.FileIdentity(ctx, f.Principal.User.ID, parentPath.Relative())
		if err != nil {
			return "", "", err
		}
	}
	return id, parent, nil
}

func (s *Server) filesReadSelectorQuery(r *http.Request, f *fileRequest, required bool) (filesReadQuery, error) {
	p, err := s.filesResolveQuery(r.Context(), f, r.URL.RawQuery, !required)
	if err != nil {
		return filesReadQuery{}, err
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return filesReadQuery{}, err
	}
	id := q.Get("id")
	q.Del("id")
	q.Set("path", string(p))
	clone := r.Clone(r.Context())
	u := *r.URL
	u.RawQuery = q.Encode()
	clone.URL = &u
	result, err := parseFilesReadQuery(clone, f.Scope, required)
	result.SelectorID = id
	if err == nil {
		if index, ok := s.filesIndex.(FileIdentityIndex); ok {
			result.ResolvedID, _, err = index.FileIdentity(r.Context(), f.Principal.User.ID, p.Relative())
			if err == nil && id != "" && result.ResolvedID != id {
				return filesReadQuery{}, fc.NewProblem(404, "not_found", "The selected file changed location.")
			}
		}
	}
	return result, err
}
func (s *Server) filesCheckIdentityBindings(ctx context.Context, op fileops.Operation) error {
	if op.Terminal() {
		return nil
	}
	index, ok := s.filesIndex.(FileIdentityIndex)
	if !ok {
		return nil
	}
	var entryID, parentID string
	var source, parent string
	if op.Kind == "upload_commit" {
		var intent storage.WriteIntent
		if json.Unmarshal(op.Intent, &intent) != nil {
			return fileops.E("invalid_intent", nil)
		}
		entryID, parentID = intent.EntryID, intent.ParentID
		source = string(intent.Target)
		parent = string(intent.ParentPath)
	} else {
		var req fileops.ActionRequest
		if json.Unmarshal(op.Intent, &req) != nil {
			return fileops.E("invalid_intent", nil)
		}
		entryID, parentID = req.EntryID, req.ParentID
		source = string(req.From)
		target := string(req.To)
		if target == "" {
			target = source
		}
		parent = path.Dir(target)
		if parent == "." {
			parent = ""
		}
	}
	for _, b := range []struct{ id, p string }{{entryID, source}, {parentID, parent}} {
		if b.id == "" {
			continue
		}
		resolved, found, err := index.FileResolveID(ctx, op.Root.OwnerID, b.id)
		if err != nil {
			return err
		}
		if !found || resolved != b.p {
			return fileops.E("target_changed", nil)
		}
	}
	return nil
}

func (s *Server) filesCheckReadIdentity(ctx context.Context, f *fileRequest, p fc.Path, id string) error {
	if id == "" {
		return nil
	}
	index, ok := s.filesIndex.(FileIdentityIndex)
	if !ok {
		return fc.NewProblem(503, "temporarily_unavailable", "File identity lookup is unavailable.")
	}
	actual, _, err := index.FileIdentity(ctx, f.Principal.User.ID, p.Relative())
	if err != nil {
		return err
	}
	if actual != id {
		return fc.NewProblem(404, "not_found", "The selected file changed location.")
	}
	if guards, ok := s.filesIndex.(interface {
		FileIdentityBusy(context.Context, string, string) (string, error)
	}); ok {
		operation, err := guards.FileIdentityBusy(ctx, f.Principal.User.ID, p.Relative())
		if err != nil {
			return err
		}
		if operation != "" {
			e := fc.NewProblem(409, "operation_in_progress", "The selected file is being changed.")
			e.OperationID = operation
			return e
		}
	}
	return nil
}

func filesSelectionMode(raw string) string {
	q, _ := url.ParseQuery(raw)
	for _, key := range []string{"id", "parentId", "parentPath"} {
		if q.Has(key) {
			return key
		}
	}
	return "path"
}

func filesActionSelectorHasID(mode string) bool { return strings.HasPrefix(mode, "id") }

func filesEqualPath(f *fileRequest, a, b fc.Path) bool {
	return fc.WithinScope(a, b, filesPathKey(f)) && fc.WithinScope(b, a, filesPathKey(f))
}

func filesRedactParent(f *fileRequest, m fc.Metadata) fc.Metadata {
	if m.Path == "/" || !fc.WithinScope(f.Scope, m.ParentPath, filesPathKey(f)) {
		m.ParentID = nil
		m.ParentPath = ""
	}
	if m.Kind == "folder" {
		for _, hidden := range filesHiddenRoots(f) {
			hp, err := fc.FromRelative(hidden)
			if err == nil && fc.WithinScope(m.Path, hp, filesPathKey(f)) {
				m.LastChangedAt = nil
				m.ChangeToken = ""
				break
			}
		}
	}
	return m
}
