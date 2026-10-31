package auth

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"everythingabout.me/backend/internal/config"
)

func TestSaveLinkSecretSurvivesStorageAndDatabaseRotation(t *testing.T) {
	seed := "previous-legacy-signing-seed"
	a := New(nil, &config.Config{AppSecret: seed, DatabaseURL: "postgres://one", COS: config.COS{SecretKey: "old-storage"}})
	b := New(nil, &config.Config{AppSecret: seed, DatabaseURL: "postgres://two", Storage: config.Storage{Backend: "local"}, COS: config.COS{SecretKey: "rotated-storage"}})
	expected := sha256.Sum256([]byte("eam/save-link/v1:" + seed))
	if !bytes.Equal(a.linkKey, expected[:]) || !bytes.Equal(a.linkKey, b.linkKey) {
		t.Fatal("explicitly preserved seed did not retain save-link identity")
	}
	changed := New(nil, &config.Config{AppSecret: "new-app-secret"})
	if bytes.Equal(a.linkKey, changed.linkKey) {
		t.Fatal("application secret rotation did not change signing identity")
	}
}
