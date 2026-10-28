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

// handleTimelineRebuild is the manual "reorganize now" entry point; normal use never needs it.
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
