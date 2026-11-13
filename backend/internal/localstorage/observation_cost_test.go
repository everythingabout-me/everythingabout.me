//go:build darwin || linux

package localstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

// localFile writes one regular file of the requested size below the bound
// content root. The bytes are a repeating pattern so the fixture itself is
// cheap; what is measured is the driver, not the fixture.
func localFile(tb testing.TB, root string, size int64) {
	tb.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		tb.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, "data.bin"))
	if err != nil {
		tb.Fatal(err)
	}
	block := make([]byte, 64<<10)
	for i := range block {
		block[i] = byte(i)
	}
	for written := int64(0); written < size; {
		n := int64(len(block))
		if size-written < n {
			n = size - written
		}
		if _, err := f.Write(block[:n]); err != nil {
			f.Close()
			tb.Fatal(err)
		}
		written += n
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
}

func localCostDriver(tb testing.TB, size int64) (*MutationDriver, string) {
	tb.Helper()
	base := tb.TempDir()
	cfg := storage.Config{OwnerID: "owner-a", ConnectionID: "local-cost", Generation: 1, ContentRoot: filepath.Join(base, "content"), SystemRoot: filepath.Join(base, "system")}
	for _, root := range []string{cfg.ContentRoot, cfg.SystemRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			tb.Fatal(err)
		}
	}
	factory, err := NewFactory(Options{})
	if err != nil {
		tb.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { driver.Close() })
	w, err := NewMutationDriver(driver, WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		tb.Fatal(err)
	}
	localFile(tb, cfg.ContentRoot, size)
	return w, cfg.ContentRoot
}

// TestObservationFileBytes reports the file bytes one Stat and one ranged Open
// consume. A returned digest is proof the whole file was read: it cannot be
// computed from metadata, and it must equal the digest of the file's contents.
func TestObservationFileBytes(t *testing.T) {
	ctx := context.Background()
	for _, size := range []int64{4 << 10, 1 << 20, 16 << 20} {
		w, root := localCostDriver(t, size)
		path := storage.Path("data.bin")
		started := time.Now()
		entry, err := w.Stat(ctx, path)
		statCost := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		length := int64(1024)
		started = time.Now()
		read, err := w.Open(ctx, path, storage.ReadOptions{Offset: 0, Length: &length})
		openCost := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, length)
		if _, err := read.Body.Read(buf); err != nil {
			t.Fatal(err)
		}
		read.Body.Close()
		digest := "none"
		if entry.Digest != nil {
			digest = entry.Digest.Algorithm
			want, err := os.ReadFile(filepath.Join(root, "data.bin"))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(want)
			if hex.EncodeToString(entry.Digest.Value) != hex.EncodeToString(sum[:]) {
				t.Fatalf("Stat digest does not cover the file's contents")
			}
			digest += " of full contents"
		}
		t.Logf("file=%9d bytes  Stat=%8s  Open(range 1024)=%8s  Entry.Digest=%s  Entry.Revision=%dB",
			size, statCost, openCost, digest, len(entry.Revision))
	}
}

