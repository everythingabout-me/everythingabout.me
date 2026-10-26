// Package paths 处理逻辑路径：归一化、父子关系，以及逻辑路径和
// COS 对象 key 之间的映射。
package paths

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// 可见文件统一放在 drive/ 前缀下，其余前缀是系统私有区。
const (
	DrivePrefix     = "drive/"
	InventoryPrefix = "_inventory/"

	MaxPathDepth  = 32
	MaxNameLength = 200
)

// Error 表示路径不合法，接口层映射成 400。
type Error struct {
	Message string
}

func (e *Error) Error() string { return e.Message }

func pathError(message string) error { return &Error{Message: message} }

// Normalize 归一化为 NFC、去掉首尾斜杠、折叠重复斜杠。
// macOS 上传的文件名是 NFD，不归一化会和 Linux 侧产生“同名两个文件”。
func Normalize(input string) (string, error) {
	value := norm.NFC.String(strings.ReplaceAll(input, "\\", "/"))
	value = strings.TrimSpace(value)

	segments := make([]string, 0, 8)
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." {
			continue
		}
		if segment == ".." {
			return "", pathError("路径不能包含 ..")
		}
		if utf8.RuneCountInString(segment) > MaxNameLength {
			return "", pathError("单级名称过长")
		}
		for _, r := range segment {
			if r <= 0x1f || r == 0x7f {
				return "", pathError("路径包含控制字符")
			}
		}
		segments = append(segments, segment)
	}
	if len(segments) > MaxPathDepth {
		return "", pathError("目录层级过深")
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

// KeyFor 文件对象 key：每个用户一个独立前缀，避免不同用户同名路径在桶里撞车。
func KeyFor(path, ownerID string) string {
	return DrivePrefix + ownerID + "/" + path
}

// DirKeyFor 目录标记对象 key（以 / 结尾，方便 coscli / rclone 识别空目录）。
func DirKeyFor(path, ownerID string) string {
	if path == "" {
		return DrivePrefix + ownerID + "/"
	}
	return DrivePrefix + ownerID + "/" + path + "/"
}

// OwnerPrefix 是某个用户在桶里的前缀，以 / 结尾。
func OwnerPrefix(ownerID string) string {
	return DrivePrefix + ownerID + "/"
}

// PathFromKey 从对象 key 还原逻辑路径；不属于该用户的返回 false。
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

// OwnerSegmentOf 判断 key 的第二段像不像用户 ID，
// 用于区分 drive/<path> 和 drive/<ownerID>/<path>。
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

// AncestorsOf 返回从最顶层到直接父目录的列表。
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
