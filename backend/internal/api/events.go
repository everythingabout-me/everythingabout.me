package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/events"
	"everythingabout.me/backend/internal/httpx"
)

// ---------- 网页：读时间线 ----------

// handleTimeline 是网页的时间线：一层扁平的事件流，按时间倒序。
//
// 参数：
//
//	cursor=...  上一页最后一条的游标（opaque）
//	limit=20    本页条数，1..50
//
// 不再有"按月分页"和"月标题"：那是 v0.2 仪表盘时代的东西，见 docs/design/timeline.md §17。
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	list, next, err := s.eventsSvc.List(r.Context(), user.ID, r.URL.Query().Get("cursor"), limit)
	if err != nil {
		return err
	}
	status, err := s.timeline.Status(r.Context(), user.ID)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{
		"events": list,
		"next":   next,
		"status": status,
	})
}

// ---------- 网页：删除一条事件 ----------

// handleDeleteEvent 是用户删除：写墓碑、清内容（见 events.DeleteOwned 的注释）。
func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.eventsSvc.DeleteOwned(r.Context(), user.ID, r.PathValue("id")); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"deleted": true})
}

// ---------- 对外 API：写事件 ----------

type createEventRequest struct {
	ExternalID string     `json:"external_id"`
	DetailURL  string     `json:"detail_url"`
	StartsAt   time.Time  `json:"starts_at"`
	EndsAt     *time.Time `json:"ends_at"`
	TZ         string     `json:"tz"`
	Precision  string     `json:"precision"`
	Title      string     `json:"title"`
	Type       string     `json:"type"`
	Payload    struct {
		Body    string          `json:"body"`
		Meta    string          `json:"meta"`
		Media   []string        `json:"media"`
		Metrics []events.Metric `json:"metrics"`
		HTML    string          `json:"html"`
		Height  int             `json:"height"`
	} `json:"payload"`
}

// handleAPICreateEvent 是三方应用的投稿入口。
//
//   - 幂等：同一个 (token, external_id) 再来一次是整体替换，返回同一个 id；
//   - 媒体路径必须在 token 的前缀里（复用文件 API 的越界规则）；
//   - 用户删过的事件再推 → 410，应用应停止重试。
func (s *Server) handleAPICreateEvent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	if err := p.RequireEvents(); err != nil {
		return err
	}
	if p.Token == nil {
		return httpx.Forbidden("events can only be written with an API token")
	}

	var req createEventRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		return httpx.BadRequest("invalid JSON body")
	}
	if req.StartsAt.IsZero() {
		return httpx.BadRequest("starts_at is required")
	}
	tz := req.TZ
	if tz == "" {
		tz = s.cfg.Timeline.Timezone
	}

	ev, created, err := s.eventsSvc.CreateApp(r.Context(), p.User.ID, p.Token.ID, p.Token.Prefix, events.AppInput{
		ExternalID: req.ExternalID,
		DetailURL:  req.DetailURL,
		StartsAt:   req.StartsAt,
		EndsAt:     req.EndsAt,
		TZ:         tz,
		Precision:  req.Precision,
		Title:      req.Title,
		Type:       req.Type,
		Payload: events.PayloadIn{
			Body:    req.Payload.Body,
			Meta:    req.Payload.Meta,
			Media:   req.Payload.Media,
			Metrics: req.Payload.Metrics,
			HTML:    req.Payload.HTML,
			Height:  req.Payload.Height,
		},
	})
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	action := "event.update"
	if created {
		action = "event.create"
	}
	s.recordAudit(r, p, action, "", status, map[string]any{
		"event": ev.ID, "external_id": req.ExternalID, "type": req.Type,
	})
	httpx.WriteJSON(w, status, map[string]any{
		"id":      ev.ID,
		"state":   "published",
		"created": created,
	})
	return nil
}

// handleAPIDeleteEvent 是应用侧删除：内容保留、外链清空（state=detached）。
func (s *Server) handleAPIDeleteEvent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	if err := p.RequireEvents(); err != nil {
		return err
	}
	if p.Token == nil {
		return httpx.Forbidden("events can only be managed with an API token")
	}
	if err := s.eventsSvc.DetachApp(r.Context(), p.User.ID, p.Token.ID, r.PathValue("id")); err != nil {
		return err
	}
	s.recordAudit(r, p, "event.delete", "", http.StatusOK, map[string]any{"event": r.PathValue("id")})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"state": "detached"})
	return nil
}

// handleAPIListEvents 给应用对账：只返回它自己写过的事件。
func (s *Server) handleAPIListEvents(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	if err := p.RequireEvents(); err != nil {
		return err
	}
	if p.Token == nil {
		return httpx.Forbidden("events can only be read with an API token")
	}

	var from, to *time.Time
	if raw := r.URL.Query().Get("from"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return httpx.BadRequest("from must be RFC3339")
		}
		from = &parsed
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return httpx.BadRequest("to must be RFC3339")
		}
		to = &parsed
	}

	list, err := s.eventsSvc.ListByToken(r.Context(), p.User.ID, p.Token.ID, from, to)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": list})
	return nil
}

// handleAPIGetEvent 返回单条事件（应用渲染自己的分享页用）。
func (s *Server) handleAPIGetEvent(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	if err := p.RequireEvents(); err != nil {
		return err
	}
	ev, err := s.eventsSvc.Get(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, ev)
	return nil
}
