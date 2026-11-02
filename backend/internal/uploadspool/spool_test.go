//go:build darwin || linux

package uploadspool

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func config(base string) storage.Config {
	return storage.Config{OwnerID: "owner", ConnectionID: "connection", Generation: 1, ContentRoot: "native-content", SystemRoot: base}
}
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := New(base, config(base))
	if storage.IsCode(err, "unsupported") {
		t.Skip("filesystem does not provide admitted local durability")
	}
	if err != nil {
		info, _ := os.Lstat(base)
		t.Fatalf("New failed: %v; base=%s info=%+v euid=%d", err, base, info, os.Geteuid())
	}
	t.Cleanup(func() { s.Close() })
	return s, base
}
func accept(p Part) (Part, error) { return p, nil }
func receive(t *testing.T, s *Store, id string, number int, data string) Part {
	t.Helper()
	p, err := s.Receive(context.Background(), id, number, int64(len(data)), nil, strings.NewReader(data), accept)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func readParts(t *testing.T, s *Store, id string, parts []Part) string {
	t.Helper()
	reader, err := s.Open(context.Background(), id, parts)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
func sessionRoot(t *testing.T, s *Store, id string) *os.Root {
	t.Helper()
	root, _, err := s.session(id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}
func assertNoAttempts(t *testing.T, root *os.Root) {
	t.Helper()
	dir, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "a-") {
			t.Fatalf("retained temporary attempt: %s", e.Name())
		}
	}
}

func TestDurableReceiptsRestartAndOrderedStream(t *testing.T) {
	s, base := newStore(t)
	var first Part
	_, err := s.Receive(context.Background(), "session", 1, 5, nil, strings.NewReader("hello"), func(p Part) (Part, error) {
		// A second independently opened Store can read the exact durable proposed
		// receipt from inside the callback, before the API acknowledges it.
		other, err := New(base, config(base))
		if err != nil {
			return Part{}, err
		}
		defer other.Close()
		reader, err := other.Open(context.Background(), "session", []Part{p})
		if err != nil {
			return Part{}, err
		}
		data, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return Part{}, err
		}
		if string(data) != "hello" {
			return Part{}, ErrIntegrity
		}
		first = p
		return p, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second := receive(t, s, "session", 2, " world")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(base, config(base))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := readParts(t, reopened, "session", []Part{first, second}); got != "hello world" {
		t.Fatalf("stream = %q", got)
	}
	replay := receive(t, reopened, "session", 1, "hello")
	if replay != first {
		t.Fatalf("replay receipt changed: %#v != %#v", replay, first)
	}
	root := sessionRoot(t, reopened, "session")
	assertNoAttempts(t, root)
}

func TestReplayHashesDigestAndExactSize(t *testing.T) {
	s, _ := newStore(t)
	original := receive(t, s, "session", 1, "first")
	tests := []struct {
		name, data string
		size       int64
		digest     *storage.Digest
		want       error
	}{
		{"different digestless replay", "other", 5, nil, ErrConflict},
		{"short", "tiny", 5, nil, ErrIntegrity},
		{"long", "longer", 5, nil, ErrIntegrity},
		{"digest mismatch", "first", 5, &storage.Digest{Algorithm: "sha-256", Value: make([]byte, 32)}, ErrIntegrity},
		{"unsupported digest", "first", 5, &storage.Digest{Algorithm: "md5", Value: make([]byte, 16)}, ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			_, err := s.Receive(context.Background(), "session", 1, tt.size, tt.digest, strings.NewReader(tt.data), func(p Part) (Part, error) { called = true; return p, nil })
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v; want %v", err, tt.want)
			}
			if called {
				t.Fatal("accepted invalid replay")
			}
		})
	}
	hash := sha256.Sum256([]byte("first"))
	p, err := s.Receive(context.Background(), "session", 1, 5, &storage.Digest{Algorithm: "sha-256", Value: hash[:]}, strings.NewReader("first"), accept)
	if err != nil || p != original {
		t.Fatalf("verified replay = %#v, %v", p, err)
	}
	if got := readParts(t, s, "session", []Part{original}); got != "first" {
		t.Fatalf("original bytes changed: %q", got)
	}
	assertNoAttempts(t, sessionRoot(t, s, "session"))
}

