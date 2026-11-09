//go:build darwin || linux

package localstorage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestMutationListScansSiblingsOncePerPage(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	// More than two maximum pages catches repeated per-child directory scans.
	const total = 2500
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("entry-%04d", i)
		if i%10 == 0 {
			if err := os.Mkdir(filepath.Join(cfg.ContentRoot, name), 0700); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(cfg.ContentRoot, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	scans, visited := 0, 0
	scan := w.listScan
	w.listScan = func(ctx context.Context, root *os.Root, path storage.Path) ([]nativeEntry, error) {
		scans++
		entries, err := scan(ctx, root, path)
		visited += len(entries)
		return entries, err
	}
	var cursor string
	seen, pages := 0, 0
	for {
		page, err := w.List(context.Background(), "", storage.PageRequest{Limit: 1000, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		expected := min(1000, total-seen)
		if len(page.Entries) != expected {
			t.Fatalf("page count=%d want=%d", len(page.Entries), expected)
		}
		for _, e := range page.Entries {
			expectedPath := storage.Path(fmt.Sprintf("entry-%04d", seen))
			if e.Path != expectedPath || len(e.Revision) == 0 || len(e.ContentIdentity) == 0 {
				t.Fatalf("bad enriched entry %+v", e)
			}
			if e.Kind == storage.File && (e.Digest == nil || e.Digest.Algorithm != "sha-256") {
				t.Fatal("missing verified file digest")
			}
			seen++
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	if seen != total || pages != 3 || scans != pages || visited != total*pages {
		t.Fatalf("seen=%d pages=%d scans=%d visited=%d", seen, pages, scans, visited)
	}
}

func TestMutationListRejectsNativeSwapAfterScan(t *testing.T) {
	for _, mode := range []string{"file-inode", "folder-symlink"} {
		t.Run(mode, func(t *testing.T) {
			w, _, cfg := mutationFixture(t)
			name := "entry"
			path := filepath.Join(cfg.ContentRoot, name)
			if mode == "folder-symlink" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			scan := w.listScan
			w.listScan = func(ctx context.Context, root *os.Root, path storage.Path) ([]nativeEntry, error) {
				entries, err := scan(ctx, root, path)
				if err != nil {
					return nil, err
				}
				if err := root.Rename(name, "retained"); err != nil {
					return nil, err
				}
				if mode == "folder-symlink" {
					err = os.Symlink(cfg.SystemRoot, filepath.Join(cfg.ContentRoot, name))
				} else {
					err = os.WriteFile(filepath.Join(cfg.ContentRoot, name), []byte("old"), 0600)
				}
				return entries, err
			}
			_, err := w.List(context.Background(), "", storage.PageRequest{})
			requireCode(t, err, "changed")
		})
	}
}
