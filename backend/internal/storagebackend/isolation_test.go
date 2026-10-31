package storagebackend

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
)

func TestLocalOwnerAliasRejectedAtBackendBoundary(t *testing.T) {
	cfg := localConfig(t)
	makeOwner(t, cfg, "owner-b", "private-b")
	if err := os.Symlink(filepath.Join(cfg.Storage.Local.ContentRoot, "owner-b"), filepath.Join(cfg.Storage.Local.ContentRoot, "owner-a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.Storage.Local.SystemRoot, "owner-a"), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if d, err := b.Open(context.Background(), "owner-a"); err == nil {
		d.Close()
		t.Fatal("owner symlink followed into another owner's content")
	}
	d, err := b.Open(context.Background(), "owner-b")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err = d.Stat(context.Background(), " a+?#%.txt "); err != nil {
		t.Fatal("valid unrelated owner blocked", err)
	}
}
func TestLocalGloballyOverlappingBasesRejected(t *testing.T) {
	cfg := localConfig(t)
	cfg.Storage.Local.SystemRoot = filepath.Join(cfg.Storage.Local.ContentRoot, "system")
	if err := os.Mkdir(cfg.Storage.Local.SystemRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if b, err := New(cfg); err == nil {
		b.Close()
		t.Fatal("global content/system ancestor overlap accepted")
	}
}
func TestS3GlobalRootSeparationBeforeOwnerBinding(t *testing.T) {
	for _, tc := range []struct {
		content, system string
		valid           bool
	}{
		{"drive/", "__eam_system__/", true},
		{"drive/", "drive-old/", true},
		{"drive/", "drive/", false},
		{"drive/", "drive/owner-a/staging/", false},
		{"drive/nested/", "drive/", false},
		{"caf\u00e9/", "cafe\u0301/system/", false},
		{"drive//", "system/", false},
		{"/", "system/", false},
	} {
		t.Run(tc.content+tc.system, func(t *testing.T) {
			cfg := &config.Config{Storage: config.Storage{Backend: "s3", Generation: 1, ConnectionID: "roots", S3: config.S3{Endpoint: "http://127.0.0.1:1", Region: "test", Bucket: "bucket", AccessKeyID: "fixture", SecretAccessKey: "fixture", PathStyle: true, ContentPrefix: tc.content, SystemPrefix: tc.system}}}
			b, err := New(cfg)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				d, err := b.Open(context.Background(), "owner-b")
				if err != nil {
					t.Fatal(err)
				}
				d.Close()
			} else if err == nil {
				b.Close()
				t.Fatal("overlapping or invalid global roots accepted")
			} else if !storage.IsCode(err, "invalid_config") {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}
