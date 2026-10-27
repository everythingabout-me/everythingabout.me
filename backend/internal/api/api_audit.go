package api

import (
	"context"
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
		slog.Warn("audit: failed to write entry", "action", action, "path", path, "error", err)
	}
}

// ActivityItem 是账户页"最近活动"的一条。来源优先显示保存链接名，
// 其次是 API token 名；网页自己的操作没有 token，来源为空。
type ActivityItem struct {
	Action    string `json:"action"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Source    string `json:"source"`
	Title     string `json:"title"`
	CreatedAt string `json:"createdAt"`
}

func (s *Server) recentActivity(ctx context.Context, userID string, limit int) ([]ActivityItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Pool().Query(ctx,
		`select a.action,
		        coalesce(a.path, ''),
		        a.status,
		        coalesce(sl.name, t.name, ''),
		        coalesce(a.detail->>'title', ''),
		        to_char(a.created_at at time zone 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
		   from audit_log a
		   left join save_links sl on a.token_id = 'link:' || sl.link_id
		   left join api_tokens t on a.token_id = t.token_id
		  where a.user_id = $1::uuid
		  order by a.created_at desc
		  limit $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]ActivityItem, 0, limit)
	for rows.Next() {
		var item ActivityItem
		if err := rows.Scan(&item.Action, &item.Path, &item.Status, &item.Source, &item.Title, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
