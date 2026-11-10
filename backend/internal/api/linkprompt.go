package api

import (
	"fmt"
	"net/url"
	"strings"

	"everythingabout.me/backend/internal/auth"
)

// linkFacts is everything the /s/<code> page knows about the connection it describes.
// The values come from the live token row and the storage connection, so the text never
// advertises a grant or a transfer class this connection does not have.
//
// The prompt renders one grant set and one transfer class instead of every branch of the
// matrix. The reason: a reader that must first decide which half of the instructions
// applies is a reader that will follow the wrong half. A connection has exactly one class
// and one policy, and the page knows both.
type linkFacts struct {
	Base      string
	Token     string
	Class     string // "A" (presigned-parts) or "C" (relay)
	Access    []string
	Scope     string // "/" or "/Inbox", without a trailing slash
	Expires   string
	Read      bool
	Write     bool
	Events    bool
	Changes   bool
	Subscribe bool
	// Connect is a minted app/token rather than a save link. Only a Connect token can
	// name itself with PATCH /me; a save link has no api_tokens row to rename.
	Connect bool
}

func (f linkFacts) render() string {
	var b strings.Builder
	f.writeHeader(&b)
	f.writeStart(&b)
	if f.Read {
		f.writeRead(&b)
	}
	if f.Write {
		f.writeWrite(&b)
	}
	if f.Events {
		f.writeEvents(&b)
	}
	if f.Changes {
		f.writeChanges(&b)
	}
	if f.Subscribe {
		f.writeSubscribe(&b)
	}
	f.writeRules(&b)
	if f.Connect {
		f.writeName(&b)
	}
	return b.String()
}

func (f linkFacts) writeHeader(b *strings.Builder) {
	b.WriteString("# Everything About Me\n\n")
	if f.Connect {
		b.WriteString("You are connected to a personal drive through the Everything About Me API.\n\n")
	} else {
		b.WriteString("This link opens a personal drive through the Everything About Me API.\n\n")
	}
	fmt.Fprintf(b, "API:      %s\n", f.Base)
	fmt.Fprintf(b, "Token:    %s\n", f.Token)
	fmt.Fprintf(b, "Granted:  %s\n", accessLabel(f.Access))
	fmt.Fprintf(b, "Scope:    %s\n", scopeLabel(f.Scope))
	fmt.Fprintf(b, "Expires:  %s\n\n", f.Expires)
	if f.Class == "A" && f.Write {
		b.WriteString("Send `Authorization: Bearer` with this token on application requests. A signed\n")
		b.WriteString("class A part PUT is the exception: it carries only the headers returned with the\n")
		b.WriteString("part URL. Build application URLs from the API origin above; transfer URLs come\n")
		b.WriteString("only from the API's grants. The token is a secret: do not write it into logs,\n")
		b.WriteString("commits or messages to third parties.\n\n")
	} else {
		b.WriteString("Send `Authorization: Bearer` with this token on every request, and build every\n")
		b.WriteString("URL from the API origin above. The token is a secret: do not write it into\n")
		b.WriteString("logs, commits or messages to third parties.\n\n")
	}
}

