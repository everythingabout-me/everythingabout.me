// Package httpx gathers the shared HTTP details: the response envelope, error types,
// Origin checks, client IPs, and JSON decoding.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"everythingabout.me/backend/internal/paths"
)

// Error is a business error that maps straight onto an HTTP status code.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func NewError(status int, message string) *Error {
	return &Error{Status: status, Message: message}
}

func BadRequest(message string) *Error   { return NewError(http.StatusBadRequest, message) }
func Unauthorized(message string) *Error { return NewError(http.StatusUnauthorized, message) }
func Forbidden(message string) *Error    { return NewError(http.StatusForbidden, message) }
func NotFound(message string) *Error     { return NewError(http.StatusNotFound, message) }
func Conflict(message string) *Error     { return NewError(http.StatusConflict, message) }
func TooManyRequests(m string) *Error    { return NewError(http.StatusTooManyRequests, m) }
func ServiceUnavailable(m string) *Error { return NewError(http.StatusServiceUnavailable, m) }
func BadGateway(message string) *Error   { return NewError(http.StatusBadGateway, message) }
func Internal(message string) *Error     { return NewError(http.StatusInternalServerError, message) }

type okEnvelope struct {
	OK   bool `json:"ok"`
	Data any  `json:"data"`
}

type errEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// WriteJSON writes a JSON response as-is.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// WriteOK writes {ok:true,data:...}, keeping the response shape from before the rewrite.
func WriteOK(w http.ResponseWriter, data any) {
	WriteJSON(w, http.StatusOK, okEnvelope{OK: true, Data: data})
}

// WriteError maps an error onto {ok:false,error:"..."}.
// Anything that is not a business error becomes a 500, with the original message kept in
// the text so it can be traced.
func WriteError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		WriteJSON(w, apiErr.Status, errEnvelope{OK: false, Error: apiErr.Message})
		return
	}
	var pathErr *paths.Error
	if errors.As(err, &pathErr) {
		WriteJSON(w, http.StatusBadRequest, errEnvelope{OK: false, Error: pathErr.Message})
		return
	}
	WriteJSON(w, http.StatusInternalServerError, errEnvelope{OK: false, Error: err.Error()})
}

// AssertSameOrigin compares Origin with Host. Requests without an Origin (curl, scripts)
// pass; SameSite=Lax already blocks the vast majority of cross-site POSTs, and this adds a
// layer against sibling subdomains.
func AssertSameOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return Forbidden("invalid request origin")
	}
	requestHost := forwardedHost(r)
	if requestHost != "" && !strings.EqualFold(parsed.Host, requestHost) {
		return Forbidden("cross-site request rejected")
	}
	return nil
}

func forwardedHost(r *http.Request) string {
	if value := r.Header.Get("X-Forwarded-Host"); value != "" {
		if first, _, _ := strings.Cut(value, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	return r.Host
}

// ReadJSON decodes the request body, capped at 1MB, and rejects content trailing the JSON.
func ReadJSON[T any](r *http.Request) (T, error) {
	var value T
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(&value); err != nil {
		return value, BadRequest("request body is not valid JSON")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, BadRequest("request body is not valid JSON")
	}
	return value, nil
}

// ClientIP takes the first address the reverse proxy reports.
func ClientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, _ := strings.Cut(forwarded, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	host := r.RemoteAddr
	if index := strings.LastIndex(host, ":"); index > 0 {
		host = host[:index]
	}
	if host == "" {
		return "local"
	}
	return host
}
