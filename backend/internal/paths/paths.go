// Package paths handles logical paths: normalization, parent/child relations, and the
// mapping between a logical path and a COS object key.
package paths

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Visible files always live under the drive/ prefix; the other prefixes are system-private.
const (
	DrivePrefix     = "drive/"
	InventoryPrefix = "_inventory/"

	MaxPathDepth  = 32
	MaxNameLength = 200
)

// Error means the path is invalid; the API layer maps it to a 400.
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

func pathError(message string) error { return &Error{Message: message} }

// Normalize converts to NFC, trims leading and trailing slashes, and collapses repeated
// slashes. Filenames uploaded from macOS are NFD, and without normalization they turn into
// "two files with the same name" against the Linux side.
func Normalize(input string) (string, error) {
	value := norm.NFC.String(strings.ReplaceAll(input, "\\", "/"))
	value = strings.TrimSpace(value)

	segments := make([]string, 0, 8)
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." {
			return "", pathError("path cannot contain '..'")
		}
		if utf8.RuneCountInString(segment) > MaxNameLength {
			return "", pathError("a path segment is too long")
		}
		for _, r := range segment {
			if r <= 0x1f || r == 0x7f {
				return "", pathError("path contains control characters")
			}
		}
		segments = append(segments, segment)
	}
	if len(segments) > MaxPathDepth {
		return "", pathError("path is nested too deeply")
	}
	return strings.Join(segments, "/"), nil
}

func ParentOf(path string) string {
	index := strings.LastIndex(path, "/")
	if index == -1 {
		return ""
	}
	return path[:index]
}

func NameOf(path string) string {
	index := strings.LastIndex(path, "/")
	if index == -1 {
		return path
	}
	return path[index+1:]
}

func IsInside(path, ancestor string) bool {
	if ancestor == "" {
		return true
	}
	return path == ancestor || strings.HasPrefix(path, ancestor+"/")
}

// KeyFor is a file's object key: one prefix per user, so the same path from two different
// users never collides in the bucket.
func KeyFor(path, ownerID string) string {
	return DrivePrefix + ownerID + "/" + path
}

// DirKeyFor is the directory marker object key (trailing /, so coscli / rclone sees empty
// directories).
func DirKeyFor(path, ownerID string) string {
	if path == "" {
		return DrivePrefix + ownerID + "/"
	}
	return DrivePrefix + ownerID + "/" + path + "/"
}

// OwnerPrefix is a user's prefix in the bucket, ending in /.
func OwnerPrefix(ownerID string) string {
	return DrivePrefix + ownerID + "/"
}

// PathFromKey recovers the logical path from an object key; a key that does not belong to
// this user returns false.
func PathFromKey(key string, ownerID string) (string, bool) {
	prefix := DrivePrefix
	if ownerID != "" {
		prefix = OwnerPrefix(ownerID)
	}
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	return strings.TrimRight(key[len(prefix):], "/"), true
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// OwnerSegmentOf checks whether the second segment of a key looks like a user ID, which
// distinguishes drive/<path> from drive/<ownerID>/<path>.
func OwnerSegmentOf(key string) string {
	if !strings.HasPrefix(key, DrivePrefix) {
		return ""
	}
	rest := key[len(DrivePrefix):]
	first, _, _ := strings.Cut(rest, "/")
	if uuidRe.MatchString(strings.ToLower(first)) {
		return first
	}
	return ""
}

// AncestorsOf returns the directories from the topmost down to the immediate parent.
func AncestorsOf(path string) []string {
	var reversed []string
	for current := ParentOf(path); current != ""; current = ParentOf(current) {
		reversed = append(reversed, current)
	}
	result := make([]string, len(reversed))
	for i, item := range reversed {
		result[len(reversed)-1-i] = item
	}
	return result
}
