package api

import (
	"testing"

	"everythingabout.me/backend/internal/auth"
	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/nameprobe"
	"everythingabout.me/backend/internal/storage"
)

func probeRequest(caseSensitive bool) *fileRequest {
	return &fileRequest{
		Driver:    &filesReadTestDriver{descriptor: storage.Descriptor{Names: storage.NameRules{CaseSensitive: caseSensitive}}},
		Principal: &auth.Principal{User: &auth.User{ID: "probe"}},
	}
}

// filesPathKey is the production producer for every WithinScope check: request
// scope, hidden pending-delete roots, change projection and cursor ordering.
// It must be the index's reference pipeline.
func TestFilesPathKeyUsesReferenceKey(t *testing.T) {
	nameprobe.CheckKey(t, "api.filesPathKey", nameprobe.ReferencePipeline, false, filesPathKey(probeRequest(false)))
}

// RequireRead gates every read with the same key, so a token scoped to one
// spelling can read an index-equal sibling and cannot read an index-distinct one.
func TestRequireReadUsesReferenceKey(t *testing.T) {
	f := probeRequest(false)
	nameprobe.CheckPair(t, "api.RequireRead", nameprobe.ReferencePipeline, false, func(a, b string) bool {
		f.Scope = fc.Path("/" + a)
		return f.RequireRead(fc.Path("/"+b)) == nil
	})
}
