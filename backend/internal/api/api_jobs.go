package api

import (
	"net/http"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/jobs"
)

// findJob loads a job for one action, confirming it belongs to the current user and
// stays within the token's scope. Other people's jobs and missing ones are both 404,
// so existence never leaks.
func (s *Server) findJob(r *http.Request, op jobs.Op) (*auth.Principal, *jobs.Job, error) {
	p, err := s.principal(r)
	if err != nil {
		return nil, nil, err
	}
	job, err := s.jobs.Get(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		return nil, nil, err
	}
	if job == nil || job.Op != op {
		return nil, nil, httpx.NotFound("no such operation")
	}
	if err := p.RequireRead(job.From); err != nil {
		return nil, nil, err
	}
	if err := p.RequireRead(job.To); err != nil {
		return nil, nil, err
	}
	return p, job, nil
}

// jobGet backs both GET /move/{id} and GET /copy/{id}.
func (s *Server) jobGet(op jobs.Op) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		_, job, err := s.findJob(r, op)
		if err != nil {
			return err
		}
		httpx.WriteJSON(w, http.StatusOK, job)
		return nil
	}
}

// jobCancel asks the job to stop: if it is running, it stops once the current item is done.
func (s *Server) jobCancel(op jobs.Op) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, job, err := s.findJob(r, op)
		if err != nil {
			return err
		}
		updated, err := s.jobs.Cancel(r.Context(), p.User.ID, job.ID)
		if err != nil {
			return err
		}
		if updated == nil {
			return httpx.NotFound("no such operation")
		}
		s.recordAudit(r, p, "cancel", job.From, http.StatusOK, map[string]any{
			"job": job.ID, "op": string(op),
		})
		httpx.WriteJSON(w, http.StatusOK, updated)
		return nil
	}
}

// jobRetry resumes from where the job failed or was cancelled.
func (s *Server) jobRetry(op jobs.Op) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, job, err := s.findJob(r, op)
		if err != nil {
			return err
		}
		switch job.Status {
		case "pending", "running":
			return httpx.Conflict("this operation is still running; no retry needed")
		case "done":
			return httpx.Conflict("this operation already finished")
		}
		updated, err := s.jobs.Retry(r.Context(), p.User.ID, job.ID)
		if err != nil {
			return err
		}
		if updated == nil {
			return httpx.NotFound("no such operation")
		}
		w.Header().Set("Location", "/"+string(op)+"/"+updated.ID)
		s.recordAudit(r, p, "retry", job.From, http.StatusAccepted, map[string]any{
			"job": job.ID, "op": string(op),
		})
		httpx.WriteJSON(w, http.StatusAccepted, updated)
		return nil
	}
}
