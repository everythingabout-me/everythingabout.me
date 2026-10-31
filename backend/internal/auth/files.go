package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"

	fc "everythingabout.me/backend/internal/filecontract"
)

// AuthenticateFiles selects by header presence before using existing token/session
// verification. Legacy account/event authentication semantics remain separate.
func (s *Service) AuthenticateFiles(ctx context.Context, r *http.Request) (*Principal, fc.CredentialSource, error) {
	selected, err := fc.SelectCredential(r.Header)
	if err != nil {
		return nil, selected.Source, err
	}
	request := r.Clone(ctx)
	request.Header = r.Header.Clone()
	if selected.Source == fc.BearerCredential {
		request.Header.Set("Authorization", "Bearer "+selected.Token)
	}
	principal, err := s.Authenticate(ctx, request)
	return principal, selected.Source, err
}

// FileCSRFToken is an opaque MAC of the random session credential under an
// independent application secret. It is returned only after cookie authentication;
// a copied token from another session cannot authorize mutations in this session.
func (s *Service) FileCSRFToken(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil || cookie.Value == "" || s.cfg.AppSecret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(s.cfg.AppSecret))
	mac.Write([]byte("eam/file-csrf/v1\x00"))
	mac.Write([]byte(cookie.Value))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (s *Service) ValidateFileCSRF(r *http.Request, token string) bool {
	expected := s.FileCSRFToken(r)
	return expected != "" && hmac.Equal([]byte(expected), []byte(token))
}
