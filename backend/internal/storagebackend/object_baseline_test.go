package storagebackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/objectmutation"
	"everythingabout.me/backend/internal/storage"
)

// All methods except HEAD are intentionally unavailable: baseline validation must
// never open content or issue a provider mutation.
type baselineObjectBackend struct {
	objectmutation.Backend
	head func(string) (objectmutation.Object, error)
}

func (b baselineObjectBackend) Head(_ context.Context, key string) (objectmutation.Object, error) {
	return b.head(key)
}

func objectBaselineBackend(t *testing.T, createOnly bool, head func(string) (objectmutation.Object, error)) *Backend {
	t.Helper()
	return &Backend{
		cfg:           config.Config{Storage: config.Storage{Backend: "cos", ConnectionID: "baseline", Generation: 1, CoordinatedWrites: true}, COS: config.COS{Bucket: "fixture-123456", Region: "test"}},
		objectBackend: baselineObjectBackend{head: head},
		objectOptions: objectmutation.Options{Admission: objectmutation.Admission{CreateOnly: createOnly}, SpoolRoot: t.TempDir()},
	}
}

func TestObjectBaselineUnversionedExplicitMarkersRequireCreateOnly(t *testing.T) {
	for _, createOnly := range []bool{false, true} {
		for _, version := range []string{"", "null", "retained-version"} {
			name := "versioned/" + version
			if createOnly {
				name = "create-only/" + version
			}
			t.Run(name, func(t *testing.T) {
				heads := 0
				b := objectBaselineBackend(t, createOnly, func(key string) (objectmutation.Object, error) {
					heads++
					if key != "drive/owner/Empty/" {
						t.Fatalf("baseline accessed another native key: %q", key)
					}
					return objectmutation.Object{Key: key, VersionID: version}, nil
				})
				err := b.ValidateObjectBaseline(context.Background(), "owner", []storage.Entry{{Path: "Empty", Kind: storage.Folder}})
				if createOnly || version == "retained-version" {
					if err != nil {
						t.Fatalf("admitted explicit marker rejected: %v", err)
					}
				} else if !storage.IsCode(err, "unsupported") {
					t.Fatalf("unversioned marker bypassed versioned admission: %v", err)
				}
				if heads != 1 {
					t.Fatalf("expected one read-only HEAD, got %d", heads)
				}
				entries, err := os.ReadDir(b.objectOptions.SpoolRoot)
				if err != nil || len(entries) != 0 {
					t.Fatalf("baseline allocated spool state: %v", err)
				}
			})
		}
	}
}

func TestObjectBaselineImplicitFolderRequiresCreateOnly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		createOnly bool
		kind       storage.Kind
		code       string
		accepted   bool
	}{
		{"create-only implicit folder", true, storage.Folder, "not_found", true},
		{"versioned implicit folder", false, storage.Folder, "not_found", false},
		{"create-only missing file", true, storage.File, "not_found", false},
		{"create-only unreadable folder", true, storage.Folder, "forbidden", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heads := 0
			b := objectBaselineBackend(t, tc.createOnly, func(key string) (objectmutation.Object, error) {
				heads++
				want := "drive/owner/Prefix"
				if tc.kind == storage.Folder {
					want += "/"
				}
				if key != want {
					t.Fatalf("baseline accessed an unexpected native key: %q", key)
				}
				return objectmutation.Object{}, storage.NewError(tc.code, "Prefix", nil)
			})
			err := b.ValidateObjectBaseline(context.Background(), "owner", []storage.Entry{{Path: "Prefix", Kind: tc.kind}})
			if tc.accepted {
				if err != nil {
					t.Fatalf("scanned implicit folder rejected: %v", err)
				}
			} else if !storage.IsCode(err, tc.code) {
				t.Fatalf("baseline bypassed native HEAD failure: %v", err)
			}
			if heads != 1 {
				t.Fatalf("expected one read-only HEAD, got %d", heads)
			}
			entries, err := os.ReadDir(b.objectOptions.SpoolRoot)
			if err != nil || len(entries) != 0 {
				t.Fatalf("implicit folder admission allocated spool state: %v", err)
			}
		})
	}
}

func TestCreateOnlyObjectBaselineStillChecksCurrentObjectAndEmptySpool(t *testing.T) {
	for _, tc := range []struct {
		name   string
		entry  storage.Entry
		object objectmutation.Object
	}{
		{"file", storage.Entry{Path: "file", Kind: storage.File, Size: 7}, objectmutation.Object{Key: "drive/owner/file", Size: 7}},
		{"wrong owner", storage.Entry{Path: "file", Kind: storage.File, Size: 7}, objectmutation.Object{Key: "drive/other/file", Size: 7}},
		{"changed size", storage.Entry{Path: "file", Kind: storage.File, Size: 7}, objectmutation.Object{Key: "drive/owner/file", Size: 8}},
		{"nonempty marker", storage.Entry{Path: "Empty", Kind: storage.Folder}, objectmutation.Object{Key: "drive/owner/Empty/", Size: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := objectBaselineBackend(t, true, func(string) (objectmutation.Object, error) { return tc.object, nil })
			err := b.ValidateObjectBaseline(context.Background(), "owner", []storage.Entry{tc.entry})
			if tc.name == "file" {
				if err != nil {
					t.Fatalf("unversioned current file rejected: %v", err)
				}
			} else if !storage.IsCode(err, "unsupported") {
				t.Fatalf("invalid current object accepted: %v", err)
			}
		})
	}
	b := objectBaselineBackend(t, true, func(key string) (objectmutation.Object, error) { return objectmutation.Object{Key: key}, nil })
	binding, err := b.Binding("owner")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	namespace := filepath.Join(b.objectOptions.SpoolRoot, "object-mutations-v1-"+hex.EncodeToString(hash[:]))
	if err = os.Mkdir(namespace, 0700); err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(namespace, "unknown-intent")
	if err = os.WriteFile(retained, []byte("retained evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = b.ValidateObjectBaseline(context.Background(), "owner", nil); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("create-only bypassed nonempty spool: %v", err)
	}
	if data, err := os.ReadFile(retained); err != nil || string(data) != "retained evidence" {
		t.Fatalf("baseline changed existing spool evidence: %v", err)
	}
}

func TestCreateOnlyGetterRequiresAdmittedObjectWrites(t *testing.T) {
	b := objectBaselineBackend(t, true, nil)
	if !b.CreateOnly() {
		t.Fatal("explicit create-only object admission was lost")
	}
	b.cfg.Storage.CoordinatedWrites = false
	if b.CreateOnly() {
		t.Fatal("read-only backend advertised create-only writes")
	}
	b.cfg.Storage.CoordinatedWrites = true
	b.objectBackend = nil
	if b.CreateOnly() {
		t.Fatal("non-object backend advertised create-only writes")
	}
}
