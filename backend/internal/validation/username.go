package validation

import (
	"regexp"
	"strings"

	"everythingabout.me/backend/internal/httpx"
)

// A handle is the first path segment of a drive address (/<handle>/Photos/a.jpg), so its
// alphabet is the one that survives a URL, a shell argument and a human retyping it:
// lowercase ASCII letters, digits and single hyphens, never at either end. Uppercase is
// folded instead of refused so typing your own name with a capital does not fail.
var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,28}[a-z0-9]$`)

// reservedUsernames are names the service needs for its own routes. Claiming one would
// give an account an address that a static route outranks, so its files would be
// unreachable at the address the account believes it owns. The list is deliberately wider
// than today's routes: adding a route later must not require taking a handle back from its
// owner.
var reservedUsernames = map[string]bool{
	"about": true, "account": true, "admin": true, "api": true, "app": true,
	"assets": true, "audit": true, "auth": true, "blog": true, "connect": true,
	"copy": true, "delete": true, "docs": true, "download": true, "drive": true,
	"eam": true, "edit": true, "events": true, "f": true, "favicon.ico": true,
	"file": true, "files": true, "health": true, "healthz": true, "help": true,
	"login": true, "logo.svg": true, "logout": true, "mail": true,
	"manifest.webmanifest": true, "me": true, "move": true, "new": true,
	"oauth": true, "open": true, "preview": true, "privacy": true, "public": true,
	"register": true, "robots.txt": true, "root": true, "s": true, "search": true,
	"settings": true, "share": true, "skill": true, "sso": true, "static": true,
	"storage": true, "stream": true, "support": true, "terms": true, "timeline": true,
	"true": true, "u": true, "upload": true, "user": true, "users": true,
	"web": true, "welcome": true, "www": true,
}

// NormalizeUsername validates and canonicalizes a handle. It is the only place the rule
// lives: the claim endpoint and any future address parser must not each re-implement it.
func NormalizeUsername(value string) (string, error) {
	handle := strings.ToLower(strings.TrimSpace(value))
	if handle == "" {
		return "", httpx.BadRequest("a username is required")
	}
	if len(handle) < 3 {
		return "", httpx.BadRequest("a username must be at least 3 characters")
	}
	if len(handle) > 30 {
		return "", httpx.BadRequest("a username must be at most 30 characters")
	}
	if !usernameRe.MatchString(handle) || strings.Contains(handle, "--") {
		return "", httpx.BadRequest(
			"a username may contain only lowercase letters, digits and single hyphens, and cannot start or end with a hyphen")
	}
	if reservedUsernames[handle] {
		return "", httpx.BadRequest("that username is reserved by the service; choose another")
	}
	return handle, nil
}
