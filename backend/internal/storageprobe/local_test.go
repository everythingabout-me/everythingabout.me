package storageprobe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func writeFixture(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, name, want string) {
	t.Helper()
	got, err := os.ReadFile(name)
	if err != nil || string(got) != want {
		t.Fatalf("native bytes = %q, %v; want %q", got, err, want)
	}
}

func unixVisibility(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("same-filesystem Unix visibility experiment unverified on this OS")
	}
}

func TestLocalAtomicNoReplace(t *testing.T) {
	unixVisibility(t)
	root := t.TempDir()
	target := filepath.Join(root, "final.txt")
	// link(2) publishes a complete staged inode only if the destination is absent.
	// A future adapter must unlink the stage and recover ambiguous unlink results;
	// this is single-file publication, not a mutable hardlink implementation of copy.
	const contenders = 12
	var wg sync.WaitGroup
	results := make(chan error, contenders)
	start := make(chan struct{})
	for i := range contenders {
		stage := filepath.Join(root, fmt.Sprintf("stage-%d", i))
		writeFixture(t, stage, fmt.Sprintf("complete-%d", i))
		wg.Go(func() {
			<-start
			results <- os.Link(stage, target)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, os.ErrExist) {
			t.Fatalf("link no-replace unavailable on fixture filesystem: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("atomic no-replace winners = %d, want exactly one", wins)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(root, "late-stage")
	writeFixture(t, stage, "must-not-replace")
	if err := os.Link(stage, target); !errors.Is(err, os.ErrExist) {
		t.Fatalf("occupied destination must reject publication: %v", err)
	}
	readFixture(t, target, string(before))
	t.Log("observed native link no-replace on the temporary filesystem; not power-loss durability or version CAS")
}

func TestLocalAtomicReplaceVisibility(t *testing.T) {
	unixVisibility(t)
	root := t.TempDir()
	target := filepath.Join(root, "final.txt")
	a, b := bytes.Repeat([]byte("a"), 4096), bytes.Repeat([]byte("b"), 4096)
	writeFixture(t, target, string(a))
	stop, result := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			got, err := os.ReadFile(target)
			if err != nil || (!bytes.Equal(got, a) && !bytes.Equal(got, b)) {
				result <- fmt.Errorf("reader observed missing/partial generation: len=%d err=%v", len(got), err)
				return
			}
			select {
			case <-stop:
				result <- nil
				return
			default:
			}
		}
	}()
	for i := range 64 {
		stage := filepath.Join(root, fmt.Sprintf("stage-%d", i))
		data := a
		if i%2 == 0 {
			data = b
		}
		writeFixture(t, stage, string(data))
		if err := os.Rename(stage, target); err != nil {
			close(stop)
			<-result
			t.Fatal(err)
		}
	}
	close(stop)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	readFixture(t, target, string(a))
	t.Log("observed complete old/new bytes across same-filesystem rename; no crash/fsync or cross-volume guarantee")
}

func TestLocalNativeExitNamesRangesAndCopy(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "Photos", "Empty")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	name := " Spelling +#?% 中文.txt "
	source := filepath.Join(root, "Photos", name)
	writeFixture(t, source, "0123456789")
	writeFixture(t, filepath.Join(root, "zero"), "")
	entries, err := os.ReadDir(filepath.Dir(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != name || entries[1].Name() != "Empty" {
		t.Fatalf("native spelling/folder entries = %v", entries)
	}
	children, err := os.ReadDir(folder)
	if err != nil || len(children) != 0 {
		t.Fatalf("empty native folder = %v, %v", children, err)
	}
	f, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, tc := range []struct {
		offset, length int64
		want           string
		wantErr        error
	}{{2, 4, "2345", nil}, {9, 1, "9", nil}, {10, 1, "", io.EOF}, {9, 3, "9", io.ErrUnexpectedEOF}} {
		buf := make([]byte, tc.length)
		n, err := io.ReadFull(io.NewSectionReader(f, tc.offset, tc.length), buf)
		if string(buf[:n]) != tc.want || !errors.Is(err, tc.wantErr) {
			t.Fatalf("range offset=%d length=%d: %q, %v", tc.offset, tc.length, buf[:n], err)
		}
	}
	// Ordinary copied bytes survive both an in-place source edit and source removal.
	copyPath := filepath.Join(root, "copy.txt")
	out, err := os.OpenFile(copyPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, io.NewSectionReader(f, 0, 10))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy: %v, close: %v", copyErr, closeErr)
	}
	writeFixture(t, source, "changed")
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	readFixture(t, copyPath, "0123456789")
	readFixture(t, filepath.Join(root, "zero"), "")
	// No application configuration, index, decoder, or database participates above.
}

func TestLocalRootIsolation(t *testing.T) {
	unixVisibility(t)
	root, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "outside-file")
	writeFixture(t, secret, "other-root")
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// Plain joined filesystem calls follow an external symlink. Path string
	// validation alone does not confine a root under arbitrary native editors.
	readFixture(t, filepath.Join(link, "outside-file"), "other-root")
	bound, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer bound.Close()
	if f, err := bound.Open("escape/outside-file"); err == nil {
		f.Close()
		t.Fatal("root-bound native open escaped via symlink")
	}
	if f, err := bound.Open("../" + filepath.Base(outside) + "/outside-file"); err == nil {
		f.Close()
		t.Fatal("root-bound native open escaped via dot-dot")
	}
	if err := bound.WriteFile("inside", []byte("own-root"), 0600); err != nil {
		t.Fatal(err)
	}
	inside, err := bound.ReadFile("inside")
	if err != nil || string(inside) != "own-root" {
		t.Fatalf("bound native positive: %q, %v", inside, err)
	}
	readFixture(t, secret, "other-root")
	t.Log("os.Root confined static symlink/dot-dot cases; full adapter race, mount and mutation admission remain unverified")
}

func TestLocalCaseAndNormalizationLimits(t *testing.T) {
	root := t.TempDir()
	for _, pair := range [][2]string{{"Case", "case"}, {"caf\u00e9", "cafe\u0301"}} {
		a, b := filepath.Join(root, pair[0]), filepath.Join(root, pair[1])
		writeFixture(t, a, "first")
		f, err := os.OpenFile(b, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			readFixture(t, b, "first")
			t.Logf("native name collision: %q and %q; adapter must reject ambiguous accepted names", pair[0], pair[1])
		} else if err != nil {
			t.Fatal(err)
		} else {
			if _, err := f.WriteString("second"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			readFixture(t, a, "first")
			readFixture(t, b, "second")
			t.Logf("distinct native names: %q and %q", pair[0], pair[1])
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Logf("native stored spelling: %q", entry.Name())
	}
}

func TestLocalReaderAndVersionLimits(t *testing.T) {
	unixVisibility(t)
	root := t.TempDir()
	target, stage := filepath.Join(root, "final"), filepath.Join(root, "stage")
	writeFixture(t, target, "old")
	old, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	writeFixture(t, stage, "new")
	if err := os.Rename(stage, target); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := old.ReadAt(buf, 0); err != nil || string(buf) != "old" {
		t.Fatalf("open descriptor did not retain replaced inode: %q, %v", buf, err)
	}
	readFixture(t, target, "new")
	// An open descriptor pins an inode, not immutable bytes against external edits.
	if _, err := old.WriteAt([]byte("BAD"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := old.ReadAt(buf, 0); err != nil || string(buf) != "BAD" {
		t.Fatalf("expected negative evidence for immutable descriptor reads: %q, %v", buf, err)
	}
	readFixture(t, target, "new")
	t.Log("descriptor survives rename; arbitrary in-place writers defeat immutable version reads")
}

func TestLocalDelayedStalePublishAndLostReply(t *testing.T) {
	unixVisibility(t)
	root := t.TempDir()
	target, late := filepath.Join(root, "final"), filepath.Join(root, "late")
	writeFixture(t, target, "baseline")
	writeFixture(t, late, "stale")
	// A channel deterministically models a paused worker; no timing assumption.
	release, done := make(chan struct{}), make(chan error, 1)
	go func() { <-release; done <- os.Rename(late, target) }()
	fresh := filepath.Join(root, "fresh")
	writeFixture(t, fresh, "newer")
	if err := os.Rename(fresh, target); err != nil {
		t.Fatal(err)
	}
	readFixture(t, target, "newer")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	readFixture(t, target, "stale")
	// Fault injection after a real native effect: an error is not proof of rollback.
	stage := filepath.Join(root, "lost-reply")
	writeFixture(t, stage, "applied")
	publishWithLostReply := func() error {
		if err := os.Rename(stage, target); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if err := publishWithLostReply(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost-reply injection = %v", err)
	}
	readFixture(t, target, "applied")
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("applied rename left stage: %v", err)
	}
	t.Log("negative native/fixture evidence: rename is not generation CAS; lease expiry/error cannot release an unresolved guard")
}
