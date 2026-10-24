// Package webui 服务前端构建产物，并为 React Router 做 SPA 回退。
package webui

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"everythingabout.me/backend/internal/httpx"
)

type Handler struct {
	root string
}

func New(dir string) http.Handler {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		absolute = dir
	}
	return &Handler{root: absolute}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "接口不存在",
		})
		return
	}
	// 只接受 GET / HEAD，其余方法交给 404
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}

	clean := path.Clean("/" + r.URL.Path)
	full := filepath.Join(h.root, filepath.FromSlash(clean))
	relative, err := filepath.Rel(h.root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	if clean != "/" {
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			http.ServeFile(w, r, full)
			return
		}
		// 带扩展名的资源找不到就是 404，不要把 index.html 当成 js 返回
		if strings.Contains(path.Base(clean), ".") {
			http.NotFound(w, r)
			return
		}
	}

	h.serveIndex(w, r)
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	index := filepath.Join(h.root, "index.html")
	if info, err := os.Stat(index); err != nil || info.IsDir() {
		http.Error(w,
			"前端还没有构建：在 web/ 运行 npm run build，或用 npm run dev 启动 Vite。",
			http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}
