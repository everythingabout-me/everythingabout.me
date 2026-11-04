package filecontract

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"unicode"
)

// EntrySelector identifies an existing resource without requiring a path-to-ID
// lookup by the client. The string JSON form retains the original path shortcut.
type EntrySelector struct {
	ID   string `json:"id,omitempty"`
	Path Path   `json:"path,omitempty"`
}

func invalidSelector() error {
	return NewProblem(400, "invalid_selector", "Provide exactly one nonempty ID or path, or one valid destination.")
}
func (s EntrySelector) IsID() bool { return s.ID != "" }

// ValidEntryID accepts canonical UUID spelling; callers still treat it as opaque
// and must authorize against the owner and current hierarchy after resolution.
func ValidEntryID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s EntrySelector) Validate() error {
	if (s.ID == "") == (s.Path == "") {
		return invalidSelector()
	}
	if s.ID != "" {
		if !ValidEntryID(s.ID) {
			return invalidSelector()
		}
		return nil
	}
	return ValidatePath(string(s.Path))
}

// decodeSelectorObject rejects duplicate fields rather than accepting the last
// conflicting value. All selector forms share the same strict structural rules.
func decodeSelectorObject(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalidSelector()
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, invalidSelector()
		}
		key, ok := token.(string)
		if !ok {
			return nil, invalidSelector()
		}
		if _, exists := fields[key]; exists {
			return nil, invalidSelector()
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, invalidSelector()
		}
		fields[key] = value
	}
	if _, err = d.Token(); err != nil {
		return nil, invalidSelector()
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, invalidSelector()
	}
	return fields, nil
}
func (s *EntrySelector) UnmarshalJSON(data []byte) error {
	*s = EntrySelector{}
	if len(data) > 0 && data[0] == '"' {
		var p string
		if json.Unmarshal(data, &p) != nil {
			return invalidSelector()
		}
		s.Path = Path(p)
		return s.Validate()
	}
	fields, err := decodeSelectorObject(data)
	if err != nil {
		return err
	}
	if len(fields) != 1 {
		return invalidSelector()
	}
	for key, value := range fields {
		switch key {
		case "id":
			err = json.Unmarshal(value, &s.ID)
		case "path":
			err = json.Unmarshal(value, &s.Path)
		default:
			return invalidSelector()
		}
		if err != nil {
			return invalidSelector()
		}
	}
	return s.Validate()
}

// Destination selects a new name, not the identity of an existing file. A
// parent selector is resolved by the service; it never silently creates parents.
type Destination struct {
	Path   Path           `json:"path,omitempty"`
	Parent *EntrySelector `json:"parent,omitempty"`
	Name   string         `json:"name,omitempty"`
}

func (d Destination) Validate() error {
	if d.Path != "" {
		if d.Parent != nil || d.Name != "" {
			return invalidSelector()
		}
		return ValidatePath(string(d.Path))
	}
	if d.Parent == nil || d.Name == "" {
		return invalidSelector()
	}
	if err := d.Parent.Validate(); err != nil {
		return err
	}
	if d.Name == "." || d.Name == ".." || strings.ContainsAny(d.Name, "/\\") || strings.ContainsFunc(d.Name, unicode.IsControl) {
		return NewProblem(400, "invalid_path", "A destination name must be one valid filename segment.")
	}
	return ValidatePath("/" + d.Name)
}
func (d *Destination) UnmarshalJSON(data []byte) error {
	*d = Destination{}
	if len(data) > 0 && data[0] == '"' {
		var p string
		if json.Unmarshal(data, &p) != nil {
			return invalidSelector()
		}
		d.Path = Path(p)
		return d.Validate()
	}
	fields, err := decodeSelectorObject(data)
	if err != nil {
		return err
	}
	if _, ok := fields["path"]; ok && len(fields) != 1 {
		return invalidSelector()
	}
	for key, value := range fields {
		switch key {
		case "path":
			err = json.Unmarshal(value, &d.Path)
		case "parent":
			err = json.Unmarshal(value, &d.Parent)
		case "name":
			err = json.Unmarshal(value, &d.Name)
		default:
			return invalidSelector()
		}
		if err != nil {
			return err
		}
	}
	return d.Validate()
}

// ParseSelectorQuery decodes query text once and rejects duplicate values. Only
// callers with an explicitly documented default may allow an absent selector.
func ParseSelectorQuery(raw string, allowDefault bool) (EntrySelector, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return EntrySelector{}, NewProblem(400, "invalid_request", "The query encoding is invalid.")
	}
	for _, v := range q {
		if len(v) != 1 {
			return EntrySelector{}, invalidSelector()
		}
	}
	id, hasID := q["id"]
	p, hasPath := q["path"]
	if !hasID && !hasPath && allowDefault {
		return EntrySelector{}, nil
	}
	if hasID == hasPath {
		return EntrySelector{}, invalidSelector()
	}
	s := EntrySelector{}
	if hasID {
		s.ID = id[0]
	} else {
		s.Path = Path(p[0])
	}
	return s, s.Validate()
}
