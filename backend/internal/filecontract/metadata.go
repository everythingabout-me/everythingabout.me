package filecontract

import "encoding/json"

// MarshalJSON preserves a useful internal Path type while representing roots
// and concealed parents as JSON null, rather than a navigable empty path.
func (m Metadata) MarshalJSON() ([]byte, error) {
	type metadata Metadata
	var parent *Path
	if m.ParentPath != "" && m.Path != "/" {
		p := m.ParentPath
		parent = &p
	}
	return json.Marshal(struct {
		*metadata
		ParentPath *Path `json:"parentPath"`
	}{metadata: (*metadata)(&m), ParentPath: parent})
}
