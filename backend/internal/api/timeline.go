package api

import (
	"net/http"
	"strconv"

	"everythingabout.me/backend/internal/httpx"
)

// handleTimeline 读内容流。参数沿用"翻页"直觉：
//
//	before=2025-06  从哪个月开始（含），默认当前月
//	months=2        一次要几个月，1..6
//
// 只返回有内容的月份，中间的空档由前端表达。
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	months := 2
	if raw := r.URL.Query().Get("months"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			months = parsed
		}
	}
	feed, err := s.timeline.Feed(r.Context(), user.ID, r.URL.Query().Get("before"), months)
	if err != nil {
		return err
	}
	return s.ok(w, feed)
}

func (s *Server) handleTimelineStatus(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	status, err := s.timeline.Status(r.Context(), user.ID)
	if err != nil {
		return err
	}
	return s.ok(w, status)
}

// handleTimelineRebuild 是手动"重新整理"的入口，正常使用不需要。
func (s *Server) handleTimelineRebuild(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	if err := s.timeline.Rebuild(r.Context(), user.ID); err != nil {
		return err
	}
	return s.ok(w, map[string]any{"queued": true})
}
