//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package localstorage

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func TestSpecialNativeEntriesRejectedWithoutBlocking(t *testing.T) {
	for _, special := range []string{"fifo", "socket"} {
		t.Run(special, func(t *testing.T) {
			factory, cfg := fixture(t, Options{})
			writeFixture(t, cfg.ContentRoot, "a regular", "safe")
			path := filepath.Join(cfg.ContentRoot, "z special")
			if special == "fifo" {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				listener, err := net.Listen("unix", path)
				if err != nil {
					if len(path) > 100 {
						t.Skip("native Unix socket path limit is shorter than the fixture directory")
					}
					t.Fatal(err)
				}
				defer listener.Close()
			}
			tree := bind(t, factory, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			page, err := tree.List(ctx, "", storage.PageRequest{Limit: 1})
			requireCode(t, err, "unsupported")
			if len(page.Entries) != 0 || page.NextCursor != "" {
				t.Fatal("special entry yielded partial scan")
			}
			_, err = tree.Stat(ctx, "z special")
			requireCode(t, err, "unsupported")
			_, err = tree.Open(ctx, "z special", storage.ReadOptions{})
			requireCode(t, err, "unsupported")
			if special == "fifo" {
				// The opening primitive also remains nonblocking if the file becomes a FIFO
				// after metadata validation, allowing the post-open type check to reject it.
				root, err := os.OpenRoot(cfg.ContentRoot)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				done := make(chan error, 1)
				go func() {
					file, err := openReadOnly(root, "z special")
					if file != nil {
						info, statErr := file.Stat()
						file.Close()
						if statErr != nil {
							err = statErr
						} else if info.Mode()&os.ModeNamedPipe == 0 {
							err = errors.New("opened FIFO has unexpected type")
						}
					}
					done <- err
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("safe native opening blocked on a FIFO")
				}
			}
		})
	}
}

func TestConcurrentSymlinkSwapsCannotEscape(t *testing.T) {
	factory, cfg := fixture(t, Options{MaxConcurrentOperations: 4})
	writeFixture(t, cfg.ContentRoot, "victim/bytes", "safe content")
	writeFixture(t, cfg.SystemRoot, "bytes", "private system bytes")
	tree := bind(t, factory, cfg)
	victim := filepath.Join(cfg.ContentRoot, "victim")
	held := filepath.Join(cfg.ContentRoot, "victim-held")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		for i := 0; i < 400 && ctx.Err() == nil; i++ {
			if err := os.Rename(victim, held); err != nil {
				return
			}
			if err := os.Symlink(cfg.SystemRoot, victim); err != nil {
				return
			}
			if err := os.Remove(victim); err != nil {
				return
			}
			if err := os.Rename(held, victim); err != nil {
				return
			}
		}
	}()
	<-started
	for i := 0; i < 250 && ctx.Err() == nil; i++ {
		result, err := tree.Open(ctx, "victim/bytes", storage.ReadOptions{})
		if err != nil {
			continue
		}
		data, readErr := io.ReadAll(result.Body)
		result.Body.Close()
		if readErr == nil && string(data) != "safe content" {
			t.Errorf("symlink swap exposed %q", data)
			break
		}
	}
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatal("symlink-swap fixture timed out")
	}
}
