package localstorage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func ownerFixture(t *testing.T) (*Factory, storage.Config, string, string) {
	t.Helper()
	factory, baseCfg := fixture(t, Options{MaxConcurrentOperations: 1})
	for _, base := range []string{baseCfg.ContentRoot, baseCfg.SystemRoot} {
		for _, owner := range []string{"owner-a", "owner-b"} {
			if err := os.Mkdir(filepath.Join(base, owner), 0700); err != nil {
				t.Fatal(err)
			}
		}
	}
	cfg := baseCfg
	cfg.ContentRoot = filepath.Join(baseCfg.ContentRoot, cfg.OwnerID)
	cfg.SystemRoot = filepath.Join(baseCfg.SystemRoot, cfg.OwnerID)
	writeFixture(t, baseCfg.ContentRoot, "owner-a/file", "owner a")
	writeFixture(t, baseCfg.ContentRoot, "owner-b/file", "owner b secret")
	return factory, cfg, baseCfg.ContentRoot, baseCfg.SystemRoot
}

func TestOpenOwnerIsolationAndExactBinding(t *testing.T) {
	f, cfg, contentBase, systemBase := ownerFixture(t)
	a, err := f.OpenOwner(context.Background(), cfg, contentBase, systemBase)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	_, got := readContents(t, a, "file", storage.ReadOptions{})
	if got != "owner a" {
		t.Fatal(got)
	}
	for _, change := range []func(*storage.Config){func(c *storage.Config) { c.ContentRoot = filepath.Join(contentBase, "owner-b") }, func(c *storage.Config) { c.SystemRoot = filepath.Join(systemBase, "owner-b") }, func(c *storage.Config) {
		c.OwnerID = "OWNER-A"
		c.ContentRoot = filepath.Join(contentBase, c.OwnerID)
		c.SystemRoot = filepath.Join(systemBase, c.OwnerID)
	}} {
		changed := cfg
		change(&changed)
		bad, err := f.OpenOwner(context.Background(), changed, contentBase, systemBase)
		if bad != nil {
			bad.Close()
			t.Fatal("mismatched owner binding opened")
		}
		requireCode(t, err, "invalid_config")
	}
	// The binding retains its verified inode if the owner path is later replaced.
	held := cfg.ContentRoot + "-held"
	if err := os.Rename(cfg.ContentRoot, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("owner-b", cfg.ContentRoot); err != nil {
		t.Fatal(err)
	}
	_, got = readContents(t, a, "file", storage.ReadOptions{})
	if got != "owner a" {
		t.Fatal("binding followed replaced owner path")
	}
	bad, err := f.OpenOwner(context.Background(), cfg, contentBase, systemBase)
	if bad != nil {
		bad.Close()
		t.Fatal("owner symlink opened")
	}
	requireCode(t, err, "invalid_config")
	// A bad sibling cannot expose itself or prevent opening an unrelated valid owner.
	bCfg := cfg
	bCfg.OwnerID = "owner-b"
	bCfg.ContentRoot = filepath.Join(contentBase, bCfg.OwnerID)
	bCfg.SystemRoot = filepath.Join(systemBase, bCfg.OwnerID)
	b, err := f.OpenOwner(context.Background(), bCfg, contentBase, systemBase)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	_, got = readContents(t, b, "file", storage.ReadOptions{})
	if got != "owner b secret" {
		t.Fatal(got)
	}
}

func TestOpenOwnerRejectsSystemAliasAndGlobalBaseOverlap(t *testing.T) {
	f, cfg, contentBase, systemBase := ownerFixture(t)
	held := cfg.SystemRoot + "-held"
	if err := os.Rename(cfg.SystemRoot, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("owner-b", cfg.SystemRoot); err != nil {
		t.Fatal(err)
	}
	bad, err := f.OpenOwner(context.Background(), cfg, contentBase, systemBase)
	if bad != nil {
		bad.Close()
		t.Fatal("system owner alias opened")
	}
	requireCode(t, err, "invalid_config")
	// Owner-specific children may be disjoint even though the trusted bases overlap.
	nested := filepath.Join(contentBase, "system-base")
	if err := os.MkdirAll(filepath.Join(nested, cfg.OwnerID), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.SystemRoot = filepath.Join(nested, cfg.OwnerID)
	_, err = f.OpenOwner(context.Background(), cfg, contentBase, nested)
	requireCode(t, err, "invalid_config")
	alias := filepath.Join(filepath.Dir(contentBase), "content-alias")
	if err := os.Symlink(contentBase, alias); err != nil {
		t.Fatal(err)
	}
	cfg.SystemRoot = filepath.Join(alias, cfg.OwnerID)
	_, err = f.OpenOwner(context.Background(), cfg, contentBase, alias)
	requireCode(t, err, "invalid_config")
}

func TestOpenOwnerMissingCanceledAndFailureRelease(t *testing.T) {
	f, cfg, contentBase, systemBase := ownerFixture(t)
	missing := cfg
	missing.OwnerID = "absent"
	missing.ContentRoot = filepath.Join(contentBase, missing.OwnerID)
	missing.SystemRoot = filepath.Join(systemBase, missing.OwnerID)
	_, err := f.OpenOwner(context.Background(), missing, contentBase, systemBase)
	requireCode(t, err, "not_found")
	if _, err := os.Stat(missing.ContentRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing owner directory created")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = f.OpenOwner(canceled, cfg, contentBase, systemBase)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// A pool of one must remain usable after both success and all rejection paths.
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	tree, err := f.OpenOwner(ctx, cfg, contentBase, systemBase)
	if err != nil {
		t.Fatal(err)
	}
	tree.Close()
}

func TestOpenOwnerConcurrentAliasSwaps(t *testing.T) {
	f, cfg, contentBase, systemBase := ownerFixture(t)
	held := cfg.ContentRoot + "-held"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	failures := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := os.Rename(cfg.ContentRoot, held); err != nil {
				failures <- err
				return
			}
			if err := os.Symlink("owner-b", cfg.ContentRoot); err != nil {
				failures <- err
				return
			}
			if err := os.Remove(cfg.ContentRoot); err != nil {
				failures <- err
				return
			}
			if err := os.Rename(held, cfg.ContentRoot); err != nil {
				failures <- err
				return
			}
		}
	}()
	for i := 0; i < 150 && ctx.Err() == nil; i++ {
		tree, err := f.OpenOwner(ctx, cfg, contentBase, systemBase)
		if err != nil {
			continue
		}
		result, err := tree.Open(ctx, "file", storage.ReadOptions{})
		if err == nil {
			data, readErr := io.ReadAll(result.Body)
			result.Body.Close()
			if readErr == nil && string(data) != "owner a" {
				t.Error(fmt.Sprintf("owner swap exposed %q", data))
			}
		}
		tree.Close()
	}
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if ctx.Err() != nil {
		t.Fatal("owner alias-swap test timed out")
	}
}
