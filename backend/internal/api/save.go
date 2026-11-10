package api

import (
	"html/template"
	"io"
	"net/http"
	"strings"

	"everythingabout.me/backend/internal/auth"
	"everythingabout.me/backend/internal/httpx"
)

// A /s/<code> link is instructions for an agent: it returns the token plus a prompt and
// accepts no content itself; saving uses unified content or durable upload sessions.
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
	prompt := saveLinkPrompt(base, code, s.filesTransferClassLabel())

	format := r.URL.Query().Get("format")
	accept := strings.ToLower(r.Header.Get("Accept"))
	switch {
	case format == "json" || strings.Contains(accept, "application/json"):
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"token":      code,
			"url":        linkURL,
			"api":        base,
			"files":      base + "/api/files",
			"upload":     base + "/api/files/uploads",
			"prompt":     prompt,
			"name":       p.Token.Name,
			"createdAt":  p.Token.CreatedAt,
			"lastUsedAt": p.Token.LastUsedAt,
		})
		return nil
	case format == "html" || strings.Contains(accept, "text/html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		copy := linkPageCopyFor(linkPageLang(r), true)
		return linkTemplate.Execute(w, linkPageData(copy, prompt, p.Token.CreatedAt, p.Token.LastUsedAt))
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
	prompt := tokenPrompt(base, plain, token, s.filesTransferClassLabel())

	format := r.URL.Query().Get("format")
	accept := strings.ToLower(r.Header.Get("Accept"))
	switch {
	case format == "json" || strings.Contains(accept, "application/json"):
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"token":         plain,
			"api":           base,
			"prompt":        prompt,
			"name":          token.Name,
			"prefix":        token.Prefix,
			"read":          token.CanRead,
			"write":         token.CanWrite,
			"events":        token.CanEvents,
			"changes":       token.CanChanges,
			"subscribe":     token.CanSubscribe,
			"policyVersion": token.PolicyVersion,
			"expiresAt":     token.ExpiresAt,
			"createdAt":     token.CreatedAt,
			"lastUsedAt":    token.LastUsedAt,
		})
		return nil
	case format == "html" || strings.Contains(accept, "text/html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		copy := linkPageCopyFor(linkPageLang(r), false)
		return linkTemplate.Execute(w, linkPageData(copy, prompt, token.CreatedAt, token.LastUsedAt))
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

// linkPageCopy is the chrome of the /s/ page. The prompt itself stays English because it
// is an API artifact read by an agent; the chrome follows the browser like the rest of
// the app. The page is server-rendered, so it cannot import the React i18n module; both
// languages live here, side by side, rather than one hard-coded language.
type linkPageCopy struct {
	Lang     string
	Title    string
	Hint     string
	Copy     string
	Copied   string
	Created  string
	LastUsed string
}

// linkPageLang picks the chrome language from the first language the browser asks for.
// A missing header gets English, the same default the prompt uses.
func linkPageLang(r *http.Request) string {
	for _, item := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag := strings.ToLower(strings.TrimSpace(strings.SplitN(item, ";", 2)[0]))
		if tag == "" {
			continue
		}
		if strings.HasPrefix(tag, "zh") {
			return "zh"
		}
		return "en"
	}
	return "en"
}

func linkPageCopyFor(lang string, saveLink bool) linkPageCopy {
	if lang == "zh" {
		if saveLink {
			return linkPageCopy{
				Lang: "zh", Title: "保存链接",
				Hint: "把这个链接发给 agent。它打开后拿到 token 和写入方式；这个页面本身不接收内容。",
				Copy: "复制说明", Copied: "已复制", Created: "创建于", LastUsed: "上次使用",
			}
		}
		return linkPageCopy{
			Lang: "zh", Title: "连接说明",
			Hint: "把这个链接发给 agent。它打开后拿到 token、范围和权限，再按说明调用 API。",
			Copy: "复制说明", Copied: "已复制", Created: "创建于", LastUsed: "上次使用",
		}
	}
	if saveLink {
		return linkPageCopy{
			Lang: "en", Title: "Save link",
			Hint: "Send this link to an agent. It opens to the token and write instructions; this page accepts no content itself.",
			Copy: "Copy instructions", Copied: "Copied", Created: "Created", LastUsed: "Last used",
		}
	}
	return linkPageCopy{
		Lang: "en", Title: "Connection instructions",
		Hint: "Send this link to an agent. It opens to the token, scope and permissions, then follows the instructions.",
		Copy: "Copy instructions", Copied: "Copied", Created: "Created", LastUsed: "Last used",
	}
}

func linkPageData(copy linkPageCopy, prompt, createdAt string, lastUsed *string) map[string]any {
	return map[string]any{
		"Lang":          copy.Lang,
		"Title":         copy.Title,
		"Hint":          copy.Hint,
		"Prompt":        prompt,
		"CreatedAt":     createdAt,
		"LastUsed":      textValue(lastUsed),
		"CopyLabel":     copy.Copy,
		"CopiedLabel":   copy.Copied,
		"CreatedLabel":  copy.Created,
		"LastUsedLabel": copy.LastUsed,
	}
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
<html lang="{{.Lang}}">
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
        font: 13px/1.7 ui-monospace, SFMono-Regular, Menlo, monospace; }
  button { display: inline-flex; align-items: center; height: 34px; padding: 0 14px;
           border: 1px solid #d8d2c4; border-radius: 999px; background: #fff; color: #1c1c1a;
           font: inherit; font-size: 13px; cursor: pointer; }
  button:hover { background: #f0ece2; }
  .meta { margin-top: 18px; font-size: 12px; color: #8a857a; }
  time { white-space: nowrap; }
</style>
</head>
<body>
<main>
  <h1>{{.Title}}</h1>
  <p>{{.Hint}}</p>
  <pre id="prompt">{{.Prompt}}</pre>
  <button id="copy" type="button" data-copied="{{.CopiedLabel}}">{{.CopyLabel}}</button>
  <p class="meta">{{.CreatedLabel}} <time datetime="{{.CreatedAt}}">{{.CreatedAt}}</time>{{if .LastUsed}} · {{.LastUsedLabel}} <time datetime="{{.LastUsed}}">{{.LastUsed}}</time>{{end}}</p>
</main>
<script>
  for (const el of document.querySelectorAll("time[datetime]")) {
    const at = new Date(el.dateTime);
    if (!Number.isNaN(at.getTime())) el.textContent = at.toLocaleString();
  }
  const copy = document.getElementById("copy");
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(document.getElementById("prompt").textContent);
      const original = copy.textContent;
      copy.textContent = copy.dataset.copied;
      setTimeout(() => { copy.textContent = original; }, 1600);
    } catch {}
  });
</script>
</body>
</html>
`

var linkTemplate = template.Must(template.New("link").Parse(linkHTML))
