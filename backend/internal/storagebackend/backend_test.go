package storagebackend

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
)

func localConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{Storage: config.Storage{Backend: "local", ConnectionID: "local-test", Generation: 1, Local: config.Local{ContentRoot: t.TempDir(), SystemRoot: t.TempDir(), CaseSensitive: false}}}
}
func makeOwner(t *testing.T, cfg *config.Config, owner, data string) {
	t.Helper()
	content := filepath.Join(cfg.Storage.Local.ContentRoot, owner)
	system := filepath.Join(cfg.Storage.Local.SystemRoot, owner)
	if err := os.Mkdir(content, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(system, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(content, " a+?#%.txt "), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestLocalFactoryIsolatedWithoutCOSAndNativeExit(t *testing.T) {
	cfg := localConfig(t)
	makeOwner(t, cfg, "owner-a", "first")
	makeOwner(t, cfg, "owner-b", "second")
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, tc := range []struct{ owner, want string }{{"owner-a", "first"}, {"owner-b", "second"}} {
		d, err := b.Open(context.Background(), tc.owner)
		if err != nil {
			t.Fatal(err)
		}
		r, err := d.Open(context.Background(), " a+?#%.txt ", storage.ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		d.Close()
		if string(got) != tc.want {
			t.Fatal("owner roots mixed")
		}
	}
	// Independent ordinary native reads require no database or application decoder.
	got, err := os.ReadFile(filepath.Join(cfg.Storage.Local.ContentRoot, "owner-a", " a+?#%.txt "))
	if err != nil || string(got) != "first" {
		t.Fatalf("native exit: %q %v", got, err)
	}
	legacy := b.Legacy()
	r, err := legacy.Get(context.Background(), "drive/owner-b/ a+?#%.txt ")
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(r)
	r.Close()
	if err != nil || string(got) != "second" {
		t.Fatal("legacy parser read bridge failed")
	}
	if err = legacy.Remove(context.Background(), "drive/owner-a/ a+?#%.txt "); err == nil {
		t.Fatal("new backend exposed legacy mutation")
	}
	if _, err = legacy.PutURL(context.Background(), "drive/owner-a/ a+?#%.txt ", 1); err == nil {
		t.Fatal("new backend minted provider URL")
	}
	if _, err = legacy.TemporaryCredentials(context.Background(), "drive/owner-a/ a+?#%.txt ", 1); err == nil {
		t.Fatal("new backend minted provider credentials")
	}
	for _, owner := range []string{"", "../owner", "owner-a/child", "/owner-a", "."} {
		if _, err = b.Open(context.Background(), owner); err == nil {
			t.Fatalf("invalid owner accepted %q", owner)
		}
	}
	b.Close()
	if _, err = b.Open(context.Background(), "owner-a"); !storage.IsCode(err, "closed") {
		t.Fatalf("closed backend reopened: %v", err)
	}
}
func TestRootBindingIdentityExcludesCredentials(t *testing.T) {
	cfg := &config.Config{COS: config.COS{Bucket: "bucket-123456", Region: "ap-guangzhou", SecretID: "id", SecretKey: "old"}, Storage: config.Storage{Backend: "cos", ConnectionID: "test", Generation: 2}}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	first, err := a.Binding("owner")
	if err != nil {
		t.Fatal(err)
	}
	cfg.COS.SecretKey = "rotated"
	cfg.COS.SecretID = "rotated-id"
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	second, _ := b.Binding("owner")
	if first != second {
		t.Fatal("credential rotation changed root identity")
	}
	cfg.COS.Bucket = "different-123456"
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	third, _ := c.Binding("owner")
	if third.ConnectionID == first.ConnectionID {
		t.Fatal("bucket replacement retained old identity")
	}
	if first.ContentRoot != "drive/owner/" || first.SystemRoot != "__eam_system__/owner/" {
		t.Fatalf("existing native layout changed: %+v", first)
	}
}