func TestConcurrentSameAndDifferentPayloads(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprint(same), func(t *testing.T) {
			s, base := newStore(t)
			other, err := New(base, config(base))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			stores := []*Store{s, other, s, other}
			start := make(chan struct{})
			results := make(chan struct {
				p   Part
				err error
			}, 4)
			for i, store := range stores {
				go func(i int, store *Store) {
					<-start
					data := "same"
					if !same {
						data = fmt.Sprintf("%04d", i)
					}
					p, err := store.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader(data), accept)
					results <- struct {
						p   Part
						err error
					}{p, err}
				}(i, store)
			}
			close(start)
			var winner Part
			success := 0
			for range stores {
				select {
				case r := <-results:
					if r.err == nil {
						success++
						if winner == (Part{}) {
							winner = r.p
						}
						if r.p != winner {
							t.Fatal("two distinct part receipts were accepted")
						}
					} else if !errors.Is(r.err, ErrConflict) {
						t.Fatal(r.err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("concurrent receives deadlocked")
				}
			}
			if same && success != 4 {
				t.Fatalf("same payload successes = %d", success)
			}
			if !same && success != 1 {
				t.Fatalf("conflicting payload successes = %d", success)
			}
			assertNoAttempts(t, sessionRoot(t, s, "session"))
		})
	}
}

func TestReceiptRejectionAndLostACK(t *testing.T) {
	s, base := newStore(t)
	dbError := errors.New("database response lost")
	_, err := s.Receive(context.Background(), "rejected", 1, 3, nil, strings.NewReader("one"), func(Part) (Part, error) { return Part{}, Reject(dbError) })
	if !errors.Is(err, dbError) {
		t.Fatal(err)
	}
	// A definitive rejection releases only its own candidate, so another payload fits.
	accepted := receive(t, s, "rejected", 1, "two")
	_, err = s.Receive(context.Background(), "rejected", 1, 3, nil, strings.NewReader("two"), func(Part) (Part, error) { return Part{}, Reject(dbError) })
	if !errors.Is(err, dbError) {
		t.Fatal(err)
	}
	if got := readParts(t, s, "rejected", []Part{accepted}); got != "two" {
		t.Fatal(got)
	}
	var persisted Part
	_, err = s.Receive(context.Background(), "uncertain", 1, 3, nil, strings.NewReader("one"), func(p Part) (Part, error) { persisted = p; return Part{}, dbError })
	if !errors.Is(err, dbError) {
		t.Fatal(err)
	}
	other, err := New(base, config(base))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if got := readParts(t, other, "uncertain", []Part{persisted}); got != "one" {
		t.Fatal(got)
	}
	for i := 0; i < 12; i++ {
		_, err := other.Receive(context.Background(), "uncertain", 1, 3, nil, strings.NewReader(fmt.Sprintf("%03d", i)), accept)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("uncertain differing retry = %v", err)
		}
	}
	root := sessionRoot(t, s, "uncertain")
	assertNoAttempts(t, root)
	dir, _ := root.Open(".")
	entries, _ := dir.ReadDir(-1)
	dir.Close()
	count := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "p-") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("retained canonical chunks = %d", count)
	}
	replay := receive(t, other, "uncertain", 1, "one")
	if replay != persisted {
		t.Fatalf("lost ACK replay changed receipt: %#v", replay)
	}
}

func TestRejectInvalidCallbackReceipt(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("data"), func(p Part) (Part, error) { p.Handle = "different"; return p, nil })
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("mismatched callback result = %v", err)
	}
	if _, err := s.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("else"), accept); !errors.Is(err, ErrConflict) {
		t.Fatalf("callback mismatch discarded uncertain bytes: %v", err)
	}
}

