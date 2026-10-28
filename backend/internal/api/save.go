package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
)

// 保存链接是给 agent 用的入口：URL 本身就是凭证，GET 返回 token + prompt，
// POST 把文本或二进制存成文件。设计见 docs/api/api.md §4.14。

const saveLinkMaxBody = apiMaxPutSize // 和 PUT /files 一样，32 MiB 以内直接收

// handleSaveLinkPage 渲染链接页：agent 默认拿到 Markdown prompt，
// 浏览器拿到一页说明，?format=json 给 App。
func (s *Server) handleSaveLinkPage(w http.ResponseWriter, r *http.Request) error {
	code := r.PathValue("code")
	p, err := s.auth.VerifySaveLink(r.Context(), code)
	if err != nil {
		return err
	}
	if p == nil {
		http.Error(w, "save link not found", http.StatusNotFound)
		return nil
	}

	base := requestBaseURL(r)
	linkURL := base + "/s/" + code
	prompt := saveLinkPrompt(base, code)

	format := r.URL.Query().Get("format")
	accept := strings.ToLower(r.Header.Get("Accept"))
	switch {
	case format == "json" || strings.Contains(accept, "application/json"):
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"token":      code,
			"url":        linkURL,
			"save":       map[string]any{"method": "POST", "url": linkURL, "contentType": "as sent"},
			"prompt":     prompt,
			"name":       p.Token.Name,
			"createdAt":  p.Token.CreatedAt,
			"lastUsedAt": p.Token.LastUsedAt,
		})
		return nil
	case format == "html" || strings.Contains(accept, "text/html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		lastUsed := ""
		if p.Token.LastUsedAt != nil {
			lastUsed = *p.Token.LastUsedAt
		}
		return saveLinkTemplate.Execute(w, map[string]any{
			"Code":      code,
			"URL":       linkURL,
			"Prompt":    prompt,
			"LastUsed":  lastUsed,
			"CreatedAt": p.Token.CreatedAt,
		})
	default:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, err := io.WriteString(w, prompt)
		return err
	}
}

