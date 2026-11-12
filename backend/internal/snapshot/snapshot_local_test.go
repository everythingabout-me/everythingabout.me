//go:build darwin || linux

package snapshot

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/storage"
)

// localTree opens a real filesystem driver so a prune is verified against native state,
// not only against a fake that deletes whatever it is handed.
func localTree(t *testing.T, base, name string) *localstorage.MutationDriver {
	t.Helper()
	cfg := storage.Config{OwnerID: "owner", ConnectionID: "local-" + name, Generation: 1, ContentRoot: filepath.Join(base, name, "content"), SystemRoot: filepath.Join(base, name, "system")}
	for _, root := range []string{cfg.ContentRoot, cfg.SystemRoot} {
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := localstorage.NewFactory(localstorage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	mutation, err := localstorage.NewMutationDriver(driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		driver.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mutation.Close(); err != nil {
			t.Error(err)
		}
	})
	return mutation
}

func writeLocalFile(t *testing.T, base, name, relative, contents string) {
	t.Helper()
	native := filepath.Join(base, name, "content", filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(native), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

// A rotation must delete the snapshot it reports removed. The fake tree used to accept
// any Remove, so a prune that passed no revision looked successful while nothing was
// deleted; this walks the real driver afterwards.
func TestPruneDeletesSnapshotsThroughTheLocalDriver(t *testing.T) {
	base := t.TempDir()
	src := localTree(t, base, "src")
	writeLocalFile(t, base, "src", "Photos/2026/trip.jpg", "jpeg")
	writeLocalFile(t, base, "src", "Notes/diary.md", "text")
	if err := os.MkdirAll(filepath.Join(base, "src", "content", "Empty"), 0700); err != nil {
		t.Fatal(err)
	}
	dst := localTree(t, base, "dst")
	ctx := context.Background()
	for _, date := range []string{"2026-11-08", "2026-11-10"} {
		if _, err := Copy(ctx, src, "", dst, storage.Path(date)); err != nil {
			t.Fatalf("copy %s: %v", date, err)
		}
	}
	removed, err := Prune(ctx, dst, dst, "", 1)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "2026-11-08" {
		t.Fatalf("removed %v, want the oldest date only", removed)
	}
	for _, gone := range []string{
		"2026-11-08",
		"2026-11-08/Empty",
		"2026-11-08/Notes",
		"2026-11-08/Notes/diary.md",
		"2026-11-08/Photos",
		"2026-11-08/Photos/2026",
		"2026-11-08/Photos/2026/trip.jpg",
	} {
		if _, err := dst.Stat(ctx, storage.Path(gone)); !storage.IsCode(err, "not_found") {
			t.Fatalf("%q survived the prune: %v", gone, err)
		}
	}
	for path, want := range map[storage.Path]string{
		"2026-11-10/Notes/diary.md":       "text",
		"2026-11-10/Photos/2026/trip.jpg": "jpeg",
	} {
		read, err := dst.Open(ctx, path, storage.ReadOptions{})
		if err != nil {
			t.Fatalf("kept snapshot %q: %v", path, err)
		}
		got, readErr := io.ReadAll(read.Body)
		read.Body.Close()
		if readErr != nil || string(got) != want {
			t.Fatalf("kept snapshot %q = %q (%v), want %q", path, got, readErr, want)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "dst", "content", "2026-11-08")); !os.IsNotExist(err) {
		t.Fatalf("the pruned date still exists natively: %v", err)
	}
}

// A snapshot re-taken after its predecessor was pruned must be written again. Intent
// ids used to name only the path, so the driver still held the previous run's settled
// create receipts and answered the new create from them: the second copy either reported
// success without writing or failed with an intent conflict, instead of rebuilding the
// date the operator just pruned.
func TestCopyAfterPruneRebuildsTheSameDate(t *testing.T) {
	base := t.TempDir()
	src := localTree(t, base, "src")
	writeLocalFile(t, base, "src", "Notes/diary.md", "first")
	dst := localTree(t, base, "dst")
	ctx := context.Background()
	if _, err := Copy(ctx, src, "", dst, "2026-11-10"); err != nil {
		t.Fatalf("first copy: %v", err)
	}
	if _, err := Prune(ctx, dst, dst, "", 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	writeLocalFile(t, base, "src", "Notes/diary.md", "second")
	if _, err := Copy(ctx, src, "", dst, "2026-11-10"); err != nil {
		t.Fatalf("copy after prune: %v", err)
	}
	read, err := dst.Open(ctx, "2026-11-10/Notes/diary.md", storage.ReadOptions{})
	if err != nil {
		t.Fatalf("re-created snapshot is missing its file: %v", err)
	}
	body, readErr := io.ReadAll(read.Body)
	read.Body.Close()
	if readErr != nil || string(body) != "second" {
		t.Fatalf("re-created file = %q (%v), want %q", body, readErr, "second")
	}
}