type blockedBody struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (b *blockedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.closed
	return 0, io.ErrClosedPipe
}
func (b *blockedBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}
func newBlocked() *blockedBody {
	return &blockedBody{started: make(chan struct{}), closed: make(chan struct{})}
}

func TestCancelClosesRequestBodyAndCleansAttempt(t *testing.T) {
	for _, closeStore := range []bool{false, true} {
		t.Run(fmt.Sprint(closeStore), func(t *testing.T) {
			s, base := newStore(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := newBlocked()
			result := make(chan error, 1)
			go func() { _, err := s.Receive(ctx, "session", 1, 4, nil, body, accept); result <- err }()
			select {
			case <-body.started:
			case <-time.After(5 * time.Second):
				t.Fatal("body was not read")
			}
			if closeStore {
				s.Close()
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel result = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation did not close body")
			}
			other, err := New(base, config(base))
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			assertNoAttempts(t, sessionRoot(t, other, "session"))
		})
	}
}

func TestAbortFencesLateReceiveAndRetainsReaderLock(t *testing.T) {
	s, _ := newStore(t)
	part := receive(t, s, "session", 1, "safe")
	reader, err := s.Open(context.Background(), "session", []Part{part})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	root := sessionRoot(t, s, "session")
	before, err := root.Stat("session.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(context.Background(), "session"); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard active reader = %v", err)
	}
	if got, err := io.ReadAll(reader); err != nil || string(got) != "safe" {
		t.Fatalf("protected stream = %q, %v", got, err)
	}
	if _, err := s.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("safe"), accept); !errors.Is(err, ErrDiscarded) {
		t.Fatalf("late receive = %v", err)
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatalf("discard not idempotent: %v", err)
	}
	after, err := root.Stat("session.lock")
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("persistent lock replaced: %v", err)
	}
	if _, err := root.Stat(partName(part.Number, part.Size, part.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical bytes survived cleanup: %v", err)
	}
}

func TestAbortWhileBodyInFlight(t *testing.T) {
	s, _ := newStore(t)
	body := newBlocked()
	result := make(chan error, 1)
	go func() { _, err := s.Receive(context.Background(), "session", 1, 4, nil, body, accept); result <- err }()
	select {
	case <-body.started:
	case <-time.After(5 * time.Second):
		t.Fatal("body was not read")
	}
	if err := s.Discard(context.Background(), "session"); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard active body = %v", err)
	}
	body.Close()
	select {
	case <-result:
	case <-time.After(5 * time.Second):
		t.Fatal("receive did not end")
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("late"), accept); !errors.Is(err, ErrDiscarded) {
		t.Fatalf("late receive = %v", err)
	}
}

func TestAbortCallbackGateIsNonblocking(t *testing.T) {
	s, _ := newStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := s.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("data"), func(Part) (Part, error) { close(entered); <-release; return Part{}, Reject(ErrDiscarded) })
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback was not entered")
	}
	if err := s.Discard(context.Background(), "session"); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard callback = %v", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, ErrDiscarded) {
		t.Fatalf("callback result = %v", err)
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
}

func TestStreamCancellationReleasesSessionLock(t *testing.T) {
	s, _ := newStore(t)
	p := receive(t, s, "session", 1, "data")
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := s.Open(ctx, "session", []Part{p})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("stream cancellation = %v", err)
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	reader.Close()
}

