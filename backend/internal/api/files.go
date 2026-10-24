package api

import (
	"math"
	"net/http"
	"strings"

	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
	"everythingabout.me/backend/internal/storage"
)

const (
	listLimit   = 5000
	searchLimit = 200
	// COS 单对象上限 48.82TB，这里给个保守值。
	maxFileSize = int64(48) * 1024 * 1024 * 1024
)

type pathBody struct {
	Path string `json:"path"`
}

type signBody struct {
	Path string  `json:"path"`
	Size float64 `json:"size"`
}

type confirmBody struct {
	Path   string `json:"path"`
	Mime   string `json:"mime"`
	SHA256 string `json:"sha256"`
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
		return httpx.BadRequest("路径不能为空")
	}
	if math.IsNaN(body.Size) || math.IsInf(body.Size, 0) || body.Size <= 0 {
		return httpx.BadRequest("文件大小不合法")
	}
	if body.Size > float64(maxFileSize) {
		return httpx.NewError(http.StatusRequestEntityTooLarge, "单个文件超过 48GB")
	}

	existing, err := s.nodes.Live(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.IsDir {
		return httpx.Conflict("同名目录已存在")
	}

	if err := s.nodes.EvictTrashedAt(r.Context(), path, user.ID, false); err != nil {
		return err
	}
	key := paths.KeyFor(path, user.ID)
	credentials, err := s.storage.TemporaryCredentials(r.Context(), key, 3600)
	if err != nil {
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
		return httpx.BadRequest("路径不能为空")
	}

	existing, err := s.nodes.Live(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if existing != nil && existing.IsDir {
		return httpx.Conflict("同名目录已存在")
	}

	// HeadObject 校验后落库
	info, err := s.storage.Head(r.Context(), paths.KeyFor(path, user.ID))
	if err != nil {
		return err
	}
	if info == nil {
		return httpx.BadRequest("对象不存在，上传可能未完成")
	}

	if err := s.nodes.EnsureParents(r.Context(), path, user.ID); err != nil {
		return err
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

	var etag *string
	if info.ETag != "" {
		etag = &info.ETag
	}
	var storageClass *string
	if info.StorageClass != "" {
		storageClass = &info.StorageClass
	}

	node, err := s.nodes.UpsertFile(r.Context(), nodes.UpsertInput{
		OwnerID:      user.ID,
		Path:         path,
		Size:         info.Size,
		Mime:         mime,
		ETag:         etag,
		StorageClass: storageClass,
		Meta:         meta,
	})
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"node": node})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
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
			return httpx.NotFound("文件不存在")
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
		return httpx.BadRequest("目录路径不能为空")
	}

	existing, err := s.nodes.Live(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		return httpx.Conflict("同名文件或目录已存在")
	}

	if err := s.nodes.EnsureParents(r.Context(), path, user.ID); err != nil {
		return err
	}
	if err := s.nodes.EvictTrashedAt(r.Context(), path, user.ID, true); err != nil {
		return err
	}
	if err := s.storage.PutEmpty(r.Context(), paths.DirKeyFor(path, user.ID)); err != nil {
		return err
	}

	node, err := s.nodes.CreateDirectory(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
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
		return httpx.BadRequest("路径没有变化")
	}

	source, err := s.nodes.Live(r.Context(), from, user.ID)
	if err != nil {
		return err
	}
	if source == nil {
		return httpx.NotFound("源文件不存在")
	}
	if source.IsDir {
		return httpx.BadRequest(
			"目录重命名需要对每个对象做服务端复制，TB 级目录会很慢。请用 coscli 改名后再跑 reconcile。")
	}

	collision, err := s.nodes.Live(r.Context(), to, user.ID)
	if err != nil {
		return err
	}
	if collision != nil {
		return httpx.Conflict("目标路径已存在")
	}

	if err := s.nodes.EnsureParents(r.Context(), to, user.ID); err != nil {
		return err
	}
	if err := s.nodes.EvictTrashedAt(r.Context(), to, user.ID, false); err != nil {
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
	return s.ok(w, map[string]any{"node": node})
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

	// 软删除：只打标记，对象留在原地；purge 任务负责真正删对象
	count, err := s.nodes.SoftDelete(r.Context(), path, user.ID)
	if err != nil {
		return err
	}
	if count == 0 {
		return httpx.NotFound("没有可删除的文件")
	}
	return s.ok(w, map[string]any{"deleted": count})
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
