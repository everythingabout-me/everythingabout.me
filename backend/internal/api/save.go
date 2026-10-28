package api

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
)

// A save link is instructions for an agent: GET /s/<code> returns the token plus a prompt.
// It accepts no content itself; saving goes through the file API (PUT /files, POST /upload).

// handleSaveLinkPage renders the link page: agents get the Markdown prompt by default,
// browsers get a page of instructions, and ?format=json serves apps.
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
			"api":        base,
			"files":      base + "/files/",
			"upload":     base + "/upload",
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

// saveLinkPrompt only explains how to call the API with this token; it defines no new write path.
func saveLinkPrompt(base, code string) string {
	return fmt.Sprintf(`# everythingabout.me

Connected to this drive.

Token: %s
API: %s
Authorization: Bearer %s

Save a short text (one request):
  PUT %s/files/Inbox/2026/10/28/note.md
  Content-Type: text/markdown

  <content>

Save a file (image, video, PDF, any size):
  POST %s/upload
  {"path": "Inbox/2026/10/28/photo.jpg", "size": 12345, "mime": "image/jpeg"}

  PUT the bytes to each returned part URL, then:
  POST %s/upload/complete

Use a unique path; PUT does not overwrite by default.
`, code, base, code, base, base, base)
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

// ---------- web: the account page's link list ----------

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
  <p>把这个链接发给 agent。它打开后拿到 token 和保存方式，再用文件 API 保存；这个页面本身不接收内容。</p>
  <pre id="prompt">{{.Prompt}}</pre>
  <button onclick="navigator.clipboard.writeText(document.getElementById('prompt').innerText)">复制说明</button>
  <p class="meta">创建于 {{.CreatedAt}}{{if .LastUsed}} · 上次使用 {{.LastUsed}}{{end}}</p>
</main>
</body>
</html>
`

var saveLinkTemplate = template.Must(template.New("save-link").Parse(saveLinkHTML))
