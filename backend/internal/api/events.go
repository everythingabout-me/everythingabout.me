package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"everythingabout.me/backend/internal/events"
	"everythingabout.me/backend/internal/httpx"
)

// ---------- web: read the timeline ----------

// handleTimeline is the web timeline: one flat event stream, newest first.
//
// Parameters:
//
//	cursor=...  cursor of the last item on the previous page (opaque)
//	limit=20    page size, 1..50
//
// There is no more "pagination by month" or "month heading": that was the v0.2 dashboard era, see
// docs/design/timeline.md §17.
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

// ---------- web: delete one event ----------

// handleDeleteEvent is the user delete: tombstone plus content wipe (see events.DeleteOwned).
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

// ---------- public API: write events ----------

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

// handleAPICreateEvent is where third-party apps submit events.
//
//   - idempotent: the same (token, external_id) again replaces it and returns the same id;
//   - media paths must sit under the token's prefix (the file API's scope rule, reused);
//   - pushing an event the user deleted returns 410, and the app should stop retrying.
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

// handleAPIDeleteEvent is the app-side delete: content stays, link cleared (state=detached).
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

// handleAPIListEvents is for app-side reconciliation: only events the app itself wrote.
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

// handleAPIGetEvent returns one event (so an app can render its own share page).
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
