//go:build darwin || linux

package localstorage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

var stageOK storage.Checkpoint = func(context.Context, []byte) error { return nil }

func writerFixture(t *testing.T) (*StagedWriter, *Factory, storage.Config) {
	t.Helper()
	f, cfg := fixture(t, Options{})
	d := bind(t, f, cfg)
	w, err := NewStagedWriter(d, WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	return w, f, cfg
}
func writeIntent(id string, path storage.Path, body string) storage.WriteIntent {
	h := sha256.Sum256([]byte(body))
	return storage.WriteIntent{ID: id, Target: path, Conditions: storage.Conditions{Absent: true}, Size: int64(len(body)), MediaType: "text/plain", Digest: &storage.Digest{Algorithm: "sha-256", Value: h[:]}}
}
func stageFile(t *testing.T, w *StagedWriter, in storage.WriteIntent, body string) storage.Staged {
	t.Helper()
	s, err := w.Stage(context.Background(), in, strings.NewReader(body), stageOK)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func inspectState(t *testing.T, w *StagedWriter, in storage.WriteIntent, state storage.PublishState) storage.Inspection {
	t.Helper()
	got, err := w.Inspect(context.Background(), in, nil)
	if err != nil || got.State != state {
		t.Fatalf("inspect=%+v err=%v want=%s", got, err, state)
	}
	return got
}
func restartWriter(t *testing.T, f *Factory, cfg storage.Config) *StagedWriter {
	t.Helper()
	w, err := NewStagedWriter(bind(t, f, cfg), WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func ledgerForTest(t *testing.T, w *StagedWriter, in storage.WriteIntent, fn func(*os.Root, *stageLedger)) {
	t.Helper()
	root, done, err := w.begin(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	r, err := w.load(root, in)
	if err != nil {
		t.Fatal(err)
	}
	fn(root, r)
}

func TestStagedAdmissionAndUnsupportedReplace(t *testing.T) {
	f, cfg := fixture(t, Options{})
	d := bind(t, f, cfg)
	if _, err := NewStagedWriter(d, WriteOptions{}); !storage.IsCode(err, "unsupported") {
		t.Fatalf("default admission=%v", err)
	}
	contents, err := os.ReadDir(cfg.SystemRoot)
	if err != nil || len(contents) != 0 {
		t.Fatal("default constructor allocated storage")
	}
	w, err := NewStagedWriter(d, WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	if d.Describe().AtomicCreate || d.Describe().AtomicReplace || d.Describe().NativeConditionalMutation {
		t.Fatal("read factory capabilities changed")
	}
	writeFixture(t, cfg.ContentRoot, "old", "keep old complete bytes")
	for _, conditions := range []storage.Conditions{{ExpectedRevision: []byte("untrustworthy")}, {}, {Absent: true, ExpectedRevision: []byte("both")}} {
		in := writeIntent("replace", "old", "new")
		in.Conditions = conditions
		_, err := w.Stage(context.Background(), in, strings.NewReader("new"), stageOK)
		requireCode(t, err, "unsupported")
	}
	root, err := w.root(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer entries.Close()
	names, err := entries.Readdirnames(-1)
	if err != nil || len(names) != 0 {
		t.Fatalf("replacement allocated: %v %v", names, err)
	}
	old, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "old"))
	if err != nil || string(old) != "keep old complete bytes" {
		t.Fatal("replacement changed old file")
	}
}
func TestStagedNativeNamesRestartAndIndependentFiles(t *testing.T) {
	w, f, cfg := writerFixture(t)
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	body := "raw\x00\xffcomplete bytes"
	names := []storage.Path{"folder/ cafe\u0301 +#?%.txt", "zero", "other"}
	for i, name := range names {
		b := body
		if i == 1 {
			b = ""
		}
		in := writeIntent(string(rune('a'+i)), name, b)
		s := stageFile(t, w, in, b)
		if _, err := os.Stat(filepath.Join(cfg.ContentRoot, string(name))); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("stage visible at final path")
		}
		fresh := restartWriter(t, f, cfg)
		ins := inspectState(t, fresh, in, storage.StagedReady)
		if !bytes.Equal(ins.Stage.Digest.Value, s.Digest.Value) {
			t.Fatal("restart lost verified stage digest")
		}
		result, err := fresh.Publish(context.Background(), *ins.Stage, stageOK)
		if err != nil || result.Entry.Path != name {
			t.Fatalf("publish=%+v %v", result, err)
		}
		dup, err := fresh.Publish(context.Background(), s, stageOK)
		if err != nil || !reflect.DeepEqual(result, dup) {
			t.Fatalf("duplicate result differs: %+v %v", dup, err)
		}
		if err := fresh.Discard(context.Background(), in, s.Handle); err != nil {
			t.Fatal(err)
		}
		if err := fresh.Discard(context.Background(), in, nil); err != nil {
			t.Fatal(err)
		}
		inspectState(t, restartWriter(t, f, cfg), in, storage.Applied)
		raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, string(name)))
		if err != nil || string(raw) != b {
			t.Fatal("ordinary direct-exit read differs")
		}
	}
	if err := os.WriteFile(filepath.Join(cfg.ContentRoot, string(names[0])), []byte("edited independently"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "other"))
	if err != nil || string(raw) != body {
		t.Fatal("independent published file changed")
	}
}
func TestStageValidationAndFailedAllocationCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		size             int64
		badHash          bool
	}{{"short", "abc", "size_mismatch", 4, false}, {"long", "abcd", "size_mismatch", 3, false}, {"hash", "abc", "digest_mismatch", 3, true}} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, cfg := writerFixture(t)
			in := writeIntent(tc.name, "final", tc.body)
			in.Size = tc.size
			if tc.badHash {
				in.Digest.Value = make([]byte, 32)
			}
			_, err := w.Stage(context.Background(), in, strings.NewReader(tc.body), stageOK)
			requireCode(t, err, tc.code)
			inspectState(t, w, in, storage.NotApplied)
			if err := w.Discard(context.Background(), in, nil); err != nil {
				t.Fatal(err)
			}
			ledgerForTest(t, w, in, func(root *os.Root, r *stageLedger) {
				if _, err := root.Lstat(dataName(r)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("known failed temp retained")
				}
			})
			if entries, err := os.ReadDir(cfg.ContentRoot); err != nil || len(entries) != 0 {
				t.Fatal("failed stage wrote final")
			}
		})
	}
	w, _, _ := writerFixture(t)
	in := writeIntent("cancel", "final", "payload")
	ctx, cancel := context.WithCancel(context.Background())
	_, err := w.Stage(ctx, in, strings.NewReader("payload"), func(context.Context, []byte) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stage=%v", err)
	}
	inspectState(t, w, in, storage.NotApplied)
}
func TestStageAllocationCheckpointBeforeDataAndLostReadyCheckpoint(t *testing.T) {
	w, f, cfg := writerFixture(t)
	in := writeIntent("allocation", "final", "payload")
	lost := errors.New("checkpoint unavailable")
	_, err := w.Stage(context.Background(), in, strings.NewReader("payload"), func(_ context.Context, b []byte) error {
		var receipt stageReceipt
		if err := json.Unmarshal(b, &receipt); err != nil {
			t.Fatal(err)
		}
		r := stageLedger{Intent: in, Attempt: receipt.Attempt}
		root, err := w.root(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		if _, err := root.Lstat(dataName(&r)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("allocated before checkpoint")
		}
		return lost
	})
	if !errors.Is(err, lost) {
		t.Fatal(err)
	}
	fresh := restartWriter(t, f, cfg)
	inspectState(t, fresh, in, storage.NotApplied)
	if err := fresh.Discard(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	in = writeIntent("ready-lost", "another", "payload")
	calls := 0
	_, err = w.Stage(context.Background(), in, strings.NewReader("payload"), func(context.Context, []byte) error {
		calls++
		if calls == 2 {
			return lost
		}
		return nil
	})
	if !errors.Is(err, lost) {
		t.Fatal(err)
	}
	ins := inspectState(t, restartWriter(t, f, cfg), in, storage.StagedReady)
	if _, err := fresh.Publish(context.Background(), *ins.Stage, stageOK); err != nil {
		t.Fatal(err)
	}
}
func TestPublishLostCheckpointAndVisibleButInflight(t *testing.T) {
	w, f, cfg := writerFixture(t)
	in := writeIntent("lost-reply", "final", "payload")
	s := stageFile(t, w, in, "payload")
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	lost := errors.New("database acknowledgement lost")
	go func() {
		_, err := w.Publish(context.Background(), s, func(_ context.Context, b []byte) error {
			var r stageReceipt
			_ = json.Unmarshal(b, &r)
			if r.Phase == "applied" {
				close(entered)
				<-release
				return lost
			}
			return nil
		})
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher never reached final checkpoint")
	}
	fresh := restartWriter(t, f, cfg)
	inspectState(t, fresh, in, storage.OutcomeUnknown)
	if _, err := fresh.Publish(context.Background(), s, stageOK); err == nil {
		t.Fatal("overlapping publisher admitted")
	} else {
		var no *storage.NoFinalEffect
		if errors.As(err, &no) {
			t.Fatal("inflight publisher falsely declared no final effect")
		}
	}
	if err := fresh.Discard(context.Background(), in, nil); err == nil {
		t.Fatal("discard admitted while publishing")
	}
	close(release)
	if err := <-finished; !errors.Is(err, lost) {
		t.Fatal(err)
	}
	inspected := inspectState(t, restartWriter(t, f, cfg), in, storage.Applied)
	if inspected.Result == nil || inspected.Result.Entry.Size != 7 {
		t.Fatal("lost actual publication result")
	}
	if raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "final")); err != nil || string(raw) != "payload" {
		t.Fatal("lost checkpoint rolled back final bytes")
	}
}
func TestInterruptedFinalEffectRecoveryAndNoSizeGuess(t *testing.T) {
	for _, mode := range []string{"applied", "absent", "same-size-other", "corrupt-inode"} {
		t.Run(mode, func(t *testing.T) {
			w, f, cfg := writerFixture(t)
			in := writeIntent("interrupted", "final", "payload")
			s := stageFile(t, w, in, "payload")
			ledgerForTest(t, w, in, func(root *os.Root, r *stageLedger) {
				dest, name, err := w.destination(context.Background(), in)
				if err != nil {
					t.Fatal(err)
				}
				defer dest.Close()
				pi, err := dest.Stat(".")
				if err != nil {
					t.Fatal(err)
				}
				r.ParentIdentity = rootIdentity(pi)
				r.Phase = "publishing"
				if err := w.save(root, r); err != nil {
					t.Fatal(err)
				}
				if mode == "applied" || mode == "corrupt-inode" {
					if err := stagedLink(root, dataName(r), dest, name); err != nil {
						t.Fatal(err)
					}
				}
			})
			if mode == "same-size-other" {
				writeFixture(t, cfg.ContentRoot, "final", "another")
			}
			if mode == "corrupt-inode" {
				writeFixture(t, cfg.ContentRoot, "final", "another")
			}
			state := storage.OutcomeUnknown
			if mode == "applied" {
				state = storage.Applied
			}
			if mode == "absent" {
				state = storage.NotApplied
			}
			fresh := restartWriter(t, f, cfg)
			inspectState(t, fresh, in, state)
			if mode == "absent" {
				_, err := w.Publish(context.Background(), s, stageOK)
				if err == nil {
					t.Fatal("stale delayed worker published after terminal recovery")
				}
				if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "final")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("stale worker changed final")
				}
			}
		})
	}
}
func TestStageIntentImmutabilityAndConcurrentAttempt(t *testing.T) {
	w, _, _ := writerFixture(t)
	in := writeIntent("same-id", "final", "abc")
	s := stageFile(t, w, in, "abc")
	changed := cloneIntent(in)
	changed.Target = "other"
	_, err := w.Stage(context.Background(), changed, strings.NewReader("abc"), stageOK)
	requireCode(t, err, "intent_conflict")
	noDigest := writeIntent("no-digest", "another", "abc")
	noDigest.Digest = nil
	stageFile(t, w, noDigest, "abc")
	_, err = w.Stage(context.Background(), noDigest, strings.NewReader("xyz"), stageOK)
	requireCode(t, err, "intent_conflict")
	altered := s
	altered.Intent.MediaType = "other"
	_, err = w.Publish(context.Background(), altered, stageOK)
	requireCode(t, err, "intent_conflict")
	in = writeIntent("concurrent", "third", "abc")
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := w.Stage(context.Background(), in, strings.NewReader("abc"), func(_ context.Context, b []byte) error {
			var r stageReceipt
			_ = json.Unmarshal(b, &r)
			if r.Phase == "reserved" {
				close(entered)
				<-release
			}
			return nil
		})
		done <- err
	}()
	<-entered
	inspectState(t, w, in, storage.OutcomeUnknown)
	if _, err := w.Stage(context.Background(), in, strings.NewReader("xyz"), stageOK); err == nil {
		t.Fatal("inflight stage attempt overwritten")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestPublishCollisionsParentsAndSymlinks(t *testing.T) {
	for _, kind := range []string{"missing-parent", "symlink-parent", "symlink-target", "case-collision", "target-collision"} {
		t.Run(kind, func(t *testing.T) {
			w, _, cfg := writerFixture(t)
			in := writeIntent(kind, "parent/final", "payload")
			if kind != "missing-parent" {
				if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "parent"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink-parent" {
				if err := os.Rename(filepath.Join(cfg.ContentRoot, "parent"), filepath.Join(cfg.ContentRoot, "actual")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("actual", filepath.Join(cfg.ContentRoot, "parent")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "missing-parent" || kind == "symlink-parent" {
				if _, err := w.Stage(context.Background(), in, strings.NewReader("payload"), stageOK); err == nil {
					t.Fatal("unsafe parent accepted")
				}
				return
			}
			s := stageFile(t, w, in, "payload")
			target := "parent/final"
			if kind == "case-collision" {
				target = "parent/FINAL"
			}
			if kind == "symlink-target" {
				writeFixture(t, cfg.ContentRoot, "untouched", "old")
				if err := os.Symlink("../untouched", filepath.Join(cfg.ContentRoot, target)); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFixture(t, cfg.ContentRoot, target, "old")
			}
			if _, err := w.Publish(context.Background(), s, stageOK); err == nil {
				t.Fatal("collision overwritten")
			}
			if err := w.Discard(context.Background(), in, nil); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, target))
			if err != nil || string(raw) != "old" {
				t.Fatal("discard or publication changed ordinary file")
			}
		})
	}
}
func TestInspectUnknownIntentFencesFutureAllocationAndEffectRecovery(t *testing.T) {
	w, _, _ := writerFixture(t)
	in := writeIntent("not-started", "final", "abc")
	inspectState(t, w, in, storage.NotApplied)
	if _, err := w.Stage(context.Background(), in, strings.NewReader("abc"), stageOK); err == nil {
		t.Fatal("allocation bypassed tombstone")
	}
	for _, action := range []string{"mkdir", "remove", "move", "copy"} {
		ins, err := w.InspectEffect(context.Background(), storage.EffectIntent{ID: "unknown", Action: action, Target: "final"}, nil)
		if err != nil || ins.State != storage.OutcomeUnknown {
			t.Fatal("unsupported recovery guessed outcome")
		}
	}
	in = writeIntent("effect", "other", "abc")
	s := stageFile(t, w, in, "abc")
	if _, err := w.Publish(context.Background(), s, stageOK); err != nil {
		t.Fatal(err)
	}
	ins, err := w.InspectEffect(context.Background(), storage.EffectIntent{ID: in.ID, Action: "publish", Target: in.Target, Conditions: in.Conditions}, nil)
	if err != nil || ins.State != storage.Applied {
		t.Fatalf("effect recovery=%+v %v", ins, err)
	}
}

// A separate process demonstrates that flock is native, rather than an in-memory
// mutex. Killing it releases the lock so a fresh recovery can seal the intent.
func TestStagedProcessLockRecovery(t *testing.T) {
	if native := os.Getenv("EAM_STAGE_LOCK_CHILD"); native != "" {
		root, err := os.OpenRoot(native)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		unlock, err := stagedLock(root, "child.lock")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		if _, err := os.Stdout.Write([]byte("locked\n")); err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	rootPath := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStagedProcessLockRecovery$")
	cmd.Env = append(os.Environ(), "EAM_STAGE_LOCK_CHILD="+rootPath)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	b := make([]byte, 7)
	if _, err := io.ReadFull(stdout, b); err != nil || string(b) != "locked\n" {
		t.Fatalf("child ready=%q %v", b, err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if unlock, err := stagedLock(root, "child.lock"); !errors.Is(err, errStagedLocked) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("process lock=%v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed child succeeded")
	}
	unlock, err := stagedLock(root, "child.lock")
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
