package filerevision

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"testing"

	fc "everythingabout.me/backend/internal/filecontract"
	"everythingabout.me/backend/internal/storage"
)

func TestContentStableIdentityAndFallback(t *testing.T) {
	entry := storage.Entry{Path: "before.txt", Kind: storage.File, NativeRef: []byte("native"), Version: []byte("version"), ContentIdentity: []byte("inode")}
	before := *Content("owner:root", entry)
	entry.Path = "folder/after.txt"
	entry.NativeRef = []byte("new-locator")
	if got := *Content("owner:root", entry); got != before {
		t.Fatal("rename changed a driver-proven stable content revision")
	}
	if got := *Content("other-owner:root", entry); got == before {
		t.Fatal("root namespaces aliased")
	}
	entry.Version = []byte("replacement")
	if got := *Content("owner:root", entry); got == before {
		t.Fatal("replacement retained content revision")
	}
	entry.ContentIdentity = nil
	h := sha256.New()
	h.Write([]byte("eam/files/content/v1\x00"))
	for _, value := range [][]byte{[]byte("owner:root"), []byte(entry.Path), entry.NativeRef, entry.Version} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write(value)
	}
	if got := *Content("owner:root", entry); got != "cr_"+hex.EncodeToString(h.Sum(nil)) {
		t.Fatal("P2 fallback changed")
	}
	old := *Content("owner:root", entry)
	entry.Path = "another.txt"
	if got := *Content("owner:root", entry); got == old {
		t.Fatal("path-scoped fallback lost native object identity")
	}
	entry.Version = nil
	if Content("owner:root", entry) != nil {
		t.Fatal("unversioned content acquired a revision")
	}
}

func TestMetadataCompatibilityAndMembership(t *testing.T) {
	m := fc.Metadata{Path: "/folder", ParentPath: "/", Name: "folder", Kind: "folder"}
	membership := []string{"file:folder/z", "file:folder/a"}
	before := append([]string{}, membership...)
	got := Metadata("namespace", m, []byte("version"), membership)
	h := sha256.New()
	representation, _ := json.Marshal(m)
	h.Write(representation)
	evidence, _ := json.Marshal(struct {
		Namespace string
		Version   []byte
	}{"namespace", []byte("version")})
	h.Write(evidence)
	members, _ := json.Marshal([]string{"file:folder/a", "file:folder/z"})
	h.Write(members)
	if got != "nr_"+hex.EncodeToString(h.Sum(nil)) {
		t.Fatal("existing metadata revision contract changed")
	}
	if membership[0] != before[0] {
		t.Fatal("revision mutated caller membership")
	}
	m.Revision = "prior"
	if Metadata("namespace", m, []byte("version"), membership) != got {
		t.Fatal("revision recursively included itself")
	}
	if Metadata("namespace", m, []byte("version"), membership[:1]) == got {
		t.Fatal("folder membership change retained revision")
	}
	if Metadata("other", m, []byte("version"), membership) == got {
		t.Fatal("root change retained revision")
	}
}