// writeStart asks the two read-only questions that decide whether the rest of the page
// applies. Capabilities is queried at the scope root, which exists by definition; the
// destination parent of a write may not exist yet, so it cannot be the first query.
func (f linkFacts) writeStart(b *strings.Builder) {
	b.WriteString("## Start here\n\n")
	switch {
	case f.Read:
		fmt.Fprintf(b, "    DRIVE='%s'\n", f.Base)
		fmt.Fprintf(b, "    TOKEN='%s'\n\n", f.Token)
		b.WriteString("    curl -sS -H \"Authorization: Bearer $TOKEN\" \"$DRIVE/api/files/me\"\n")
		fmt.Fprintf(b, "    curl -sS -H \"Authorization: Bearer $TOKEN\" \"$DRIVE/api/files/capabilities?path=%s\"\n\n", url.QueryEscape(f.Scope))
		b.WriteString("`/api/files/me` returns the effective `read` and `write` for this token.\n")
		b.WriteString("`/api/files/capabilities` returns this connection's `transferClass`, `uploads`,\n")
		b.WriteString("`maxFileBytes`, `maxPartBytes` and `maxSinglePutBytes`.\n\n")
		if f.Write {
			b.WriteString("If `write` is false, stop and tell the owner: the deployment has not admitted\n")
			b.WriteString("writes yet. A refused write is not a reason to retry with another credential.\n\n")
		}
	case f.Write:
		b.WriteString("This connection is write-only. File introspection returns 403, so create an upload\n")
		b.WriteString("session with the exact file size and honor its admission result and part geometry.\n\n")
	default:
		b.WriteString("File reads and writes are not granted to this connection; only the sections below\n")
		b.WriteString("apply.\n\n")
	}
}

func (f linkFacts) writeRead(b *strings.Builder) {
	b.WriteString("## Read\n\n")
	b.WriteString("List a folder (folders first; `nextCursor` is the opaque next page):\n\n")
	fmt.Fprintf(b, "    curl -sS -H \"Authorization: Bearer $TOKEN\" \"$DRIVE/api/files/list?path=%s&limit=200\"\n\n", url.QueryEscape(f.Scope))
	b.WriteString("Read one file's metadata, including `contentRevision`:\n\n")
	fmt.Fprintf(b, "    curl -sS -H \"Authorization: Bearer $TOKEN\" \"$DRIVE/api/files?path=%s\"\n\n", url.QueryEscape(f.examplePath()))

	if f.Class == "A" {
		b.WriteString("Download through a short-lived pre-signed URL. This connection never streams bytes\n")
		b.WriteString("from `GET /api/files/content`; that route returns 409 `grant_required` here.\n\n")
	} else {
		b.WriteString("Download bytes directly from this origin:\n\n")
		fmt.Fprintf(b, "    curl -sS -H \"Authorization: Bearer $TOKEN\" \"$DRIVE/api/files/content?path=%s\"\n\n", url.QueryEscape(f.examplePath()))
		b.WriteString("For a short-lived, shareable URL, use `POST /api/files/downloads` as below.\n\n")
	}
	b.WriteString("    curl -sS -X POST -H \"Authorization: Bearer $TOKEN\" \\\n")
	b.WriteString("         -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(b, "         -d '{\"items\":[{\"path\":%q}]}' \"$DRIVE/api/files/downloads\"\n\n", f.examplePath())
	if f.Class == "A" {
		b.WriteString("Fetch `items[].url` with exactly `items[].headers` and nothing else: the grant URL\n")
		b.WriteString("is already authorized, so do not attach the API token. Do this before\n")
		b.WriteString("`items[].expiresAt`; if it expires, request another and resume from the bytes you\n")
		b.WriteString("already have.\n\n")
	} else {
		b.WriteString("Fetch `items[].url` from `$DRIVE` with `Authorization: Bearer $TOKEN`, before\n")
		b.WriteString("`items[].expiresAt`. If it expires, request another and resume from the bytes you\n")
		b.WriteString("already have.\n\n")
	}
}

