// Package filerevision derives public observation revisions from private native
// evidence. These values are not native mutation fences.
package filerevision

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"sort"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

// Content identifies a selected native file representation within its root. A
// trusted driver may supply identity independent of a path to preserve revisions
// through rename. Other drivers retain the path-scoped P2 revision contract.
func Content(namespace string, entry storage.Entry) *string {
	if entry.Kind != storage.File || len(entry.Version) == 0 {
		return nil
	}
	h := sha256.New()
	h.Write([]byte("eam/files/content/v1\x00"))
	values := [][]byte{[]byte(namespace), []byte(entry.Path), entry.NativeRef, entry.Version}
	if len(entry.ContentIdentity) != 0 {
		// Domain separation prevents stable identities from aliasing path tuples.
		h.Write([]byte("stable-identity\x00"))
		values = [][]byte{[]byte(namespace), entry.ContentIdentity, entry.Version}
	}
	for _, value := range values {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write(value)
	}
	revision := "cr_" + hex.EncodeToString(h.Sum(nil))
	return &revision
}

// Metadata hashes the public representation, root and selected native version.
// Folder membership is the immediate native child kind:path set. The input and
// membership slice are not modified, allowing settlement and reads to share it.
func Metadata(namespace string, metadata fc.Metadata, version []byte, membership []string) string {
	metadata.Revision = ""
	h := sha256.New()
	representation, _ := json.Marshal(metadata)
	h.Write(representation)
	evidence, _ := json.Marshal(struct {
		Namespace string
		Version   []byte
	}{namespace, version})
	h.Write(evidence)
	if metadata.Kind == "folder" {
		members := append([]string{}, membership...)
		sort.Strings(members)
		encoded, _ := json.Marshal(members)
		h.Write(encoded)
	}
	return "nr_" + hex.EncodeToString(h.Sum(nil))
}