// BenchmarkObservationStat measures the cost of observing one file. ns/op is the
// observation. MB/s is deliberately computed from the file size: a flat ns/op next
// to a rising MB/s is what "this does not read the file" looks like, while a version
// that does read it sits at hash speed (~2.5 GB/s here) with ns/op proportional to
// the size.
func BenchmarkObservationStat(b *testing.B) {
	for _, size := range []int64{64 << 10, 1 << 20, 16 << 20} {
		w, _ := localCostDriver(b, size)
		ctx := context.Background()
		b.Run(sizeName(size), func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := w.Stat(ctx, storage.Path("data.bin")); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkObservationOpenRange measures what one 1 KiB ranged Open costs to
// start on a file far larger than the range it serves.
func BenchmarkObservationOpenRange(b *testing.B) {
	for _, size := range []int64{1 << 20, 16 << 20, 256 << 20} {
		w, _ := localCostDriver(b, size)
		ctx := context.Background()
		length := int64(1024)
		b.Run(sizeName(size), func(b *testing.B) {
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				read, err := w.Open(ctx, storage.Path("data.bin"), storage.ReadOptions{Length: &length})
				if err != nil {
					b.Fatal(err)
				}
				read.Body.Close()
			}
		})
	}
}

// TestObservationDoesNotReadFileBytes proves the observation reads no bytes, not
// even by accident: the file is made unreadable to its owner, so any open-for-read
// on the observation path fails, while stat and directory reads still work. Stat and
// List must therefore succeed. A hash-based revision fails here with
// permission_denied before it can answer.
func TestObservationDoesNotReadFileBytes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based proof does not hold for root")
	}
	w, root := localCostDriver(t, 1<<20)
	if err := os.Chmod(filepath.Join(root, "data.bin"), 0); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := w.Stat(ctx, storage.Path("data.bin")); err != nil {
		t.Fatalf("Stat of a metadata-observable file failed: %v", err)
	}
	page, err := w.List(ctx, "", storage.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("List of a folder whose file is unreadable failed: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Path != "data.bin" {
		t.Fatalf("List = %+v", page.Entries)
	}
}

func sizeName(size int64) string {
	if size%(1<<20) == 0 {
		return itoa(size>>20) + "MiB"
	}
	return itoa(size>>10) + "KiB"
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}

// BenchmarkObservationListFolder measures what listing one folder costs. A
// reconcile pass lists every folder, so any per-entry byte work becomes a full
// read of the folder.
func BenchmarkObservationListFolder(b *testing.B) {
	base := b.TempDir()
	cfg := storage.Config{OwnerID: "owner-a", ConnectionID: "local-cost", Generation: 1, ContentRoot: filepath.Join(base, "content"), SystemRoot: filepath.Join(base, "system")}
	for _, root := range []string{cfg.ContentRoot, cfg.SystemRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			b.Fatal(err)
		}
	}
	factory, err := NewFactory(Options{})
	if err != nil {
		b.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { driver.Close() })
	w, err := NewMutationDriver(driver, WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		b.Fatal(err)
	}
	const files, size = 16, int64(1 << 20)
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(cfg.ContentRoot, "file-"+itoa(int64(i))), make([]byte, size), 0600); err != nil {
			b.Fatal(err)
		}
	}
	ctx := context.Background()
	b.SetBytes(files * size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.List(ctx, "", storage.PageRequest{Limit: 100}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestObservationScalesWithDirectoryNotFile reports what a Local observation still
// costs: the parent directory is enumerated to prove the exact native spelling of
// one name on a case-insensitive filesystem, so a Stat in a folder of 5000 siblings
// does 5000 metadata reads. That is a property of the name comparison, not of the
// file: the same observation of the same file is unchanged by the file's size.
func TestObservationScalesWithDirectoryNotFile(t *testing.T) {
	base := t.TempDir()
	cfg := storage.Config{OwnerID: "owner-a", ConnectionID: "local-cost", Generation: 1, ContentRoot: filepath.Join(base, "content"), SystemRoot: filepath.Join(base, "system")}
	for _, root := range []string{cfg.ContentRoot, cfg.SystemRoot} {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	factory, err := NewFactory(Options{})
	if err != nil {
		t.Fatal(err)
	}
	driver, err := factory.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { driver.Close() })
	w, err := NewMutationDriver(driver, WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, siblings := range []int{1, 1000, 5000} {
		dir := filepath.Join(cfg.ContentRoot, "dir-"+itoa(int64(siblings)))
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < siblings; i++ {
			if err := os.WriteFile(filepath.Join(dir, "sibling-"+itoa(int64(i))), []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		path := storage.Path("dir-" + itoa(int64(siblings)) + "/sibling-0")
		started := time.Now()
		for i := 0; i < 10; i++ {
			if _, err := w.Stat(ctx, path); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("siblings=%4d  Stat(4-byte file)=%s", siblings, time.Since(started)/10)
	}
}

// TestRevisionFollowsNativeStateWithoutReadingBytes pins what the cheap revision
// still promises, and exactly where it stops. It replaced a content digest, so the
// two facts that mattered are checked here: a rename does not version the file (a
// move must preserve the revision), and a rewrite does. The last case is the
// documented gap — a rewrite that keeps the inode, the size and the mtime — and it
// is asserted so that changing the derivation has to be a deliberate act that also
// updates nativeVersion's comment.
func TestRevisionFollowsNativeStateWithoutReadingBytes(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	path := filepath.Join(cfg.ContentRoot, "file")
	if err := os.WriteFile(path, []byte("first bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	first := mutationStat(t, w, "file")

	renamed := filepath.Join(cfg.ContentRoot, "moved")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	moved := mutationStat(t, w, "moved")
	if !bytes.Equal(first.Revision, moved.Revision) || !bytes.Equal(first.ContentIdentity, moved.ContentIdentity) {
		t.Fatal("rename changed the native state a move must preserve")
	}

	// Same length, different bytes: the mtime moves, so the state moved.
	if err := os.WriteFile(renamed, []byte("other bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	rewritten := mutationStat(t, w, "moved")
	if bytes.Equal(rewritten.Revision, moved.Revision) {
		t.Fatal("an in-place rewrite did not change the revision")
	}

	// The one unobservable rewrite: same inode, same size, mtime restored. See
	// nativeVersion for why a cheap revision cannot see it and why callers tolerate it.
	if err := os.WriteFile(renamed, []byte("different!!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(renamed, rewritten.ModifiedAt, rewritten.ModifiedAt); err != nil {
		t.Fatal(err)
	}
	if got := mutationStat(t, w, "moved"); !bytes.Equal(got.Revision, rewritten.Revision) {
		t.Fatal("nativeVersion now observes an in-place rewrite with a restored mtime; update its comment and this expectation")
	}
}