func (f linkFacts) writeWrite(b *strings.Builder) {
	b.WriteString("## Write\n\n")
	switch {
	case f.Class == "A":
		b.WriteString("This connection is transfer class A (`presigned-parts`): every write is a resumable\n")
		b.WriteString("upload session, including a short text file. `PUT /api/files/content` returns\n")
		b.WriteString("409 `session_required` here.\n\n")
	case f.Read:
		b.WriteString("This connection is transfer class C (`relay`): bytes pass through this origin. A\n")
		b.WriteString("single `PUT /api/files/content` is available when capabilities reports\n")
		b.WriteString("`maxSinglePutBytes > 0`; larger files use a resumable session.\n\n")
	default:
		b.WriteString("This connection is transfer class C (`relay`) and write-only: use a resumable\n")
		b.WriteString("session for every file. Without file read there is no capabilities endpoint to\n")
		b.WriteString("read a single-PUT limit from.\n\n")
	}

	b.WriteString("The destination parent must exist unless you pass `\"parents\":true`, which creates\n")
	b.WriteString("missing ancestor folders in the same request.\n\n")
	b.WriteString("Create the session with the exact size:\n\n")
	b.WriteString("    POST $DRIVE/api/files/uploads\n")
	b.WriteString("    Authorization: Bearer $TOKEN\n")
	b.WriteString("    Content-Type: application/json\n")
	b.WriteString("    Idempotency-Key: <unique-key>\n")
	fmt.Fprintf(b, "    {\"path\":%q,\"size\":12,\"mediaType\":\"text/markdown\",\"parents\":true}\n\n", f.examplePath())
	b.WriteString("The response carries `id`, `partSize`, `partCount` and `transport.parts`. Split the\n")
	b.WriteString("file at exactly `partSize`; the final part keeps the exact remainder. A zero-byte\n")
	b.WriteString("file has no parts and can be committed immediately.\n\n")

	if f.Class == "A" {
		b.WriteString("PUT each part to its own URL with exactly its headers. The URL binds the method,\n")
		b.WriteString("object, part number and exact length, and no digest. Do not add application\n")
		b.WriteString("`Authorization`, CSRF or cookie headers, and never change the method, host, path\n")
		b.WriteString("or signed query.\n\n")
		b.WriteString("    PUT <transport.parts[n].url>    (headers: exactly <transport.parts[n].headers>)\n\n")
		b.WriteString("Renew an expired or 403 part URL (the part keeps its bound length):\n\n")
		b.WriteString("    POST $DRIVE/api/files/uploads/<id>/parts/sign\n")
		b.WriteString("    {\"numbers\":[1,2,3]}\n\n")
		b.WriteString("After an uncertain part request, resend the same part or read the provider's listing\n")
		b.WriteString("with `GET $DRIVE/api/files/uploads/<id>/parts`. Do not resend after commit has started.\n\n")
	} else {
		b.WriteString("PUT each part with `Authorization` and `Content-Length` to\n\n")
		b.WriteString("    $DRIVE/api/files/uploads/<id>/parts/<number>\n\n")
		b.WriteString("The service computes SHA-256, returns a durable receipt once the bytes are safe, and\n")
		b.WriteString("an identical replay returns the same receipt. Parts start at 1; a zero-byte file has\n")
		b.WriteString("no parts.\n\n")
	}

	b.WriteString("When every part is in place:\n\n")
	b.WriteString("    POST $DRIVE/api/files/uploads/<id>/commit\n\n")
	b.WriteString("`202` means the commit is still settling: poll the `Location`\n")
	b.WriteString("(`$DRIVE/api/files/operations/<operationId>`) until `state` is `succeeded`. `failed`,\n")
	b.WriteString("`cancelled` and `outcome_unknown` are not success; settle an `outcome_unknown`\n")
	b.WriteString("session through the same upload id, never by starting over.\n\n")

	if f.Class == "C" && f.Read {
		b.WriteString("A short create may use one request instead of a session, when capabilities reports\n")
		b.WriteString("`maxSinglePutBytes > 0`:\n\n")
		fmt.Fprintf(b, "    PUT $DRIVE/api/files/content?path=%s\n", url.QueryEscape(f.examplePath()))
		b.WriteString("    Authorization: Bearer $TOKEN\n")
		b.WriteString("    Content-Type: text/markdown\n")
		b.WriteString("    If-None-Match: *\n")
		b.WriteString("    Idempotency-Key: <unique-key>\n")
		b.WriteString("    Content-Length: 12\n\n")
		b.WriteString("    hello world\n\n")
		b.WriteString("`maxSinglePutBytes` bounds this request; above it, use a session.\n\n")
	}

	b.WriteString("Create is the default. To replace an existing file, read its `contentRevision` with\n")
	b.WriteString("`GET $DRIVE/api/files?path=<path>` and send\n")
	if f.Class == "C" && f.Read {
		b.WriteString("`\"condition\":{\"mode\":\"replace\",\"contentRevision\":\"...\"}` in the session, or\n")
		b.WriteString("`If-Match: \"<contentRevision>\"` on a single PUT. A `409` or `412` conflict is final;\n")
		b.WriteString("never overwrite blindly.\n\n")
	} else {
		b.WriteString("`\"condition\":{\"mode\":\"replace\",\"contentRevision\":\"...\"}` in the session. A\n")
		b.WriteString("`409` or `412` conflict is final; never overwrite blindly.\n\n")
	}
}

