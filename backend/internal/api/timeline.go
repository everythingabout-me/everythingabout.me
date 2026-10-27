package api

import (
	"net/http"

	"everythingabout.me/backend/internal/httpx"
)

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
