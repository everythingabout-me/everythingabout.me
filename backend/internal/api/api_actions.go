package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
)

const (
	// 没带 Prefer 时，服务端最多同步等这么久；超过就回 202。
	defaultSyncBudget = 30 * time.Second
	// Prefer: respond-async, wait=N 里 N 的上限。
	maxAsyncWait = 60 * time.Second
)

// preferAsync 是 RFC 7240 的 Prefer: respond-async[, wait=N]。
// 服务端允许忽略它（RFC 也这么写），所以客户端必须两种响应都认。
type preferAsync struct {
	Requested bool
	Wait      time.Duration
}

// parsePrefer 只认 respond-async 和 wait=N，其余偏好一律忽略。
// wait 既可以写在同一个 preference 里（respond-async; wait=10），
// 也可以像 RFC 例子那样平铺（respond-async, wait=10）。
func parsePrefer(header string) preferAsync {
	var pref preferAsync
	for _, item := range strings.Split(header, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		main, rest, _ := strings.Cut(item, ";")
		main = strings.ToLower(strings.TrimSpace(main))
		if main == "respond-async" {
			pref.Requested = true
		}
		pref.applyParam(main)
		for _, param := range strings.Split(rest, ";") {
			pref.applyParam(strings.ToLower(strings.TrimSpace(param)))
		}
	}
	return pref
}

func (p *preferAsync) applyParam(param string) {
	value, ok := strings.CutPrefix(param, "wait=")
	if !ok {
		return
	}
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 0 {
		return
	}
	wait := time.Duration(seconds) * time.Second
	if wait > maxAsyncWait {
		wait = maxAsyncWait
	}
	p.Wait = wait
}

// Budget 是这次请求愿意同步等多久。0 表示"别等，立刻回 202"。
func (p preferAsync) Budget() time.Duration {
	if p.Requested {
		return p.Wait
	}
	return defaultSyncBudget
}

// writeJobResult 把一个动作的结果写回去：做完了 200，没做完 202 + Location。
// 两边的 body 同形，客户端一个解析器就够。
func writeJobResult(w http.ResponseWriter, action string, job *jobs.Job) {
	if job.Status == "failed" {
		httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{
			"error":   "job_failed",
			"message": job.Error,
			"id":      job.ID,
			"status":  job.Status,
		})
		return
	}
	w.Header().Set("Location", "/"+action+"/"+job.ID)
	if job.Terminal() {
		httpx.WriteJSON(w, http.StatusOK, job)
		return
	}
	w.Header().Set("Retry-After", "2")
	httpx.WriteJSON(w, http.StatusAccepted, job)
}

func auditStatus(job *jobs.Job) int {
	if job.Status == "failed" {
		return http.StatusInternalServerError
	}
	if job.Terminal() {
		return http.StatusOK
	}
	return http.StatusAccepted
}

// runJob 起一个后台作业，然后按 Prefer 的预算等一会儿。
func (s *Server) runJob(
	r *http.Request,
	op jobs.Op,
	userID, from, to string,
	pref preferAsync,
) (*jobs.Job, error) {
	job, err := s.jobs.Start(r.Context(), op, userID, from, to)
	if err != nil {
		return nil, err
	}
	return s.jobs.Wait(r.Context(), userID, job.ID, pref.Budget())
}

// assertParentDirs 防止在文件底下长出一棵子树（POSIX 也不允许）。
func (s *Server) assertParentDirs(ctx context.Context, userID, path string) error {
	for _, dir := range paths.AncestorsOf(path) {
		node, err := s.nodes.Live(ctx, dir, userID)
		if err != nil {
			return err
		}
		if node != nil && !node.IsDir {
			return httpx.Conflict("parent path " + dir + " is a file")
		}
	}
	return nil
}

// ---- POST /mkdir ----

type apiMkdirBody struct {
	Path string `json:"path"`
}

func (s *Server) handleAPIMkdir(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiMkdirBody](r)
	if err != nil {
		return err
	}
	path, err := paths.Normalize(body.Path)
	if err != nil {
		return err
	}
	if path == "" {
		return httpx.BadRequest("directory path cannot be empty")
	}
	if err := p.RequireWrite(path); err != nil {
		return err
	}
	if err := s.assertParentDirs(r.Context(), p.User.ID, path); err != nil {
		return err
	}

	existing, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		if !existing.IsDir {
			return httpx.Conflict("a file with the same name already exists")
		}
		// 已经在了就是幂等的：agent 重试不该收到 409。
		httpx.WriteJSON(w, http.StatusOK, toAPINode(existing))
		return nil
	}

	// mkdir -p：父目录没有就一起建。
	if err := s.nodes.EnsureParents(r.Context(), path, p.User.ID); err != nil {
		return err
	}
	if err := s.storage.PutEmpty(r.Context(), paths.DirKeyFor(path, p.User.ID)); err != nil {
		return err
	}

	node, err := s.nodes.CreateDirectory(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if node == nil {
		// 并发建同一个目录，别人先落了库。
		if node, err = s.nodes.Live(r.Context(), path, p.User.ID); err != nil {
			return err
		}
	}
	if node == nil {
		return httpx.Internal("failed to create the directory")
	}

	s.recordAudit(r, p, "mkdir", path, http.StatusCreated, nil)
	httpx.WriteJSON(w, http.StatusCreated, toAPINode(node))
	return nil
}

