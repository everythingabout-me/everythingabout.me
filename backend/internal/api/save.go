package api

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
)

// A /s/<code> link is instructions for an agent: it returns the token plus a prompt and
// accepts no content itself; saving goes through the file API (PUT /api/files, POST /upload).
// The code is either a save link or an eam_ token: the Connect handoff links here with the
// token secret, so one page can explain a scoped connection and a full-access save link.

// handleSaveLinkPage renders the link page: agents get the Markdown prompt by default,
// browsers get a page of instructions, and ?format=json serves apps.
func (s *Server) handleSaveLinkPage(w http.ResponseWriter, r *http.Request) error {
	code := r.PathValue("code")
	p, err := s.auth.VerifySaveLink(r.Context(), code)
	if err != nil {
		return err
	}
	if p != nil {
		return s.serveSaveLink(w, r, code, p)
	}

	token, err := s.auth.VerifyToken(r.Context(), code)
	if err != nil {
		return err
	}
	if token == nil {
		http.Error(w, "link not found", http.StatusNotFound)
		return nil
	}
	return s.serveTokenLink(w, r, code, token)
}

func (s *Server) serveSaveLink(w http.ResponseWriter, r *http.Request, code string, p *auth.Principal) error {
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
			"files":      base + "/api/files/",
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
		return linkTemplate.Execute(w, map[string]any{
			"Title":     "保存链接",
			"Hint":      "把这个链接发给 agent。它打开后拿到 token 和保存方式，再用文件 API 保存；这个页面本身不接收内容。",
			"Prompt":    prompt,
			"LastUsed":  textValue(p.Token.LastUsedAt),
			"CreatedAt": p.Token.CreatedAt,
		})
	default:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, err := io.WriteString(w, prompt)
		return err
	}
}

// serveTokenLink is the connection's own instruction page. It reads the live row, so the
// prompt always shows the current scope, permissions and expiry.
func (s *Server) serveTokenLink(w http.ResponseWriter, r *http.Request, plain string, token *auth.Token) error {
	base := requestBaseURL(r)
	prompt := tokenPrompt(base, plain, token)

	format := r.URL.Query().Get("format")
	accept := strings.ToLower(r.Header.Get("Accept"))
	switch {
	case format == "json" || strings.Contains(accept, "application/json"):
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"token":      plain,
			"api":        base,
			"prompt":     prompt,
			"name":       token.Name,
			"prefix":     token.Prefix,
			"read":       token.CanRead,
			"write":      token.CanWrite,
			"events":     token.CanEvents,
			"expiresAt":  token.ExpiresAt,
			"createdAt":  token.CreatedAt,
			"lastUsedAt": token.LastUsedAt,
		})
		return nil
	case format == "html" || strings.Contains(accept, "text/html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		return linkTemplate.Execute(w, map[string]any{
			"Title":     "连接说明",
			"Hint":      "把这个链接发给 agent。它打开后拿到 token、范围和权限，再按说明调用 API。",
			"Prompt":    prompt,
			"LastUsed":  textValue(token.LastUsedAt),
			"CreatedAt": token.CreatedAt,
		})
	default:
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, err := io.WriteString(w, prompt)
		return err
	}
}

func textValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// tokenPrompt is the full instructions behind the one-line Connect prompt. It is English
// like every API artifact, and it is generated from the row so it cannot go stale.
func tokenPrompt(base, plain string, token *auth.Token) string {
	parts := make([]string, 0, 3)
	if token.CanRead {
		parts = append(parts, "read")
	}
	if token.CanWrite {
		parts = append(parts, "write")
	}
	if token.CanEvents {
		parts = append(parts, "events")
	}
	access := strings.Join(parts, " + ")
	if access == "" {
		access = "no access"
	}

	scope := "the whole drive"
	listPath := "/list"
	writePath := "Inbox/note.md"
	if token.Prefix != "" {
		scope = token.Prefix + "/"
		listPath = "/list?path=" + url.QueryEscape(token.Prefix)
		writePath = token.Prefix + "/note.md"
	}
	expires := "never"
	if token.ExpiresAt != nil {
		expires = *token.ExpiresAt
	}

	return fmt.Sprintf(`# everythingabout.me

You can access my drive through the Everything About Me API.

API:     %s
Token:   %s
Access:  %s
Scope:   %s
Expires: %s

Use it as a Bearer token:
  curl -H "Authorization: Bearer %s" \
       "%s%s"

  curl -X PUT -H "Authorization: Bearer %s" \
       --data-binary @note.md "%s/api/files/%s"

First call GET /me to confirm the token and its scope.
`, base, plain, access, scope, expires, plain, base, listPath, plain, base, writePath)
}

// saveLinkPrompt only explains how to call the API with this token; it defines no new write path.
func saveLinkPrompt(base, code string) string {
	return fmt.Sprintf(`# everythingabout.me

Connected to this drive.

Token: %s
API: %s
Authorization: Bearer %s

Save a short text (one request):
  PUT %s/api/files/Inbox/2026/10/28/note.md
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

const linkHTML = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Everything About Me</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; padding: 48px 24px; font: 15px/1.7 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
         background: #f7f5f0; color: #1c1c1a; }
  main { max-width: 680px; margin: 0 auto; }
  h1 { font-size: 22px; margin: 0 0 8px; }
  p { color: #6d6a63; margin: 0 0 20px; }
  pre { padding: 18px; border-radius: 12px; background: #fff; border: 1px solid #e4dfd4;
        overflow: auto; white-space: pre-wrap; word-break: break-word;
        font: 13px/1.65 ui-monospace, SFMono-Regular, Menlo, monospace; }
  button { display: inline-flex; align-items: center; height: 34px; padding: 0 14px;
           border: 1px solid #d8d2c4; border-radius: 999px; background: #fff; color: #1c1c1a;
           font: inherit; font-size: 13px; cursor: pointer; }
  button:hover { background: #f0ece2; }
  .meta { margin-top: 18px; font-size: 12px; color: #8a857a; }
</style>
</head>
<body>
<main>
  <h1>{{.Title}}</h1>
  <p>{{.Hint}}</p>
  <pre id="prompt">{{.Prompt}}</pre>
  <button onclick="navigator.clipboard.writeText(document.getElementById('prompt').textContent)">复制说明</button>
  <p class="meta">创建于 {{.CreatedAt}}{{if .LastUsed}} · 上次使用 {{.LastUsed}}{{end}}</p>
</main>
</body>
</html>
`

var linkTemplate = template.Must(template.New("link").Parse(linkHTML))
