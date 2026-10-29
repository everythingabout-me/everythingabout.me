package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
)

// The Connect page creates two kinds of connection: an app, handed over with a QR code,
// and a token, copied as a text block. Both are rows in api_tokens; kind only changes the
// defaults, the copy and the lifetime cap.

const (
	connectLimitPerUser = 50
	connectPerHour      = 30
	connectQRSize       = 512
)

type connectBody struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Prefix    string `json:"prefix"`
	Read      bool   `json:"read"`
	Write     bool   `json:"write"`
	Events    bool   `json:"events"`
	ExpiresIn int64  `json:"expiresIn"` // seconds; 0 = never (apps only)
}

// connectPayload is the JSON encoded into the QR code. The app only needs api + token;
// the rest is display metadata and is verified again through GET /me.
type connectPayload struct {
	V         int     `json:"v"`
	Type      string  `json:"type"`
	API       string  `json:"api"`
	ID        string  `json:"id"`
	Token     string  `json:"token"`
	Name      string  `json:"name"`
	Scope     string  `json:"scope"`
	Read      bool    `json:"read"`
	Write     bool    `json:"write"`
	Events    bool    `json:"events"`
	ExpiresAt *string `json:"expiresAt"`
}

func buildConnectPayload(api string, token *auth.Token, secret string) (string, error) {
	encoded, err := json.Marshal(connectPayload{
		V:         1,
		Type:      "eam.connect",
		API:       api,
		ID:        token.ID,
		Token:     secret,
		Name:      token.Name,
		Scope:     token.Prefix,
		Read:      token.CanRead,
		Write:     token.CanWrite,
		Events:    token.CanEvents,
		ExpiresAt: token.ExpiresAt,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (s *Server) handleConnectList(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	tokens, err := s.auth.ListTokens(r.Context(), user.ID)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{
		"tokens": tokens,
		"api":    requestBaseURL(r),
	})
}

func (s *Server) handleConnectCreate(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.limiter.Enforce("connect-create:"+user.ID, connectPerHour, time.Hour); err != nil {
		return err
	}

	body, err := httpx.ReadJSON[connectBody](r)
	if err != nil {
		return err
	}
	if body.ExpiresIn < 0 {
		return httpx.BadRequest("expiresIn cannot be negative")
	}
	// Bound the value before it is converted to a Duration; the per-kind cap is in auth.
	if body.ExpiresIn > int64((366 * 24 * time.Hour).Seconds()) {
		return httpx.BadRequest("expiresIn is too large")
	}

	active, err := s.auth.CountActiveTokens(r.Context(), user.ID)
	if err != nil {
		return err
	}
	if active >= connectLimitPerUser {
		return httpx.Conflict("too many active connections; revoke one first")
	}

	var expiry *time.Time
	if body.ExpiresIn > 0 {
		at := time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
		expiry = &at
	}

	secret, token, err := s.auth.CreateToken(r.Context(), auth.NewTokenInput{
		UserID:    user.ID,
		Name:      body.Name,
		Kind:      body.Kind,
		Prefix:    body.Prefix,
		CanRead:   body.Read,
		CanWrite:  body.Write,
		CanEvents: body.Events,
		Expiry:    expiry,
	})
	if err != nil {
		return err
	}

	api := requestBaseURL(r)
	payload, err := buildConnectPayload(api, token, secret)
	if err != nil {
		return err
	}
	png, err := qrcode.Encode(payload, qrcode.Medium, connectQRSize)
	if err != nil {
		// The secret only exists in this response; without a QR the app cannot get it.
		// Revoke the row so a failed handoff does not leave a usable credential behind.
		_ = s.auth.RevokeToken(r.Context(), user.ID, token.ID)
		return httpx.Internal("failed to render the QR code")
	}

	s.recordAudit(r, &auth.Principal{User: user}, "connection.create", "", http.StatusCreated, map[string]any{
		"title": token.Name,
		"kind":  token.Kind,
		"scope": token.Prefix,
	})

	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"ok": true,
		"data": map[string]any{
			"token":   token,
			"secret":  secret,
			"api":     api,
			"payload": payload,
			"qr":      "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		},
	})
	return nil
}

func (s *Server) handleConnectRevoke(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := s.auth.RevokeToken(r.Context(), user.ID, id); err != nil {
		return err
	}
	s.recordAudit(r, &auth.Principal{User: user}, "connection.revoke", "", http.StatusOK, map[string]any{
		"id": id,
	})
	return s.ok(w, map[string]any{"revoked": true})
}
