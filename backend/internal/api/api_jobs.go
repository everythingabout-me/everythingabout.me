package api

import (
	"net/http"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/paths"
)

// handleAPIMoveJob 查询目录移动的进度。
func (s *Server) handleAPIMoveJob(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	job, err := s.mover.Get(r.Context(), p.User.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if job == nil {
		return httpx.NotFound("任务不存在")
	}
	httpx.WriteJSON(w, http.StatusOK, job)
	return nil
}

// handleAPIMoveRetry 从失败的地方接着做。
func (s *Server) handleAPIMoveRetry(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	job, err := s.mover.Get(r.Context(), p.User.ID, id)
	if err != nil {
		return err
	}
	if job == nil {
		return httpx.NotFound("任务不存在")
	}
	if job.Status == "running" || job.Status == "done" {
		return httpx.Conflict("任务状态是 " + job.Status + "，不能重试")
	}
	if p.Token != nil && p.Token.Prefix != "" {
		if !paths.IsInside(job.From, p.Token.Prefix) || !paths.IsInside(job.To, p.Token.Prefix) {
			return httpx.Forbidden("这个令牌只能访问 " + p.Token.Prefix)
		}
	}

	updated, err := s.mover.Retry(r.Context(), p.User.ID, id)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusAccepted, updated)
	return nil
}