// ---- POST /copy ----

type apiCopyBody struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

func (s *Server) handleAPICopy(w http.ResponseWriter, r *http.Request) error {
	p, err := s.principal(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[apiCopyBody](r)
	if err != nil {
		return err
	}
	from, to, err := sourceAndTarget(body.From, body.To, p)
	if err != nil {
		return err
	}
	if err := s.assertParentDirs(r.Context(), p.User.ID, to); err != nil {
		return err
	}
	pref := parsePrefer(r.Header.Get("Prefer"))

	source, err := s.nodes.Live(r.Context(), from, p.User.ID)
	if err != nil {
		return err
	}
	if source == nil {
		return httpx.NotFound("source file not found")
	}

	target, err := s.nodes.Live(r.Context(), to, p.User.ID)
	if err != nil {
		return err
	}

	if source.IsDir {
		if target != nil {
			return httpx.Conflict("target path already exists")
		}
		job, err := s.runJob(r, jobs.OpCopy, p.User.ID, from, to, pref)
		if err != nil {
			return err
		}
		s.recordAudit(r, p, "copy", from, auditStatus(job), map[string]any{"to": to, "job": job.ID})
		writeJobResult(w, "copy", job)
		return nil
	}

	if target != nil {
		if target.IsDir {
			return httpx.Conflict("target is a directory")
		}
		if !body.Overwrite {
			return httpx.Conflict("target path already exists")
		}
		// 覆盖 = 把目标彻底删掉，没有回收站可言。
		if _, err := s.nodes.Purge(r.Context(), to, p.User.ID); err != nil {
			return err
		}
	}
	if err := s.checkQuota(r.Context(), p.User.ID, source.Size, nil); err != nil {
		return err
	}
	if err := s.nodes.EnsureParents(r.Context(), to, p.User.ID); err != nil {
		return err
	}

	targetKey := paths.KeyFor(to, p.User.ID)
	if err := s.storage.Copy(r.Context(), source.Key, targetKey); err != nil {
		return err
	}

	var modified *time.Time
	if source.SourceModifiedAt != nil {
		if parsed, err := time.Parse(time.RFC3339, *source.SourceModifiedAt); err == nil {
			modified = &parsed
		}
	}

	node, err := s.nodes.UpsertFile(r.Context(), nodes.UpsertInput{
		OwnerID:          p.User.ID,
		Path:             to,
		Size:             source.Size,
		Mime:             source.Mime,
		ETag:             source.ETag,
		StorageClass:     source.StorageClass,
		SourceModifiedAt: modified,
		Meta:             sourceMeta(source),
	})
	if err != nil {
		return err
	}
	s.timeline.Notify(p.User.ID)

	status := http.StatusCreated
	if target != nil {
		status = http.StatusOK
	}
	s.recordAudit(r, p, "copy", from, status, map[string]any{"to": to, "size": source.Size})
	httpx.WriteJSON(w, status, toAPINode(node))
	return nil
}

// sourceAndTarget 校验 from/to：都不是根、不相等、不互相包含，并且都在令牌范围内。
func sourceAndTarget(rawFrom, rawTo string, p *auth.Principal) (string, string, error) {
	from, err := paths.Normalize(rawFrom)
	if err != nil {
		return "", "", err
	}
	to, err := paths.Normalize(rawTo)
	if err != nil {
		return "", "", err
	}
	if err := nodes.AssertNotRoot(from); err != nil {
		return "", "", err
	}
	if err := nodes.AssertNotRoot(to); err != nil {
		return "", "", err
	}
	if from == to {
		return "", "", httpx.BadRequest("path did not change")
	}
	if err := p.RequireWrite(from); err != nil {
		return "", "", err
	}
	if err := p.RequireWrite(to); err != nil {
		return "", "", err
	}
	if err := p.RequireRead(from); err != nil {
		return "", "", err
	}
	if paths.IsInside(to, from) {
		return "", "", httpx.BadRequest("target cannot be inside the source")
	}
	return from, to, nil
}

// sourceMeta 把源文件的 meta（sha256 之类）带过去，内容一样，指纹也一样。
func sourceMeta(row *nodes.Row) map[string]any {
	if len(row.Meta) == 0 {
		return nil
	}
	var meta map[string]any
	if err := json.Unmarshal(row.Meta, &meta); err != nil {
		return nil
	}
	return meta
}
