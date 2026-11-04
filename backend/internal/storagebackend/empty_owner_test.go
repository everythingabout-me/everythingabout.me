package storagebackend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/config"
	"everythingabout.me/backend/internal/storage"
)

func coordinatedLocalConfig(t *testing.T) (*Backend, *config.Config) {
	t.Helper()
	cfg := localConfig(t)
	cfg.Storage.CoordinatedWrites = true
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b, cfg
}

// Local owners exist only after the operator/native layer created both roots; the
// driver refuses to invent them. Object prefixes need no such directory.
func addEmptyLocalOwner(t *testing.T, cfg *config.Config, owner string) {
	t.Helper()
	for _, base := range []string{cfg.Storage.Local.ContentRoot, cfg.Storage.Local.SystemRoot} {
		if err := os.Mkdir(filepath.Join(base, owner), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func openOwner(t *testing.T, b *Backend, owner string) storage.Driver {
	t.Helper()
	driver, err := b.Open(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	return driver
}

func TestVerifyEmptyOwnerAcceptsFreshLocalOwner(t *testing.T) {
	b, cfg := coordinatedLocalConfig(t)
	owner := "fresh-owner"
	addEmptyLocalOwner(t, cfg, owner)
	driver := openOwner(t, b, owner)
	if err := b.VerifyEmptyOwner(context.Background(), owner, driver); err != nil {
		t.Fatalf("fresh owner rejected: %v", err)
	}
	// Repeated verification is read-only and remains admissible.
	if err := b.VerifyEmptyOwner(context.Background(), owner, driver); err != nil {
		t.Fatalf("second verification rejected: %v", err)
	}
}

func TestVerifyEmptyOwnerRejectsContent(t *testing.T) {
	b, cfg := coordinatedLocalConfig(t)
	owner := "content-owner"
	addEmptyLocalOwner(t, cfg, owner)
	driver := openOwner(t, b, owner)
	if err := os.WriteFile(filepath.Join(cfg.Storage.Local.ContentRoot, owner, "note.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := b.VerifyEmptyOwner(context.Background(), owner, driver); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("content owner admitted: %v", err)
	}
}

func TestVerifyEmptyOwnerRejectsSystemRootState(t *testing.T) {
	b, cfg := coordinatedLocalConfig(t)
	owner := "staged-owner"
	addEmptyLocalOwner(t, cfg, owner)
	driver := openOwner(t, b, owner)
	binding, err := b.Binding(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(binding.SystemRoot, "upload.part"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = b.VerifyEmptyOwner(context.Background(), owner, driver); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("owner with staged system state admitted: %v", err)
	}
}

func TestVerifyEmptyOwnerRejectsMismatchedDriver(t *testing.T) {
	b, cfg := coordinatedLocalConfig(t)
	addEmptyLocalOwner(t, cfg, "other-owner")
	driver := openOwner(t, b, "other-owner")
	if err := b.VerifyEmptyOwner(context.Background(), "fresh-owner", driver); !storage.IsCode(err, "invalid_config") {
		t.Fatalf("mismatched owner driver admitted: %v", err)
	}
}

func TestVerifyEmptyOwnerRequiresCoordinatedWrites(t *testing.T) {
	cfg := localConfig(t)
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	owner := "uncoordinated-owner"
	addEmptyLocalOwner(t, cfg, owner)
	driver, err := b.Open(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	defer driver.Close()
	if err = b.VerifyEmptyOwner(context.Background(), owner, driver); !storage.IsCode(err, "unsupported") {
		t.Fatalf("non-coordinated backend admitted: %v", err)
	}
}

func TestVerifyEmptyOwnerRejectsStagedContent(t *testing.T) {
	b, cfg := coordinatedLocalConfig(t)
	owner := "staged-content-owner"
	addEmptyLocalOwner(t, cfg, owner)
	driver := openOwner(t, b, owner)
	binding, err := b.Binding(owner)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(binding.SystemRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "staged-v1-") {
			if err = os.WriteFile(filepath.Join(binding.SystemRoot, entry.Name(), "part"), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = b.VerifyEmptyOwner(context.Background(), owner, driver); !storage.IsCode(err, "baseline_required") {
		t.Fatalf("owner with staged bytes admitted: %v", err)
	}
}
