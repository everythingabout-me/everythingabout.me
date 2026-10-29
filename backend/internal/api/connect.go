package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
)

// The Connect page creates two kinds of connection: an app, handed over with a QR code,
// and a token, copied as a text block. Both are rows in api_tokens; kind only changes the
// defaults, the copy and the lifetime cap. An app is minted nameless and names itself
// through PATCH /me; a token is named while it is minted.

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

// connectPayload is the JSON encoded into the QR code. It is a credential, not a policy
// snapshot: the page can change the scope, permissions and lifetime after the QR exists,
// and the app reads the current values from GET /me. Keeping them here would go stale.
type connectPayload struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	API   string `json:"api"`
	ID    string `json:"id"`
	Token string `json:"token"`
}

func buildConnectPayload(api string, token *auth.Token, secret string) (string, error) {
	encoded, err := json.Marshal(connectPayload{
		V:     1,
		Type:  "eam.connect",
		API:   api,
		ID:    token.ID,
		Token: secret,
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

// connectUpdateBody is the whole policy of an existing connection. The page can change all
// of it after the QR exists: the QR is only the credential, so nothing in it goes stale.
type connectUpdateBody struct {
	Prefix    string `json:"prefix"`
	Read      bool   `json:"read"`
	Write     bool   `json:"write"`
	Events    bool   `json:"events"`
	ExpiresIn int64  `json:"expiresIn"` // seconds; 0 = never (apps only)
}

func (s *Server) handleConnectUpdate(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}

	body, err := httpx.ReadJSON[connectUpdateBody](r)
	if err != nil {
		return err
	}
	if body.ExpiresIn < 0 {
		return httpx.BadRequest("expiresIn cannot be negative")
	}
	if body.ExpiresIn > int64((366 * 24 * time.Hour).Seconds()) {
		return httpx.BadRequest("expiresIn is too large")
	}

	var expiry *time.Time
	if body.ExpiresIn > 0 {
		at := time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
		expiry = &at
	}

	token, err := s.auth.UpdateToken(r.Context(), user.ID, r.PathValue("id"), auth.UpdateTokenInput{
		Prefix:    body.Prefix,
		CanRead:   body.Read,
		CanWrite:  body.Write,
		CanEvents: body.Events,
		Expiry:    expiry,
	})
	if err != nil {
		return err
	}

	w.Header().Set("Cache-Control", "no-store")
	return s.ok(w, map[string]any{"token": token})
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

// connectSelfBody is the only field a credential can change about itself. Basic info stays
// server-owned; the name is what the page shows in Connected apps.
type connectSelfBody struct {
	Name string `json:"name"`
}

// handleAPIPatchMe lets a client name the connection it authenticated with. An app is minted
// without a name, scans the QR, and reports its name here; the page then shows it.
func (s *Server) handleAPIPatchMe(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	if p.Token == nil {
		return httpx.BadRequest("PATCH /me needs a Bearer token")
	}
	if strings.HasPrefix(p.Token.ID, auth.SaveLinkTokenPrefix) {
		return httpx.BadRequest("a save link cannot set a connection name")
	}
	body, err := httpx.ReadJSON[connectSelfBody](r)
	if err != nil {
		return err
	}
	token, err := s.auth.SetTokenName(r.Context(), p.User.ID, p.Token.ID, body.Name)
	if err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"name":      token.Name,
		"prefix":    token.Prefix,
		"read":      token.CanRead,
		"write":     token.CanWrite,
		"events":    token.CanEvents,
		"expiresAt": token.ExpiresAt,
	})
	return nil
}