func (f linkFacts) writeEvents(b *strings.Builder) {
	b.WriteString("## Events\n\n")
	b.WriteString("`POST /events` writes one timeline card. `starts_at` and `external_id` are required;\n")
	b.WriteString("the same `external_id` again replaces the card and keeps its id.\n\n")
	b.WriteString("    POST $DRIVE/events\n")
	b.WriteString("    Authorization: Bearer $TOKEN\n")
	b.WriteString("    Content-Type: application/json\n")
	b.WriteString("    {\"external_id\":\"note-1\",\"starts_at\":\"2026-11-10T10:00:00Z\",\n")
	b.WriteString("     \"tz\":\"Asia/Shanghai\",\"type\":\"note\",\"title\":\"...\",\n")
	b.WriteString("     \"payload\":{\"body\":\"...\"}}\n\n")
	b.WriteString("`type` is `photo` (1-4 `payload.media` drive-relative paths inside your scope),\n")
	b.WriteString("`note` (`payload.body`), `metric` (1-3 `payload.metrics`) or `html` (`payload.html`\n")
	b.WriteString("plus `height`). A card the user deleted returns `410`; stop retrying it.\n\n")
}

func (f linkFacts) writeChanges(b *strings.Builder) {
	b.WriteString("## Change history\n\n")
	b.WriteString("Start from a bounded snapshot, then pull the feed:\n\n")
	b.WriteString("    POST $DRIVE/api/files/change-snapshots\n")
	b.WriteString("    Content-Type: application/json\n")
	fmt.Fprintf(b, "    {\"target\":{\"path\":%q},\"kind\":\"folder\",\"recursive\":true}\n\n", f.Scope)
	b.WriteString("The response carries `entriesUrl` (immutable metadata pages) and `changesCursor`.\n\n")
	b.WriteString("    GET $DRIVE/api/files/changes?cursor=<changesCursor>&limit=200\n\n")
	b.WriteString("Drain until `hasMore` is false, and persist `nextCursor` with each page's effects.\n")
	b.WriteString("`409 baseline_required` means take a new snapshot; `410 cursor_expired` means the\n")
	b.WriteString("cursor is gone and a new baseline is required.\n\n")
}

func (f linkFacts) writeSubscribe(b *strings.Builder) {
	b.WriteString("## Live changes\n\n")
	b.WriteString("    POST $DRIVE/api/files/subscriptions\n")
	b.WriteString("    Content-Type: application/json\n")
	fmt.Fprintf(b, "    {\"target\":{\"path\":%q},\"kind\":\"folder\",\"recursive\":true,\"delivery\":{\"type\":\"sse\"}}\n\n", f.Scope)
	b.WriteString("    GET $DRIVE/api/files/subscriptions/<id>/events\n\n")
	b.WriteString("SSE is an availability signal, not an acknowledgement: on every notification and on\n")
	b.WriteString("reconnect, pull `GET $DRIVE/api/files/changes` from your saved cursor.\n\n")
}

