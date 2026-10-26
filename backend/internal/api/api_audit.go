package api

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
)

// recordAudit 记一条写操作。审计失败不影响业务，只打日志。
func (s *Server) recordAudit(
	r *http.Request,
	p *auth.Principal,
	action, path string,
	status int,
	detail map[string]any,
) {
	if p == nil || p.User == nil {
		return
	}
	var tokenID any
	if p.Token != nil {
		tokenID = p.Token.ID
	}
	payload := []byte("{}")
	if len(detail) > 0 {
		if encoded, err := json.Marshal(detail); err == nil {
			payload = encoded
		}
	}
	if _, err := s.db.Pool().Exec(r.Context(),
		`insert into audit_log (user_id, token_id, action, path, status, ip, user_agent, detail)
		 values ($1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb)`,
		p.User.ID, tokenID, action, path, status,
		httpx.ClientIP(r), r.UserAgent(), string(payload)); err != nil {
		slog.Warn("audit: 写入失败", "action", action, "path", path, "error", err)
	}
}
