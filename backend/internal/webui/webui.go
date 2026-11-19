// Package webui serves the built frontend and does the SPA fallback for React Router.
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
			"error": "no such API endpoint",
		})
		return
	}
	// Only GET and HEAD are accepted; other methods fall through to a 404
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
		// A missing top-level file with an extension is an asset and must 404 rather than
		// receiving index.html in place of a .js. Everything below a first segment is an
		// app address: /<handle>/Photos/2024/a.jpg is a real file detail page and ends in
		// an extension, so the rule cannot be "contains a dot".
		if missingAsset(clean) {
			http.NotFound(w, r)
			return
		}
	}

	h.serveIndex(w, r)
}

// missingAsset reports paths that are assets by shape: one segment ending in an
// extension (/logo.svg, /missing.png), or anything under the build's /assets/ directory.
// A path with a first segment and anything after it is a drive address.
func missingAsset(clean string) bool {
	if strings.HasPrefix(clean, "/assets/") {
		return true
	}
	trimmed := strings.Trim(clean, "/")
	return !strings.Contains(trimmed, "/") && strings.Contains(path.Base(clean), ".")
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request) {
	index := filepath.Join(h.root, "index.html")
	if info, err := os.Stat(index); err != nil || info.IsDir() {
		http.Error(w,
			"frontend is not built: run npm run build in web/, or npm run dev for Vite.",
			http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, index)
}
