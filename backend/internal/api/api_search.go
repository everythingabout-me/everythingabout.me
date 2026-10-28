package api

import (
	"strings"

	"net/http"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
)

// scopePrefix narrows the requested path to what the token allows:
// no path means the token's own prefix; a path outside it is 403.
func scopePrefix(p *auth.Principal, requested string) (string, error) {
	if p.Token != nil && p.Token.Prefix != "" {
		if requested == "" {
			return p.Token.Prefix, nil
		}
		if !paths.IsInside(requested, p.Token.Prefix) {
			return "", httpx.Forbidden("this token can only access " + p.Token.Prefix)
		}
	}
	return requested, nil
}

// handleAPISearch searches file names and full paths, with path / mime / type filters.
func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		return httpx.BadRequest("q cannot be empty")
	}
	prefix, err := paths.Normalize(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	scoped, err := scopePrefix(p, prefix)
	if err != nil {
		return err
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		return err
	}

	limit := parseLimit(r.URL.Query().Get("limit"))
	items, err := s.nodes.SearchPage(r.Context(), p.User.ID, nodes.SearchOptions{
		Keyword: query,
		Prefix:  scoped,
		Mime:    strings.TrimSpace(r.URL.Query().Get("mime")),
		Kind:    strings.TrimSpace(r.URL.Query().Get("type")),
	}, limit+1, cursor)
	if err != nil {
		return err
	}

	var next *string
	if len(items) > limit {
		items = items[:limit]
		if len(items) > 0 {
			encoded := encodeCursor(&items[len(items)-1])
			next = &encoded
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"writable": p.CanWrite(scoped),
		"items":    toAPINodes(items),
		"cursor":   next,
	})
	return nil
}
