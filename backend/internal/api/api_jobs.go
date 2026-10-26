package api

import (
	"net/http"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/jobs"
)

// findJob 取出某个动作下的作业，并确认它属于当前用户、也没越出令牌范围。
// 别人的作业和不存在一律 404，不泄露存在性。
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
		return nil, nil, httpx.NotFound("没有这个操作记录")
	}
	if err := p.RequireRead(job.From); err != nil {
		return nil, nil, err
	}
	if err := p.RequireRead(job.To); err != nil {
		return nil, nil, err
	}
	return p, job, nil
}

// jobGet 是 GET /move/{id} 和 GET /copy/{id} 的共同实现。
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

// jobCancel 请求停下：正在跑的话，当前这一项搬完就停。
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
			return httpx.NotFound("没有这个操作记录")
		}
		s.recordAudit(r, p, "cancel", job.From, http.StatusOK, map[string]any{
			"job": job.ID, "op": string(op),
		})
		httpx.WriteJSON(w, http.StatusOK, updated)
		return nil
	}
}

// jobRetry 从失败/取消的地方接着做。
func (s *Server) jobRetry(op jobs.Op) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		p, job, err := s.findJob(r, op)
		if err != nil {
			return err
		}
		switch job.Status {
		case "pending", "running":
			return httpx.Conflict("这个操作还在跑，不用重试")
		case "done":
			return httpx.Conflict("这个操作已经完成了")
		}
		updated, err := s.jobs.Retry(r.Context(), p.User.ID, job.ID)
		if err != nil {
			return err
		}
		if updated == nil {
			return httpx.NotFound("没有这个操作记录")
		}
		w.Header().Set("Location", "/"+string(op)+"/"+updated.ID)
		s.recordAudit(r, p, "retry", job.From, http.StatusAccepted, map[string]any{
			"job": job.ID, "op": string(op),
		})
		httpx.WriteJSON(w, http.StatusAccepted, updated)
		return nil
	}
}