// handleSaveLinkCapture 接收任意类型的内容，文本、图片、PDF、视频都走这里。
// 和 PUT /files 一样，32 MiB 以内由服务端落盘；更大的文件走 /upload 直传。
func (s *Server) handleSaveLinkCapture(w http.ResponseWriter, r *http.Request) error {
	p, err := s.auth.VerifySaveLink(r.Context(), r.PathValue("code"))
	if err != nil {
		return err
	}
	if p == nil {
		return httpx.NotFound("save link not found")
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, saveLinkMaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return httpx.NewError(http.StatusRequestEntityTooLarge,
				"content is too large; use POST /upload for files over 32MiB")
		}
		return httpx.BadRequest("failed to read request body")
	}
	if len(body) == 0 {
		return httpx.BadRequest("content cannot be empty")
	}

	contentType := cleanMediaType(r.Header.Get("Content-Type"))
	filename := saveLinkFilename(r)
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(filename))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" && isTextContent(contentType) {
		title = saveLinkTitle(string(body))
	}
	if title == "" && filename != "" {
		title = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	}
	if title == "" {
		title = "Saved file"
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80])
	}

	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		source = p.Token.Name
	}

	now := time.Now()
	if loc, err := time.LoadLocation(s.cfg.Timeline.Timezone); err == nil {
		now = now.In(loc)
	}
	hash := sha256.Sum256(body)
	hashName := hex.EncodeToString(hash[:])
	ext := saveLinkExtension(filename, contentType)
	slug := fileSlug(title)
	path := fmt.Sprintf("Inbox/%s/%s%s", now.Format("2006/01/02"), hashName[:12], ext)
	if slug != "" {
		path = fmt.Sprintf("Inbox/%s/%s-%s%s", now.Format("2006/01/02"), slug, hashName[:8], ext)
	}

	existing, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		s.recordAudit(r, p, "capture", path, http.StatusOK, map[string]any{
			"size": existing.Size, "title": title, "link": p.Token.Name, "duplicate": true,
		})
		writeCaptureResult(w, r, http.StatusOK, path, title, contentType, existing.Size, true)
		return nil
	}

	size := int64(len(body))
	if err := s.checkQuota(r.Context(), p.User.ID, size, nil); err != nil {
		return err
	}
	if err := s.assertParentDirs(r.Context(), p.User.ID, path); err != nil {
		return err
	}
	if err := s.nodes.EnsureParents(r.Context(), path, p.User.ID); err != nil {
		return err
	}

	info, err := s.storage.Put(r.Context(), paths.KeyFor(path, p.User.ID), bytes.NewReader(body), size, contentType)
	if err != nil {
		return err
	}

	var etag, storageClass *string
	if info.ETag != "" {
		etag = &info.ETag
	}
	if info.StorageClass != "" {
		storageClass = &info.StorageClass
	}
	var modified *time.Time
	if !info.LastModified.IsZero() {
		modified = &info.LastModified
	}
	_, err = s.nodes.UpsertFile(r.Context(), nodes.UpsertInput{
		OwnerID:          p.User.ID,
		Path:             path,
		Size:             info.Size,
		Mime:             &contentType,
		ETag:             etag,
		StorageClass:     storageClass,
		SourceModifiedAt: modified,
		Meta: map[string]any{
			"mime": contentType, "sha256": hashName, "title": title, "source": source,
			"captured_at": now.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return err
	}
	s.timeline.Notify(p.User.ID)
	s.recordAudit(r, p, "capture", path, http.StatusCreated, map[string]any{
		"size": info.Size, "title": title, "link": p.Token.Name, "mime": contentType,
	})
	writeCaptureResult(w, r, http.StatusCreated, path, title, contentType, info.Size, false)
	return nil
}

func writeCaptureResult(w http.ResponseWriter, r *http.Request, status int, path, title, contentType string, size int64, duplicate bool) {
	httpx.WriteJSON(w, status, map[string]any{
		"path":      path,
		"url":       requestBaseURL(r) + "/drive?path=" + url.QueryEscape(path),
		"title":     title,
		"mime":      contentType,
		"size":      size,
		"duplicate": duplicate,
	})
}

func cleanMediaType(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(raw); err == nil {
		return parsed
	}
	return strings.ToLower(raw)
}

func isTextContent(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		contentType == "application/json" ||
		contentType == "application/xml" ||
		contentType == "application/x-www-form-urlencoded"
}

// saveLinkFilename 先看 ?filename=，再看 Content-Disposition。
func saveLinkFilename(r *http.Request) string {
	if name := strings.TrimSpace(r.URL.Query().Get("filename")); name != "" {
		return filepath.Base(name)
	}
	raw := r.Header.Get("Content-Disposition")
	if raw == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(raw); err == nil {
		if name := params["filename"]; name != "" {
			return filepath.Base(name)
		}
	}
	return ""
}

func saveLinkExtension(filename, contentType string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if validExtension(ext) {
		return ext
	}
	switch contentType {
	case "text/markdown":
		return ".md"
	case "text/plain", "application/x-www-form-urlencoded":
		return ".txt"
	case "application/json":
		return ".json"
	case "application/pdf":
		return ".pdf"
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/quicktime":
		return ".mov"
	}
	if extensions, err := mime.ExtensionsByType(contentType); err == nil && len(extensions) > 0 {
		return extensions[0]
	}
	return ".bin"
}

func validExtension(ext string) bool {
	if len(ext) < 2 || len(ext) > 11 || ext[0] != '.' {
		return false
	}
	for _, char := range ext[1:] {
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) {
			return false
		}
	}
	return true
}

func saveLinkTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len([]rune(line)) > 80 {
			line = string([]rune(line)[:80])
		}
		return line
	}
	return "Saved note"
}

