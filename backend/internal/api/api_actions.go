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
	// without Prefer, the server waits synchronously this long at most; past that, 202.
	defaultSyncBudget = 30 * time.Second
	// upper bound for N in Prefer: respond-async, wait=N.
	maxAsyncWait = 60 * time.Second
)

// preferAsync is RFC 7240's Prefer: respond-async[, wait=N].
// Servers are allowed to ignore it (the RFC says as much), so clients must handle both responses.
type preferAsync struct {
	Requested bool
	Wait      time.Duration
}

// parsePrefer understands only respond-async and wait=N; every other preference is ignored.
// wait may ride inside the same preference (respond-async; wait=10), or be flat
// like the RFC example (respond-async, wait=10).
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

// Budget is how long this request is willing to wait synchronously. 0 means "don't wait, 202 now".
func (p preferAsync) Budget() time.Duration {
	if p.Requested {
		return p.Wait
	}
	return defaultSyncBudget
}

// writeJobResult writes an action's result back: 200 when done, 202 + Location when not.
// Both bodies share a shape, so one client parser covers both.
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

// runJob starts a background job, then waits for the Prefer budget.
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

// assertParentDirs stops a subtree from growing under a file (POSIX forbids that too).
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
		// already there means idempotent: an agent retry should not get a 409.
		httpx.WriteJSON(w, http.StatusOK, toAPINode(existing))
		return nil
	}

	// mkdir -p: create missing parents along the way.
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
		// a concurrent create of the same directory won the insert.
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
		// overwrite means the target is purged outright; there is no trash.
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

// sourceAndTarget validates from/to: neither is root, they differ, neither contains
// the other, and both stay inside the token's scope.
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

// sourceMeta carries the source's meta (sha256 and the like) over: same bytes, same fingerprint.
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
