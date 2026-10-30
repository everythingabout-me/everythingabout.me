package filecontract

import (
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Path is an account-absolute path. Construct it with ParsePath or FromRelative.
// The zero value is invalid; root is "/".
type Path string

// ValidatePath checks syntax only; it never cleans, trims or normalizes spelling.
func ValidatePath(path string) error {
	if !utf8.ValidString(path) || !strings.HasPrefix(path, "/") {
		return problem(http.StatusBadRequest, "invalid_path", "A path must be valid UTF-8 and start with a slash.")
	}
	if path == "/" {
		return nil
	}
	if strings.ContainsRune(path, '\\') || strings.ContainsFunc(path, unicode.IsControl) {
		return problem(http.StatusBadRequest, "invalid_path", "Backslashes and control characters are not allowed in paths.")
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return problem(http.StatusBadRequest, "invalid_path", "Empty, dot and dot-dot path segments are not allowed.")
		}
	}
	return nil
}

func ParsePath(path string) (Path, error) {
	if err := ValidatePath(path); err != nil {
		return "", err
	}
	return Path(path), nil
}

// Relative removes exactly the boundary slash from a validated Path. Root maps to
// the existing internal empty root. It performs no URL decoding or name rewriting.
func (p Path) Relative() string { return strings.TrimPrefix(string(p), "/") }

// FromRelative converts native root-relative spelling to an API path. The native
// root is "". Already-absolute input is rejected to catch double conversions.
func FromRelative(path string) (Path, error) {
	if strings.HasPrefix(path, "/") {
		return "", problem(http.StatusBadRequest, "invalid_path", "An internal path must be relative to its root.")
	}
	return ParsePath("/" + path)
}

// ParseQueryPath consumes the raw query, decodes it exactly once and requires one
// path value. Do not pass URL.Query().Get("path") here; use ParsePath for decoded
// values and literal JSON strings. Malformed query escapes are never ignored.
func ParseQueryPath(rawQuery string) (Path, error) {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", problem(http.StatusBadRequest, "invalid_request", "The query encoding is invalid.")
	}
	values := query["path"]
	if len(values) != 1 {
		return "", problem(http.StatusBadRequest, "invalid_request", "Exactly one path query value is required.")
	}
	return ParsePath(values[0])
}

// SegmentKey supplies the native backend's name equivalence (for example NFC,
// case folding, or exact identity). It must not change segmentation. A nil key
// uses exact byte comparison; callers must supply different native semantics.
type SegmentKey func(string) string

// WithinScope compares validated paths at segment boundaries. It does not rebase
// scoped paths, follow file identity through moves, or resolve symlinks.
func WithinScope(scope, candidate Path, key SegmentKey) bool {
	if ValidatePath(string(scope)) != nil || ValidatePath(string(candidate)) != nil {
		return false
	}
	if scope == "/" {
		return true
	}
	source, target := strings.Split(scope.Relative(), "/"), strings.Split(candidate.Relative(), "/")
	if len(source) > len(target) {
		return false
	}
	for i := range source {
		a, b := source[i], target[i]
		if key != nil {
			a, b = key(a), key(b)
		}
		if a != b {
			return false
		}
	}
	return true
}

// RootBinding identifies the native tree, excluding credentials. A credential
// rotation does not change this value; moving roots or changing configuration
// generation does. It is not an ownership verifier or a filesystem sandbox.
type RootBinding struct {
	OwnerID      string
	ConnectionID string
	Generation   int64
	ContentRoot  string
	SystemRoot   string
}
