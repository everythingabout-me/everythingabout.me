package api

import (
	"encoding/json"
	fc "everythingabout.me/backend/internal/filecontract"
)

// Saved journal results retain full owner provenance. Every response projects
// that result to the caller's current visible parent boundary.
func filesScopedResult(raw json.RawMessage, f *fileRequest) json.RawMessage {
	if len(raw) == 0 || f == nil {
		return raw
	}
	var resource map[string]json.RawMessage
	if json.Unmarshal(raw, &resource) != nil {
		return raw
	}
	var p, parent fc.Path
	if json.Unmarshal(resource["path"], &p) != nil {
		return raw
	}
	_ = json.Unmarshal(resource["parentPath"], &parent)
	if p == "/" || parent == "" || !fc.WithinScope(f.Scope, parent, filesPathKey(f)) {
		resource["parentId"] = json.RawMessage("null")
		resource["parentPath"] = json.RawMessage("null")
		encoded, err := json.Marshal(resource)
		if err == nil {
			return encoded
		}
	}
	return raw
}
