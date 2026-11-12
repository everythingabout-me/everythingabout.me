// Package storagebaseline compares read-only native and index snapshots. It never
// decides that unindexed native content is garbage or performs reconciliation.
package storagebaseline

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"everythingabout.me/backend/internal/filecontract"
	"golang.org/x/text/unicode/norm"
)

// Snapshot contains application metadata, never file bytes or storage credentials.
// Complete describes enumeration, not a cross-system snapshot or writer fencing.
type Snapshot struct {
	Version         int               `json:"version"`
	OwnerID         string            `json:"ownerId"`
	Backend         string            `json:"backend"`
	ContentRoot     string            `json:"contentRoot"`
	StartedAt       time.Time         `json:"startedAt"`
	FinishedAt      time.Time         `json:"finishedAt"`
	NativeComplete  bool              `json:"nativeComplete"`
	IndexComplete   bool              `json:"indexComplete"`
	WritersQuiesced bool              `json:"writersQuiesced"`
	Native          []NativeEntry     `json:"native"`
	Index           []IndexEntry      `json:"index"`
	LegacyUploads   []json.RawMessage `json:"legacyUploads"`
}

type NativeEntry struct {
	Key        string    `json:"key"`
	Size       int64     `json:"size"`
	ETag       string    `json:"etag,omitempty"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

type IndexEntry struct {
	ID       string          `json:"id"`
	Key      string          `json:"key"`
	Path     string          `json:"path"` // existing internal relative path, without normalization
	Folder   bool            `json:"folder"`
	Size     int64           `json:"size"`
	ETag     *string         `json:"etag"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

type Issue struct {
	Code   string `json:"code"`
	Path   string `json:"path"`
	Key    string `json:"key,omitempty"`
	Detail string `json:"detail"`
}

type Report struct {
	NativeFiles              int     `json:"nativeFiles"`
	NativeFolders            int     `json:"nativeFolders"` // includes implicit parents, excludes root
	NativeBytes              int64   `json:"nativeBytes"`
	IndexBytes               int64   `json:"indexBytes"`
	OverQuota                bool    `json:"overQuota"`
	Comparable               bool    `json:"comparable"`
	ReservationBaselineReady bool    `json:"reservationBaselineReady"`
	Issues                   []Issue `json:"issues"`
}

// Compare keeps every native file charged, including files missing from the index.
// quota==0 means unlimited. Over-quota data is preserved; no removal is suggested.
func Compare(s Snapshot, quota int64) Report {
	r := Report{Issues: []Issue{}}
	issue := func(code, path, key, detail string) { r.Issues = append(r.Issues, Issue{code, path, key, detail}) }
	prefix := "drive/" + s.OwnerID + "/"
	if s.Version != 1 || s.OwnerID == "" || strings.ContainsAny(s.OwnerID, "/\\") || s.ContentRoot != prefix || s.Backend != "cos" {
		issue("invalid_binding", "", "", "Expected version 1 and an explicit COS owner root.")
		return r
	}
	if quota < 0 {
		issue("invalid_quota", "", "", "Quota cannot be negative.")
	}
	r.Comparable = s.NativeComplete && s.IndexComplete && s.WritersQuiesced && !s.FinishedAt.Before(s.StartedAt) && !s.StartedAt.IsZero() && !s.FinishedAt.IsZero()
	if !r.Comparable {
		issue("incomplete_baseline", "", "", "Complete listings and independently established writer quiescence are required.")
	}
	native := map[string]NativeEntry{}
	kinds := map[string]bool{}
	spellings := map[string]string{}
	addPath := func(path string, folder bool, key string) {
		if old, ok := kinds[path]; ok && old != folder {
			issue("name_conflict", "/"+path, key, "A file and folder occupy the same path.")
		}
		kinds[path] = folder
		canonical := norm.NFC.String(path)
		if old, ok := spellings[canonical]; ok && old != path {
			issue("normalization_conflict", "/"+path, key, "Native spelling collides with "+old)
		} else {
			spellings[canonical] = path
		}
	}
	for _, e := range s.Native {
		if !strings.HasPrefix(e.Key, prefix) {
			issue("outside_root", "", e.Key, "Native key is outside the captured owner root.")
			continue
		}
		if _, ok := native[e.Key]; ok {
			issue("duplicate_key", "", e.Key, "Native listing repeated a key.")
			continue
		}
		native[e.Key] = e
		path := strings.TrimPrefix(e.Key, prefix)
		folder := strings.HasSuffix(e.Key, "/")
		if folder {
			path = strings.TrimSuffix(path, "/")
		}
		if e.Key == prefix {
			if e.Size != 0 {
				issue("nonempty_folder_marker", "/", e.Key, "A nonempty root marker needs explicit operator resolution.")
			}
			continue
		}
		if path == "" {
			issue("invalid_name", "/", e.Key, "A file cannot occupy the bound root.")
			continue
		}
		if err := validateRelative(path); err != nil {
			issue("invalid_name", "/"+path, e.Key, err.Error())
		}
		if e.Size < 0 {
			issue("invalid_size", "/"+path, e.Key, "Native size cannot be negative.")
		}
		// A folder marker's body is not content: the service reasserts a marker during a
		// purge with a short token so its ETag moves. The key ending in "/" is what makes
		// it a folder; only the root marker below still requires an empty body, because the
		// service never reasserts the account root.
		if !folder && e.Size >= 0 {
			r.NativeFiles++
			if e.Size > 9007199254740991-r.NativeBytes {
				issue("usage_overflow", "/"+path, e.Key, "Logical usage exceeds the public integer limit.")
			} else {
				r.NativeBytes += e.Size
			}
		}
		addPath(path, folder, e.Key)
		segments := strings.Split(path, "/")
		for i := 1; i < len(segments); i++ {
			addPath(strings.Join(segments[:i], "/"), true, "")
		}
	}
	for path, folder := range kinds {
		if folder && path != "" {
			r.NativeFolders++
		}
	}
	indexed := map[string]bool{}
	for _, e := range s.Index {
		path := e.Path
		if indexed[path] {
			issue("duplicate_index_path", "/"+path, e.Key, "Index contains a duplicate path.")
		}
		indexed[path] = true
		expected := prefix + path
		if e.Folder {
			expected += "/"
		}
		if path == "" && e.Folder {
			expected = prefix
		}
		if e.Key != expected {
			issue("index_mapping_mismatch", "/"+path, e.Key, "Index key differs from its exact owner/path mapping.")
		}
		if path != "" {
			if err := validateRelative(path); err != nil {
				issue("invalid_index_path", "/"+path, e.Key, err.Error())
			}
		}
		if e.Size < 0 {
			issue("invalid_index_size", "/"+path, e.Key, "Index size cannot be negative.")
		}
		if !e.Folder && e.Size >= 0 {
			if e.Size > 9007199254740991-r.IndexBytes {
				issue("usage_overflow", "/"+path, e.Key, "Indexed usage exceeds the public integer limit.")
			} else {
				r.IndexBytes += e.Size
			}
		}
		folder, found := kinds[path]
		if path == "" && e.Folder {
			continue
		}
		if !found {
			issue("index_only", "/"+path, e.Key, "No native entry or implicit parent was captured; do not prune from an incomplete scan.")
			continue
		}
		if folder != e.Folder {
			issue("kind_mismatch", "/"+path, e.Key, "Native and indexed entry kinds differ.")
			continue
		}
		if e.Folder {
			continue
		}
		n, ok := native[e.Key]
		if !ok {
			issue("index_mapping_mismatch", "/"+path, e.Key, "No exact native key matches the index.")
			continue
		}
		if e.Size != n.Size {
			issue("size_mismatch", "/"+path, e.Key, "Native and indexed file sizes differ.")
		}
		if e.ETag != nil && n.ETag != "" && *e.ETag != n.ETag {
			issue("validator_mismatch", "/"+path, e.Key, "Opaque native validators differ; neither is assumed to be a checksum.")
		}
	}
	for path := range kinds {
		if !indexed[path] {
			issue("native_only", "/"+path, "", "Native content is retained and charged; index repair is required.")
		}
	}
	if len(s.LegacyUploads) > 0 {
		issue("legacy_sessions_present", "", "", "Legacy sessions require provider-side reconciliation; database status alone does not revoke native authority.")
	}
	r.OverQuota = quota > 0 && r.NativeBytes > quota
	sort.Slice(r.Issues, func(i, j int) bool {
		a, b := r.Issues[i], r.Issues[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Key < b.Key
	})
	r.ReservationBaselineReady = r.Comparable && len(r.Issues) == 0
	return r
}

func validateRelative(path string) error {
	_, err := filecontract.FromRelative(path)
	return err
}
