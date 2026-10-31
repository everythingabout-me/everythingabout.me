package api

import (
	"math"
	"net/http"
	"strings"
	"time"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/jobs"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
)

const (
	listLimit   = 5000
	searchLimit = 200
	// COS caps one object at 48.82 TB; this stays well below that.
	maxFileSize = int64(48) * 1024 * 1024 * 1024
)

type pathBody struct {
	Path string `json:"path"`
}

type signBody struct {
	Path      string  `json:"path"`
	Size      float64 `json:"size"`
	Overwrite bool    `json:"overwrite"`
}

type confirmBody struct {
	Path   string `json:"path"`
	Mime   string `json:"mime"`
	SHA256 string `json:"sha256"`
	// ModifiedAt is the browser's File.lastModified (milliseconds). When uploading old
	// photos it is closer to the real time than object storage's LastModified (= today).
	ModifiedAt int64 `json:"modifiedAt"`
}

type renameBody struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type crumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	path, err := paths.Normalize(r.URL.Query().Get("path"))
	if err != nil {
		return err
	}

	// node tells the web app whether this path is a folder (is_dir), a file to open on its detail
	// page, or missing (null). The root has no row, so it is null there too.
	var node *nodes.Row
	if path != "" {
		node, err = s.nodes.Live(r.Context(), path, user.ID)
		if err != nil {
			return err
		}
	}

	items, err := s.nodes.List(r.Context(), user.ID, path, listLimit)
	if err != nil {
		return err
	}
	if items == nil {
		items = []nodes.Row{}
	}

	breadcrumb := []crumb{{Name: "全部文件", Path: ""}}
	cursor := ""
	for _, segment := range strings.Split(path, "/") {
		if segment == "" {
			continue
		}
		if cursor == "" {
			cursor = segment
		} else {
			cursor += "/" + segment
		}
		breadcrumb = append(breadcrumb, crumb{Name: segment, Path: cursor})
	}

	return s.ok(w, map[string]any{
		"path":       path,
		"parent":     paths.ParentOf(path),
		"breadcrumb": breadcrumb,
		"truncated":  len(items) >= listLimit,
		"node":       node,
		"items":      items,
	})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	keyword := strings.TrimSpace(r.URL.Query().Get("q"))
	if keyword == "" {
		return s.ok(w, map[string]any{"items": []nodes.Row{}})
	}

	items, err := s.nodes.Search(r.Context(), user.ID, keyword, searchLimit)
	if err != nil {
		return err
	}
	if items == nil {
		items = []nodes.Row{}
	}
	return s.ok(w, map[string]any{"items": items})
}

func (s *Server) handleUploadSign(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[signBody](r)
	if err != nil {
		return err
	}
	path, err := paths.Normalize(body.Path)
	if err != nil {
		return err
	}
	if path == "" {
		return httpx.BadRequest("path cannot be empty")
	}
	if math.IsNaN(body.Size) || math.IsInf(body.Size, 0) || body.Size < 0 {
		return httpx.BadRequest("invalid file size")
	}
	if body.Size > float64(maxFileSize) {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "a single file is limited to 48GB")
	}

	existing, err := s.nodes.Live(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.IsDir {
		return httpx.Conflict("a directory with the same name already exists")
	}
	if existing != nil && !body.Overwrite {
		return httpx.Conflict("file already exists; send overwrite: true to overwrite")
	}

	key := paths.KeyFor(path, user.ID)
	credentials, err := s.storage.TemporaryCredentials(r.Context(), key, 3600)
	if err != nil {
		return err
	}

	if err := s.watchUpload(r.Context(), user.ID, path, false, time.Unix(credentials.ExpiredTime, 0)); err != nil {
		return err
	}

	return s.ok(w, map[string]any{
		"bucket":      s.cfg.COS.Bucket,
		"region":      s.cfg.COS.Region,
		"key":         key,
		"credentials": credentials,
	})
}

