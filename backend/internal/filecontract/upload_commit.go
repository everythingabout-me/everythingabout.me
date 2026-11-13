package filecontract

import "encoding/json"

// UploadCommitResult is the durable result of a settled upload publication: the published
// file's metadata plus the revision its parent folder carries after the settlement.
//
// The parent folder's revision belongs in the answer because the client keeps that value
// for its next mutation of the folder — rename, move and delete all send it as
// If-Revision. Publishing a file changes the folder's revision, so a client without it has
// to read the folder back after an import just to learn a value the settlement already
// computed; a client that does not read sends the pre-import value and meets a permanent
// 412. This is stored as the operation result, so a repeated commit, a session status and
// an Operation poll all answer with the same document.
//
// It embeds Metadata and flattens it on the wire, because the commit answer has always been
// the file resource itself; wrapping it would break every existing reader.
type UploadCommitResult struct {
	Metadata
	ParentRevision string `json:"parentRevision"`
}

// MarshalJSON flattens the file's fields and appends parentRevision.
//
// Metadata has its own marshaler (it renders a concealed parent as null), and embedding it
// promotes that method to this type, which would encode only the file and silently drop the
// added field. Encoding the file first and adding the one key keeps both documented shapes
// in one place.
func (r UploadCommitResult) MarshalJSON() ([]byte, error) {
	resource, err := json.Marshal(r.Metadata)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(resource, &fields); err != nil {
		return nil, err
	}
	revision, err := json.Marshal(r.ParentRevision)
	if err != nil {
		return nil, err
	}
	fields["parentRevision"] = revision
	return json.Marshal(fields)
}
