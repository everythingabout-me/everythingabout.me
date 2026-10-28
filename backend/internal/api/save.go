package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
	"everythingabout.me/backend/internal/nodes"
	"everythingabout.me/backend/internal/paths"
)

// 保存链接是给 agent 用的入口：URL 本身就是凭证，GET 返回 token + prompt，
// POST 把正文存成一个 Markdown 文件。设计见 docs/api/api.md §4.14。

const saveLinkMaxBody = 1 << 20 // 1 MiB，纯文本足够

type saveLinkRequest struct {
	Text   string `json:"text"`
	Title  string `json:"title"`
	Source string `json:"source"`
}

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
			"save":       map[string]any{"method": "POST", "url": linkURL, "contentType": "text/markdown"},
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

// handleSaveLinkCapture 接收正文。支持原始 text/markdown 和 JSON 两种写法；
// 文件名用正文 hash，同一天重复提交不会生成两份。
func (s *Server) handleSaveLinkCapture(w http.ResponseWriter, r *http.Request) error {
	p, err := s.auth.VerifySaveLink(r.Context(), r.PathValue("code"))
	if err != nil {
		return err
	}
	if p == nil {
		return httpx.NotFound("save link not found")
	}

	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if !saveLinkAcceptsText(contentType) {
		return httpx.NewError(http.StatusUnsupportedMediaType,
			"binary files must use the direct upload flow (POST /upload with this link's Bearer token); see the link prompt")
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, saveLinkMaxBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return httpx.NewError(http.StatusRequestEntityTooLarge, "content is too large; keep it under 1MiB")
		}
		return httpx.BadRequest("failed to read request body")
	}

	var req saveLinkRequest
	if strings.HasPrefix(contentType, "application/json") {
		if err := json.Unmarshal(body, &req); err != nil {
			return httpx.BadRequest("invalid JSON body")
		}
	} else {
		req.Text = string(body)
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		return httpx.BadRequest("content cannot be empty")
	}
	if title := strings.TrimSpace(r.URL.Query().Get("title")); title != "" {
		req.Title = title
	}
	if source := strings.TrimSpace(r.URL.Query().Get("source")); source != "" {
		req.Source = source
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		title = saveLinkTitle(req.Text)
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80])
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = p.Token.Name
	}

	now := time.Now()
	if loc, err := time.LoadLocation(s.cfg.Timeline.Timezone); err == nil {
		now = now.In(loc)
	}
	hash := sha256.Sum256([]byte(req.Text))
	hashName := hex.EncodeToString(hash[:])
	slug := fileSlug(title)
	path := fmt.Sprintf("Inbox/%s/%s.md", now.Format("2006/01/02"), hashName[:12])
	if slug != "" {
		path = fmt.Sprintf("Inbox/%s/%s-%s.md", now.Format("2006/01/02"), slug, hashName[:8])
	}

	existing, err := s.nodes.Live(r.Context(), path, p.User.ID)
	if err != nil {
		return err
	}
	if existing != nil {
		s.recordAudit(r, p, "capture", path, http.StatusOK, map[string]any{
			"size": existing.Size, "title": title, "link": p.Token.Name, "duplicate": true,
		})
		writeCaptureResult(w, r, http.StatusOK, path, title, true)
		return nil
	}

	content := saveFileContent(req.Text)
	size := int64(len(content))
	if err := s.checkQuota(r.Context(), p.User.ID, size, nil); err != nil {
		return err
	}
	if err := s.assertParentDirs(r.Context(), p.User.ID, path); err != nil {
		return err
	}
	if err := s.nodes.EnsureParents(r.Context(), path, p.User.ID); err != nil {
		return err
	}

	mime := "text/markdown; charset=utf-8"
	info, err := s.storage.Put(r.Context(), paths.KeyFor(path, p.User.ID), bytes.NewReader([]byte(content)), size, mime)
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
		Mime:             &mime,
		ETag:             etag,
		StorageClass:     storageClass,
		SourceModifiedAt: modified,
		Meta: map[string]any{
			"mime": mime, "sha256": hashName, "title": title, "source": source,
			"captured_at": now.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return err
	}
	s.timeline.Notify(p.User.ID)
	s.recordAudit(r, p, "capture", path, http.StatusCreated, map[string]any{
		"size": info.Size, "title": title, "link": p.Token.Name,
	})
	writeCaptureResult(w, r, http.StatusCreated, path, title, false)
	return nil
}

func writeCaptureResult(w http.ResponseWriter, r *http.Request, status int, path, title string, duplicate bool) {
	httpx.WriteJSON(w, status, map[string]any{
		"path":      path,
		"url":       requestBaseURL(r) + "/drive?path=" + url.QueryEscape(path),
		"title":     title,
		"duplicate": duplicate,
	})
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

// saveFileContent 只保证正文以换行结尾，不添加任何 frontmatter。
func saveFileContent(text string) string {
	if strings.HasSuffix(text, "\n") {
		return text
	}
	return text + "\n"
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

// saveLinkAcceptsText 区分“文本走 /s”和“文件走直传”。
// 没有 Content-Type 的请求按文本处理，方便 curl --data-binary。
func saveLinkAcceptsText(contentType string) bool {
	return contentType == "" ||
		strings.HasPrefix(contentType, "text/") ||
		strings.HasPrefix(contentType, "application/json")
}

func saveLinkPrompt(base, code string) string {
	return fmt.Sprintf(`# everythingabout.me

Connected to this drive.

Token: %s

Text:
  POST %s/s/%s
  Content-Type: text/markdown

  <content>

  Optional query: ?title=...
  Response: {"path": "Inbox/2026/10/28/....md", "url": "..."}

Files (image, video, PDF):
  POST %s/upload
  {"path": "Inbox/2026/10/28/photo.jpg", "size": 12345, "mime": "image/jpeg"}

  PUT the bytes to each returned part URL, then:
  POST %s/upload/complete

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