func (f linkFacts) writeRules(b *strings.Builder) {
	b.WriteString("## Rules\n\n")
	if f.Write {
		b.WriteString("- `409` with `name_conflict`, `target_changed` or `part_conflict` means the\n")
		b.WriteString("  destination changed. Read the current state and decide; do not blindly retry.\n")
		b.WriteString("- `412` is a failed precondition: never replace without the `contentRevision` you\n")
		b.WriteString("  observed.\n")
	}
	if f.Write {
		b.WriteString("- `409 baseline_required` means this deployment has not admitted storage yet; report\n")
		b.WriteString("  it to the owner and stop. Retrying cannot fix it.\n")
	}
	b.WriteString("- `429` carries `Retry-After`; wait that long, then retry with jitter.\n")
	b.WriteString("- `401` means this link was revoked or expired; ask the owner for a new one.\n")
	b.WriteString("- Errors are `application/problem+json` with `code`, `detail` and `requestId`.\n\n")
}

func (f linkFacts) writeName(b *strings.Builder) {
	b.WriteString("## Optional\n\n")
	b.WriteString("To give this connection a name on the owner's Connect page:\n\n")
	b.WriteString("    PATCH $DRIVE/me\n")
	b.WriteString("    Authorization: Bearer $TOKEN\n")
	b.WriteString("    Content-Type: application/json\n")
	b.WriteString("    {\"name\":\"My app\"}\n")
}

// tokenPrompt is the full instructions behind a Connect token's one-line prompt. It is
// generated from the live row, so grants, scope and expiry cannot go stale.
func tokenPrompt(base, plain string, token *auth.Token, class string) string {
	scope := "/"
	if token.Prefix != "" {
		scope = "/" + token.Prefix
	}
	expires := "never"
	if token.ExpiresAt != nil {
		expires = *token.ExpiresAt
	}
	access := make([]string, 0, 5)
	if token.CanRead {
		access = append(access, "read")
	}
	if token.CanWrite {
		access = append(access, "write")
	}
	if token.CanEvents {
		access = append(access, "events")
	}
	if token.CanChanges {
		access = append(access, "changes")
	}
	if token.CanSubscribe {
		access = append(access, "subscribe")
	}
	return linkFacts{
		Base:      base,
		Token:     plain,
		Class:     promptClass(class),
		Access:    access,
		Scope:     scope,
		Expires:   expires,
		Read:      token.CanRead,
		Write:     token.CanWrite,
		Events:    token.CanEvents,
		Changes:   token.CanChanges,
		Subscribe: token.CanSubscribe,
		Connect:   true,
	}.render()
}

// saveLinkPrompt explains the same unified API to a full-access save link. A save link's
// principal carries CanEvents, but POST /events resolves the credential in api_tokens and
// a save link has no row there, so the grant cannot work today; do not advertise it.
func saveLinkPrompt(base, code, class string) string {
	return linkFacts{
		Base:    base,
		Token:   code,
		Class:   promptClass(class),
		Access:  []string{"read", "write"},
		Scope:   "/",
		Expires: "never",
		Read:    true,
		Write:   true,
		Connect: false,
	}.render()
}

// promptClass is the class label the page renders. Class B (resumable-session) has no
// driver and validateTransferClass rejects it, so only A and C can reach a running
// server; anything else falls back to C rather than advertising A's unsigned-part flow.
func promptClass(class string) string {
	if class == "A" {
		return "A"
	}
	return "C"
}

func accessLabel(parts []string) string {
	if len(parts) == 0 {
		return "no access"
	}
	return strings.Join(parts, " + ")
}

func scopeLabel(scope string) string {
	if scope == "/" {
		return "/ (the whole drive)"
	}
	return scope
}

// examplePath is the file the command examples use. It sits at the scope root so a scoped
// token never sees an example outside its own scope.
func (f linkFacts) examplePath() string {
	if f.Scope == "/" {
		return "/note.md"
	}
	return f.Scope + "/note.md"
}
