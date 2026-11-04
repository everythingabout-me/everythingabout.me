package api

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
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
	prompt := saveLinkPrompt(base, code)

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
	parts := make([]string, 0, 5)
	if token.CanRead {
		parts = append(parts, "read")
	}
	if token.CanWrite {
		parts = append(parts, "write")
	}
	if token.CanEvents {
		parts = append(parts, "events")
	}
	if token.CanChanges {
		parts = append(parts, "changes")
	}
	if token.CanSubscribe {
		parts = append(parts, "subscribe")
	}
	access := strings.Join(parts, " + ")
	if access == "" {
		access = "no access"
	}

	scope := "the whole drive"
	root := "/"
	writePath := "/note.md"
	if token.Prefix != "" {
		scope = "/" + token.Prefix
		root = scope
		writePath = scope + "/note.md"
	}
	expires := "never"
	if token.ExpiresAt != nil {
		expires = *token.ExpiresAt
	}

	prompt := fmt.Sprintf(`# everythingabout.me

You can access my drive through the Everything About Me API.

API:     %s
Token:   %s
Access:  %s
Scope:   %s
Expires: %s

Use Authorization: Bearer %s on application control requests and embedded uploads.
Signed transfer requests use only their returned scoped signature and headers, as described below.
First call GET /me to confirm the connection and its current permissions.
PATCH /me manages the Connect identity; POST /events authors events.
Use only the configured API origin for metadata and bytes. Never use provider credentials or URLs.
`, base, plain, access, scope, expires, plain)
	if token.CanRead {
		prompt += fmt.Sprintf(`
GET /api/files/me confirms file access and scope.
List the authorized folder:
  curl -H "Authorization: Bearer %s" "%s/api/files/list?path=%s"
Read a file with GET /api/files/content?path=<URL-encoded absolute path>.
`, plain, base, url.QueryEscape(root))
	}
	if token.CanWrite {
		prompt += unifiedWritePrompt(base, plain, writePath)
	}
	if token.CanChanges {
		prompt += "\nFile change history is explicitly granted. Consult GET /api/files/capabilities for the current change feed contract.\n"
	}
	if token.CanSubscribe {
		prompt += "Live file change subscriptions are explicitly granted within the same file scope.\n"
	}
	return prompt
}

// saveLinkPrompt explains only unified API requests; the link itself is read-only.
func saveLinkPrompt(base, code string) string {
	return fmt.Sprintf(`# everythingabout.me

Connected to this drive.

Token: %s
API: %s
Authorization: Bearer %s

First call GET /api/files/me and GET /api/files/capabilities?path=%%2F.
Use only this API origin for metadata, content, numbered parts and Operations.
`, code, base, code) + unifiedWritePrompt(base, code, "/note.md")
}

func unifiedWritePrompt(base, token, target string) string {
	return fmt.Sprintf(`
The destination parent must exist. Create folders explicitly when needed:
  POST /api/files
  Content-Type: application/json
  Idempotency-Key: <unique-folder-key>
  {"kind":"folder","path":"<absolute parent path>","parents":true}

If file reads are permitted, after the parent exists check GET /api/files/capabilities?path=%s for admitted write and size limits.
For a write-only connection, 403 from file introspection means those limits are unavailable;
create a session with the exact file size and honor its admission result and returned part geometry.
A rejected admission grants no upload authority. Do not retry with guessed read permissions.

Small content PUT is optional: use the following example only after capabilities reports
maxSinglePutBytes > 0 and the content fits that limit. EdgeOne signed deployments report
maxSinglePutBytes=0; use the session flow below for every file, including short text.
Write-only connections with unavailable capabilities should use sessions as well.
Save a short text when content PUT is enabled (create only):
  curl -X PUT -H "Authorization: Bearer %s" \
       -H "Content-Type: text/markdown" -H "If-None-Match: *" \
       -H "Idempotency-Key: <unique-write-key>" \
       --data-binary @note.md "%s/api/files/content?path=%s"

For resumable files within the advertised limits:
  POST %s/api/files/uploads
  Authorization: Bearer %s
  Content-Type: application/json
  Idempotency-Key: <unique-session-key>
  {"path":%q,"size":12345,"mediaType":"image/jpeg","condition":{"mode":"create"}}

Use the returned id, partSize, partCount, maxParallelism and transport. Split exactly at partSize;
the final part has the exact remainder. For transport=embedded, PUT each part with Content-Length to:
  /api/files/uploads/{id}/parts/{number}
For transport=cos-signed, compute the part's lowercase hexadecimal SHA-256 and base64 MD5, then POST:
  /api/files/uploads/{id}/parts/{number}/authorize
  {"size":<exact bytes>,"sha256":"<hex>","md5":"<base64>"}
Use the returned method=PUT, origin-relative /api/files/transfer/ URL and exact Content-MD5/Content-Type.
Do not add application Authorization, CSRF or cookie headers to this signed byte PUT; never replace its
host, path or signed query, and never follow a redirect to another origin. COS verifies this narrow signature.
POST /api/files/uploads/{id}/parts/{number}/confirm to recover an earlier native success before resending.
On 409 part_pending, send the signed part then confirm again, including after a lost PUT reply. Only the
application's durable confirm receipt counts as acknowledgement. checksumSource=client-sha256-cos-md5 means
the SHA-256 is a client assertion and COS verifies MD5, not an independently server-computed SHA-256.
Numbers start at 1; a zero-byte file has no parts. Persist the id and recover receipts with
GET /api/files/uploads/{id}/parts. Identical-byte retries are safe; changed digests conflict.
After every part is acknowledged:
  POST /api/files/uploads/{id}/commit
Commit may return 202 with Location pointing to /api/files/operations/{operationId}.
Poll that same-origin Operation until state is succeeded; failed, cancelled or
outcome_unknown is not success. Resume status with GET /api/files/uploads/{id}.
A 409 operation_in_progress may include operationId: wait for that exact Operation to
succeed, then retry admission before sending bytes; unavailable status keeps the wait.
Use Authorization: Bearer on application control requests, embedded parts, commit and Operation status.
Signed /api/files/transfer/ byte requests use only the returned COS signature/query and headers.
Choose a unique path. When file reads are permitted, to replace deliberately read its contentRevision and use
If-Match: "<contentRevision>" for PUT or condition.mode=replace plus contentRevision
for a session. A write-only connection must obtain an authorized revision separately or use create-only.
Never silently overwrite after a conflict. Writes may be disabled until
the deployment's storage configuration and baseline are explicitly admitted.
`, url.QueryEscape(path.Dir(target)), token, base, url.QueryEscape(target), base, token, target)
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
