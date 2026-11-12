package api

import (
	"errors"
	"log/slog"
	"net/http"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
)

// principal resolves a Bearer token or the cookie session for the public API.
func (s *Server) principal(r *http.Request) (*auth.Principal, error) {
	return s.auth.Authenticate(r.Context(), r)
}

// ---- GET /me ----

func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	bytes, files, err := s.nodes.Usage(r.Context(), p.User.ID)
	if err != nil {
		return err
	}

	var token any
	if p.Token != nil {
		token = map[string]any{
			"name":          p.Token.Name,
			"prefix":        p.Token.Prefix,
			"read":          p.Token.CanRead,
			"write":         p.Token.CanWrite,
			"events":        p.Token.CanEvents,
			"changes":       p.Token.CanChanges,
			"subscribe":     p.Token.CanSubscribe,
			"policyVersion": p.Token.PolicyVersion,
			"expiresAt":     p.Token.ExpiresAt,
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user":  map[string]any{"email": p.User.Email},
		"token": token,
		"usage": map[string]any{"bytes": bytes, "files": files},
	})
	return nil
}

// ---- public API errors ----

func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusPreconditionFailed:
		return "precondition_failed"
	case http.StatusLengthRequired:
		return "length_required"
	case http.StatusGone:
		return "gone"
	case http.StatusRequestEntityTooLarge:
		return "too_large"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusInsufficientStorage:
		return "quota_exceeded"
	default:
		return "internal"
	}
}

// writeAPIError writes the public API error body: {"error":"code","message":"..."}.
func writeAPIError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	code := "internal"
	message := "internal server error"

	var apiErr *httpx.Error
	var pathErr *paths.Error
	switch {
	case errors.As(err, &apiErr):
		status = apiErr.Status
		code = codeForStatus(status)
		message = apiErr.Message
	case errors.As(err, &pathErr):
		status = http.StatusBadRequest
		code = "invalid_path"
		message = pathErr.Message
	default:
		slog.Error("api", "error", err)
		message = err.Error()
	}

	httpx.WriteJSON(w, status, map[string]string{"error": code, "message": message})
}