func (s *Server) handleUploadConfirm(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[confirmBody](r)
	if err != nil {
		return err
	}
	path, err := paths.Normalize(body.Path)
	if err != nil {
		return err
	}
	if path == "" {
		return httpx.BadRequest("path cannot be empty")
	}

	meta := map[string]any{}
	var mime *string
	if body.Mime != "" {
		meta["mime"] = body.Mime
		mime = &body.Mime
	}
	if body.SHA256 != "" {
		meta["sha256"] = body.SHA256
	}

	var modified *time.Time
	if body.ModifiedAt > 0 {
		candidate := time.UnixMilli(body.ModifiedAt)
		if candidate.Year() >= 1990 && candidate.Before(time.Now().AddDate(0, 0, 2)) {
			modified = &candidate
		}
	}
	node, err := s.nodes.ConfirmUpload(r.Context(), nodes.UpsertInput{
		OwnerID:          user.ID,
		Path:             path,
		Mime:             mime,
		SourceModifiedAt: modified,
		Meta:             meta,
	})
	if err != nil {
		return err
	}
	s.settleUpload(r.Context(), user.ID, path, false)
	s.timeline.Notify(user.ID)
	return s.ok(w, map[string]any{"node": node})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.assertBackendRoute(r); err != nil {
		httpx.WriteError(w, err)
		return
	}
	asJSON := r.URL.Query().Get("json") == "1"

	err := func() error {
		user, err := s.requireUser(r)
		if err != nil {
			return err
		}
		path, err := paths.Normalize(r.URL.Query().Get("path"))
		if err != nil {
			return err
		}
		node, err := s.nodes.Live(r.Context(), path, user.ID)
		if err != nil {
			return err
		}
		if node == nil || node.IsDir {
			return httpx.NotFound("file not found")
		}

		url, err := s.storage.GetURL(r.Context(), node.Key, storage.URLOptions{
			DownloadName: node.Name,
			ContentType:  deref(node.Mime),
		})
		if err != nil {
			return err
		}

		if asJSON {
			return s.ok(w, map[string]any{"url": url, "name": node.Name, "size": node.Size})
		}
		http.Redirect(w, r, url, http.StatusFound)
		return nil
	}()
	if err != nil {
		httpx.WriteError(w, err)
	}
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[pathBody](r)
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

	existing, err := s.nodes.Live(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		return httpx.Conflict("an item with the same name already exists")
	}

	if err := s.nodes.EnsureParents(r.Context(), path, user.ID); err != nil {
		return err
	}
	if err := s.watchUpload(r.Context(), user.ID, path, true, time.Now().Add(time.Hour)); err != nil {
		return err
	}
	if err := s.storage.PutEmpty(r.Context(), paths.DirKeyFor(path, user.ID)); err != nil {
		return err
	}

	node, err := s.nodes.CreateDirectory(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	s.settleUpload(r.Context(), user.ID, path, true)
	var payload any
	if node != nil {
		payload = node
	}
	return s.ok(w, map[string]any{"node": payload})
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[renameBody](r)
	if err != nil {
		return err
	}
	from, err := paths.Normalize(body.From)
	if err != nil {
		return err
	}
	to, err := paths.Normalize(body.To)
	if err != nil {
		return err
	}
	if err := nodes.AssertNotRoot(from); err != nil {
		return err
	}
	if err := nodes.AssertNotRoot(to); err != nil {
		return err
	}
	if from == to {
		return httpx.BadRequest("path did not change")
	}

	source, err := s.nodes.Live(r.Context(), from, user.ID)
	if err != nil {
		return err
	}
	if source == nil {
		return httpx.NotFound("source file not found")
	}

	collision, err := s.nodes.Live(r.Context(), to, user.ID)
	if err != nil {
		return err
	}
	if collision != nil {
		return httpx.Conflict("target path already exists")
	}

	// A directory is a subtree: renaming it means copying every object inside, so it runs as a
	// background move and reports progress at its own subpath. Files stay one synchronous copy.
	if source.IsDir {
		if paths.IsInside(to, from) {
			return httpx.BadRequest("target cannot be inside the source")
		}
		if err := s.assertParentDirs(r.Context(), user.ID, to); err != nil {
			return err
		}
		job, err := s.runJob(r, jobs.OpMove, user.ID, from, to, parsePrefer(r.Header.Get("Prefer")))
		if err != nil {
			return err
		}
		if job.Status == "failed" {
			return httpx.Internal(job.Error)
		}
		if job.Status == "done" {
			s.timeline.Notify(user.ID)
		}
		w.Header().Set("Location", "/api/fs/rename/"+job.ID)
		status := http.StatusAccepted
		if job.Terminal() {
			status = http.StatusOK
		} else {
			w.Header().Set("Retry-After", "2")
		}
		httpx.WriteOKStatus(w, status, map[string]any{"job": job})
		return nil
	}

	if err := s.nodes.EnsureParents(r.Context(), to, user.ID); err != nil {
		return err
	}

	targetKey := paths.KeyFor(to, user.ID)
	if err := s.storage.Copy(r.Context(), source.Key, targetKey); err != nil {
		return err
	}
	if err := s.storage.Remove(r.Context(), source.Key); err != nil {
		return err
	}

	node, err := s.nodes.Move(r.Context(), source.ID, to, user.ID)
	if err != nil {
		return err
	}
	s.timeline.Notify(user.ID)
	return s.ok(w, map[string]any{"node": node})
}

// GET /api/fs/rename/{id}: progress of a directory rename that answered 202.
func (s *Server) handleRenameStatus(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	job, err := s.jobs.Get(r.Context(), user.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if job == nil {
		return httpx.NotFound("rename job not found")
	}
	return s.ok(w, map[string]any{"job": job})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	body, err := httpx.ReadJSON[pathBody](r)
	if err != nil {
		return err
	}
	path, err := paths.Normalize(body.Path)
	if err != nil {
		return err
	}
	if err := nodes.AssertNotRoot(path); err != nil {
		return err
	}

	// permanent delete: drop the object and the index row together.
	count, err := s.nodes.Purge(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if count == 0 {
		return httpx.NotFound("nothing to delete")
	}
	s.timeline.Notify(user.ID)
	return s.ok(w, map[string]any{"deleted": count})
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
