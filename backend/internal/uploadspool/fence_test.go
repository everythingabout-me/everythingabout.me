//go:build darwin || linux

package uploadspool

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type gatedReader struct {
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
	reader  io.Reader
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.proceed })
	return r.reader.Read(p)
}

func TestCompletedLateBodyCannotAcceptAfterAbort(t *testing.T) {
	s, base := newStore(t)
	body := &gatedReader{started: make(chan struct{}), proceed: make(chan struct{}), reader: strings.NewReader("late")}
	result := make(chan error, 1)
	accepted := make(chan struct{}, 1)
	go func() {
		_, err := s.Receive(context.Background(), "session", 1, 4, nil, body, func(p Part) (Part, error) { accepted <- struct{}{}; return p, nil })
		result <- err
	}()
	select {
	case <-body.started:
	case <-time.After(5 * time.Second):
		t.Fatal("body did not start")
	}
	if err := s.Discard(context.Background(), "session"); !errors.Is(err, ErrBusy) {
		t.Fatalf("discard active attempt = %v", err)
	}
	close(body.proceed)
	select {
	case err := <-result:
		if !errors.Is(err, ErrDiscarded) {
			t.Fatalf("late completed body = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late receive did not finish")
	}
	select {
	case <-accepted:
		t.Fatal("late completed body reached receipt callback")
	default:
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	other, err := New(base, config(base))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Receive(context.Background(), "session", 1, 4, nil, strings.NewReader("late"), accept); !errors.Is(err, ErrDiscarded) {
		t.Fatalf("restart resurrected aborted session: %v", err)
	}
}

func TestCancellationWhileWaitingForReceiptGate(t *testing.T) {
	s, _ := newStore(t)
	root := sessionRoot(t, s, "session")
	unlock, err := lockFile(context.Background(), root, "gate.lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	body := &gatedReader{started: make(chan struct{}), proceed: make(chan struct{}), reader: strings.NewReader("data")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := s.Receive(ctx, "session", 1, 4, nil, body, accept); result <- err }()
	select {
	case <-body.started:
	case <-time.After(5 * time.Second):
		t.Fatal("body did not start")
	}
	close(body.proceed)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("gate cancellation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled receive remained blocked on gate")
	}
	assertNoAttempts(t, root)
}

func TestStableEOFAndCloseReleaseReadProtection(t *testing.T) {
	s, _ := newStore(t)
	part := receive(t, s, "session", 1, "data")
	reader, err := s.Open(context.Background(), "session", []Part{part})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
			t.Fatalf("completed Read = %v", err)
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Discard(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	part = receive(t, s, "early-close", 1, "data")
	reader, err = s.Open(context.Background(), "early-close", []Part{part})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("closed Read = %v", err)
	}
	if err := s.Discard(context.Background(), "early-close"); err != nil {
		t.Fatal(err)
	}
}

func TestBaseAndInRootLockSymlinkSafety(t *testing.T) {
	s, base := newStore(t)
	// Owner-owned read/traverse-only access to the base is compatible with the
	// object spool baseline; the actual namespace remains private.
	if err := os.Chmod(base, 0755); err != nil {
		t.Fatal(err)
	}
	other, err := New(base, config(base))
	if err != nil {
		t.Fatalf("read-only public base denied: %v", err)
	}
	other.Close()
	if err := os.Chmod(base, 0777); err != nil {
		t.Fatal(err)
	}
	if other, err := New(base, config(base)); err == nil {
		other.Close()
		t.Fatal("world-writable base admitted")
	}
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session.lock", "slot-0.lock", "gate.lock"} {
		root := sessionRoot(t, s, "in-root-"+name)
		if err := root.Symlink("session.json", name); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Receive(context.Background(), "in-root-"+name, 1, 4, nil, strings.NewReader("data"), accept); err == nil {
			t.Fatalf("in-root lock symlink admitted: %s", name)
		}
	}
	// A namespace itself may not be a symlink, even to another owner-private
	// directory inside the same admitted base.
	if err := s.root.Mkdir("other", 0700); err != nil {
		t.Fatal(err)
	}
	key, _ := sessionKey("in-root-session")
	if err := s.root.Symlink("other", "s-"+key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(context.Background(), "in-root-session", nil); err == nil {
		t.Fatal("in-root session symlink admitted")
	}
	namespace := filepath.Join(base, "uploadspool-v1-"+s.binding)
	info, err := os.Stat(namespace)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("namespace was exposed: %v, %v", info, err)
	}
}