// fileSlug 把标题变成文件名可用的短横线串；中文、字母、数字保留。
func fileSlug(title string) string {
	var buf strings.Builder
	lastDash := false
	for _, char := range strings.TrimSpace(title) {
		ok := unicode.IsLetter(char) || unicode.IsDigit(char) || char == '-' || char == '_'
		if ok {
			buf.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && buf.Len() > 0 {
			buf.WriteByte('-')
			lastDash = true
		}
	}
	slug := strings.Trim(buf.String(), "-")
	if len([]rune(slug)) > 48 {
		slug = strings.TrimRight(string([]rune(slug)[:48]), "-")
	}
	return slug
}

func saveLinkPrompt(base, code string) string {
	return fmt.Sprintf(`# everythingabout.me

Connected to this drive.

Token: %s
Save: POST %s/s/%s
Content-Type: text/markdown (or image/jpeg, application/pdf, video/mp4, ...)

  <content or file bytes>

Optional query: ?title=...&filename=photo.jpg
Response: {"path": "...", "url": "...", "mime": "...", "size": ...}

Files over 32 MiB:
  POST %s/upload
  {"path": "Inbox/2026/10/28/photo.jpg", "size": 12345678, "mime": "image/jpeg"}
  PUT the bytes to each returned part URL, then POST %s/upload/complete

Full API: Authorization: Bearer %s
`, code, base, code, base, base, code)
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		if first, _, ok := strings.Cut(forwarded, ","); ok {
			forwarded = first
		}
		if value := strings.TrimSpace(forwarded); value != "" {
			scheme = value
		}
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		if first, _, ok := strings.Cut(forwarded, ","); ok {
			forwarded = first
		}
		if value := strings.TrimSpace(forwarded); value != "" {
			host = value
		}
	}
	return scheme + "://" + host
}

// ---------- 网页：账户页的链接列表 ----------

func (s *Server) handleListSaveLinks(w http.ResponseWriter, r *http.Request) error {
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	links, err := s.auth.ListSaveLinks(r.Context(), user.ID)
	if err != nil {
		return err
	}
	return s.ok(w, map[string]any{"links": links})
}

func (s *Server) handleCreateSaveLink(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	link, err := s.auth.CreateSaveLink(r.Context(), user.ID)
	if err != nil {
		return err
	}
	s.recordAudit(r, &auth.Principal{User: user}, "save_link.create", "", http.StatusCreated, map[string]any{
		"link": link.Name,
	})
	return s.ok(w, map[string]any{"link": link})
}

func (s *Server) handleRevokeSaveLink(w http.ResponseWriter, r *http.Request) error {
	if err := httpx.AssertSameOrigin(r); err != nil {
		return err
	}
	user, err := s.requireUser(r)
	if err != nil {
		return err
	}
	id := r.PathValue("id")
	if err := s.auth.RevokeSaveLink(r.Context(), user.ID, id); err != nil {
		return err
	}
	s.recordAudit(r, &auth.Principal{User: user}, "save_link.revoke", "", http.StatusOK, map[string]any{
		"id": id,
	})
	return s.ok(w, map[string]any{"revoked": true})
}

const saveLinkHTML = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>保存链接 · Everything About Me</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; padding: 48px 24px; font: 15px/1.7 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
         background: #f7f5f0; color: #1c1c1a; }
  main { max-width: 680px; margin: 0 auto; }
  h1 { font-size: 22px; margin: 0 0 8px; }
  p { color: #6d6a63; margin: 0 0 20px; }
  pre { padding: 18px; border-radius: 12px; background: #fff; border: 1px solid #e4dfd4;
        overflow: auto; font: 13px/1.65 ui-monospace, SFMono-Regular, Menlo, monospace; }
  button { display: inline-flex; align-items: center; height: 34px; padding: 0 14px;
           border: 1px solid #d8d2c4; border-radius: 999px; background: #fff; color: #1c1c1a;
           font: inherit; font-size: 13px; cursor: pointer; }
  button:hover { background: #f0ece2; }
  .meta { margin-top: 18px; font-size: 12px; color: #8a857a; }
</style>
</head>
<body>
<main>
  <h1>保存链接</h1>
  <p>把这个链接发给 agent。它打开后看到 token 和保存方式，直接把内容 POST 过来即可。</p>
  <pre id="prompt">{{.Prompt}}</pre>
  <button onclick="navigator.clipboard.writeText(document.getElementById('prompt').innerText)">复制说明</button>
  <p class="meta">创建于 {{.CreatedAt}}{{if .LastUsed}} · 上次使用 {{.LastUsed}}{{end}}</p>
</main>
</body>
</html>
`

var saveLinkTemplate = template.Must(template.New("save-link").Parse(saveLinkHTML))
