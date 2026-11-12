package api

import (
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestFilesPublicationMatches(t *testing.T) {
	revision := []byte(`native-etag:"abc"`)
	entry := storage.Entry{Kind: storage.File, Size: 5, Revision: revision}
	recorded := []filesPublicationCandidate{{size: 5, handle: revision}}

	if !filesPublicationMatches(entry, 5, recorded) {
		t.Fatal("the same publication was not recognized")
	}
	for _, tc := range []struct {
		name       string
		entry      storage.Entry
		size       int64
		candidates []filesPublicationCandidate
	}{
		{"a different upload size", entry, 6, recorded},
		{"no failed publication", entry, 5, nil},
		{"another session's revision", entry, 5, []filesPublicationCandidate{{size: 5, handle: []byte(`native-etag:"other"`)}}},
		{"a session that recorded another size", entry, 5, []filesPublicationCandidate{{size: 4, handle: revision}}},
		{"an entry with no revision", storage.Entry{Kind: storage.File, Size: 5}, 5, recorded},
		{"a folder at the destination", storage.Entry{Kind: storage.Folder, Size: 5, Revision: revision}, 5, recorded},
	} {
		if filesPublicationMatches(tc.entry, tc.size, tc.candidates) {
			t.Fatalf("%s was accepted as this owner's publication", tc.name)
		}
	}
}