func TestCorruptMissingAndSubstitutedBytesFail(t *testing.T) {
	for _, mode := range []string{"corrupt", "missing", "symlink", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := newStore(t)
			p := receive(t, s, "session", 1, "good")
			root := sessionRoot(t, s, "session")
			name := partName(p.Number, p.Size, p.SHA256)
			switch mode {
			case "corrupt":
				file, err := root.OpenFile(name, os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.Write([]byte("evil"))
				file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := root.Remove(name); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := root.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("session.json", name); err != nil {
					t.Fatal(err)
				}
			case "metadata":
				file, err := root.OpenFile(recordName(1), os.O_WRONLY|os.O_TRUNC, 0)
				if err != nil {
					t.Fatal(err)
				}
				file.Write([]byte("{}"))
				file.Close()
			}
			reader, err := s.Open(context.Background(), "session", []Part{p})
			if err == nil {
				_, err = io.ReadAll(reader)
				reader.Close()
			}
			if !errors.Is(err, ErrIntegrity) {
				t.Fatalf("bad stream = %v", err)
			}
			if mode == "corrupt" {
				_, err := s.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("good"), accept)
				if !errors.Is(err, ErrIntegrity) {
					t.Fatalf("replay overwrote corruption: %v", err)
				}
			}
		})
	}
}

func TestIsolationManifestValidationAndPrivatePermissions(t *testing.T) {
	s, base := newStore(t)
	p := receive(t, s, "../../session", 1, "data")
	if got := readParts(t, s, "../../session", []Part{p}); got != "data" {
		t.Fatal(got)
	}
	for _, parts := range [][]Part{{p, p}, {{Number: 2, Size: p.Size, SHA256: p.SHA256, Handle: p.Handle}}, make([]Part, MaxParts+1)} {
		if reader, err := s.Open(context.Background(), "../../session", parts); !errors.Is(err, ErrInvalid) {
			if reader != nil {
				reader.Close()
			}
			t.Fatalf("invalid manifest = %v", err)
		}
	}
	if reader, err := s.Open(context.Background(), "other", []Part{p}); !errors.Is(err, ErrInvalid) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("cross-session receipt = %v", err)
	}
	for _, change := range []func(*storage.Config){func(c *storage.Config) { c.OwnerID = "other" }, func(c *storage.Config) { c.Generation++ }, func(c *storage.Config) { c.ConnectionID = "other" }, func(c *storage.Config) { c.ContentRoot = "other" }, func(c *storage.Config) { c.SystemRoot = "other" }} {
		cfg := config(base)
		change(&cfg)
		other, err := New(base, cfg)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := other.Open(context.Background(), "../../session", []Part{p})
		other.Close()
		if !errors.Is(err, ErrInvalid) {
			if reader != nil {
				reader.Close()
			}
			t.Fatalf("cross-binding receipt = %v", err)
		}
	}
	root := sessionRoot(t, s, "../../session")
	info, err := root.Stat(".")
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("session permissions = %v, %v", info, err)
	}
	file, err := readFile(root, partName(p.Number, p.Size, p.SHA256))
	if err != nil {
		t.Fatal(err)
	}
	info, err = file.Stat()
	file.Close()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("chunk permissions = %v, %v", info, err)
	}
	if got := readParts(t, s, "empty", nil); got != "" {
		t.Fatalf("empty upload = %q", got)
	}
}

func TestSymlinkNamespaceAndUnknownCleanup(t *testing.T) {
	s, base := newStore(t)
	outside := t.TempDir()
	link := filepath.Join(t.TempDir(), "base-link")
	if err := os.Symlink(base, link); err != nil {
		t.Fatal(err)
	}
	if store, err := New(link, config(base)); err == nil {
		store.Close()
		t.Fatal("symlink base admitted")
	}
	key, _ := sessionKey("linked")
	if err := s.root.Symlink(outside, "s-"+key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Receive(context.Background(), "linked", 1, 4, nil, strings.NewReader("data"), accept); err == nil {
		t.Fatal("symlink session admitted")
	}
	for _, lockName := range []string{"session.lock", "slot-0.lock", "gate.lock"} {
		id := "lock-" + lockName
		root := sessionRoot(t, s, id)
		if err := root.Symlink(filepath.Join(outside, "lock"), lockName); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Receive(context.Background(), id, 1, 4, nil, strings.NewReader("data"), accept); err == nil {
			t.Fatalf("symlink lock admitted: %s", lockName)
		}
	}
	p := receive(t, s, "cleanup", 1, "data")
	root := sessionRoot(t, s, "cleanup")
	if err := immutable(root, "unknown", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	unrecorded := partName(2, 4, strings.Repeat("0", 64))
	if err := immutable(root, unrecorded, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := root.Mkdir("a-"+strings.Repeat("f", 32), 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(context.Background(), "cleanup"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("unknown attempt cleanup = %v", err)
	}
	for _, name := range []string{"unknown", unrecorded, "a-" + strings.Repeat("f", 32)} {
		if _, err := root.Lstat(name); err != nil {
			t.Fatalf("unknown entry removed: %s: %v", name, err)
		}
	}
	if _, err := root.Lstat(partName(p.Number, p.Size, p.SHA256)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recognized part retained: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v, %v", entries, err)
	}
}

func TestRestartReclaimsOnlyRecordedSlotAttempt(t *testing.T) {
	s, base := newStore(t)
	root := sessionRoot(t, s, "session")
	attempt, id, err := newAttempt(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	file, err := createFile(attempt, "data")
	if err != nil {
		t.Fatal(err)
	}
	file.Write(bytes.Repeat([]byte("x"), 1024))
	file.Sync()
	file.Close()
	syncDirectory(attempt)
	attempt.Close()
	s.Close()
	other, err := New(base, config(base))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	receive(t, other, "session", 1, "next")
	if _, err := root.Lstat(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale attempt survived slot reuse: %v", err)
	}
}

// Subprocesses use only their test executable, local temporary base and stdin;
// this verifies flock admission across process boundaries without a database.
func TestProcessSlotHelper(t *testing.T) {
	args := os.Args
	if len(args) < 3 || args[len(args)-2] != "uploadspool-child" {
		return
	}
	base := args[len(args)-1]
	s, err := New(base, config(base))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	body := &notifyReader{reader: os.Stdin}
	if _, err := s.Receive(context.Background(), "process", 1, 1, nil, body, accept); err != nil {
		t.Fatal(err)
	}
}

type notifyReader struct {
	reader io.Reader
	once   sync.Once
}

func (r *notifyReader) Read(p []byte) (int, error) {
	r.once.Do(func() { fmt.Fprintln(os.Stdout, "ready") })
	return r.reader.Read(p)
}
func TestCrossProcessFourReceiveSlots(t *testing.T) {
	s, base := newStore(t)
	type child struct {
		cmd   *exec.Cmd
		stdin io.WriteCloser
	}
	children := []child{}
	defer func() {
		for _, child := range children {
			child.stdin.Close()
			_ = child.cmd.Wait()
		}
	}()
	for i := 0; i < 4; i++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestProcessSlotHelper$", "--", "uploadspool-child", base)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, child{cmd, stdin})
		ready := make(chan error, 1)
		go func() {
			var b [6]byte
			_, err := io.ReadFull(stdout, b[:])
			if err == nil && string(b[:]) != "ready\n" {
				err = fmt.Errorf("unexpected child readiness: %q", b)
			}
			ready <- err
		}()
		select {
		case err := <-ready:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("child did not acquire receive slot")
		}
	}
	called := false
	_, err := s.Receive(context.Background(), "process", 1, 1, nil, strings.NewReader("x"), func(p Part) (Part, error) { called = true; return p, nil })
	if !errors.Is(err, ErrBusy) || called {
		t.Fatalf("fifth receive = %v, called=%v", err, called)
	}
	for _, child := range children {
		if _, err := child.stdin.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		child.stdin.Close()
	}
	for _, child := range children {
		if err := child.cmd.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	children = nil
	p := receive(t, s, "process", 1, "x")
	if got := readParts(t, s, "process", []Part{p}); got != "x" {
		t.Fatal(got)
	}
}
