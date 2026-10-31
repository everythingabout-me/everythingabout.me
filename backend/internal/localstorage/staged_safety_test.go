//go:build darwin || linux

package localstorage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

type stagedBindingSecret struct{}

func (stagedBindingSecret) Load(context.Context) ([]byte, error) {
	return []byte("unused-test-secret"), nil
}

func TestStagedStorageBindingPreservesExactConfigurationAndCopies(t *testing.T) {
	f, cfg := fixture(t, Options{CaseSensitive: true})
	originalContent := cfg.ContentRoot
	cfg.ContentRoot = filepath.Join(filepath.Dir(originalContent), "configured-content-alias")
	if err := os.Symlink(originalContent, cfg.ContentRoot); err != nil {
		t.Fatal(err)
	}
	cfg.Credentials = stagedBindingSecret{}
	w, err := NewStagedWriter(bind(t, f, cfg), WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	bound := w.StorageBinding()
	if bound.OwnerID != cfg.OwnerID || bound.ConnectionID != cfg.ConnectionID || bound.Generation != cfg.Generation || bound.ContentRoot != cfg.ContentRoot || bound.SystemRoot != cfg.SystemRoot || bound.Credentials != nil {
		t.Fatalf("writer lost exact operator binding or retained credentials: %+v", bound)
	}
	bound.OwnerID = "other-owner"
	bound.ConnectionID = "other-connection"
	bound.Generation++
	bound.ContentRoot = "/wrong"
	bound.SystemRoot = "/also-wrong"
	bound.Credentials = stagedBindingSecret{}
	fresh := w.StorageBinding()
	if fresh.OwnerID != cfg.OwnerID || fresh.ConnectionID != cfg.ConnectionID || fresh.Generation != cfg.Generation || fresh.ContentRoot != cfg.ContentRoot || fresh.SystemRoot != cfg.SystemRoot || fresh.Credentials != nil {
		t.Fatal("caller mutated writer binding")
	}
	rules := w.StorageNameRules()
	if !rules.CaseSensitive || rules.MaxSegmentBytes != maxSegmentBytes || rules.MaxPathBytes != maxPathBytes || rules.MaxDepth != maxDepth {
		t.Fatalf("writer name rules do not match local factory: %+v", rules)
	}
	rules.CaseSensitive = false
	rules.MaxDepth = 1
	rules.ReservedNames = append(rules.ReservedNames, "injected")
	if next := w.StorageNameRules(); !next.CaseSensitive || next.MaxDepth != maxDepth || len(next.ReservedNames) != 0 {
		t.Fatal("caller mutated writer name rules")
	}
}

type blockedStageBody struct {
	reader  *io.PipeReader
	reading chan struct{}
	once    sync.Once
	closes  atomic.Int32
}

func (b *blockedStageBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.reading) })
	return b.reader.Read(p)
}
func (b *blockedStageBody) Close() error { b.closes.Add(1); return b.reader.Close() }

func TestBlockedStageCancellationAndDriverCloseReleaseLockAndSlot(t *testing.T) {
	for _, mode := range []string{"context", "driver-close"} {
		t.Run(mode, func(t *testing.T) {
			f, cfg := fixture(t, Options{MaxConcurrentOperations: 1})
			d := bind(t, f, cfg)
			w, err := NewStagedWriter(d, WriteOptions{CoordinatedExclusiveWriter: true})
			if err != nil {
				t.Fatal(err)
			}
			pipe, producer := io.Pipe()
			defer producer.Close()
			body := &blockedStageBody{reader: pipe, reading: make(chan struct{})}
			in := writeIntent("blocked-reader", "final", "abc")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := w.Stage(ctx, in, body, stageOK); done <- err }()
			select {
			case <-body.reading:
			case <-time.After(5 * time.Second):
				t.Fatal("stage never began blocked Read")
			}
			if mode == "context" {
				cancel()
			} else {
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("blocked stage error=%v", err)
				}
			case <-time.After(5 * time.Second):
				_ = producer.Close()
				t.Fatal("cancellation did not unblock ingress")
			}
			// A fresh binding needs the shared single operation slot. Its successful
			// open and recovery also prove the former stage released its OS lock.
			probeCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			freshDriver, err := f.Open(probeCtx, cfg)
			if err != nil {
				t.Fatalf("stage retained operation slot: %v", err)
			}
			defer freshDriver.Close()
			fresh, err := NewStagedWriter(freshDriver, WriteOptions{CoordinatedExclusiveWriter: true})
			if err != nil {
				t.Fatal(err)
			}
			ins, err := fresh.Inspect(probeCtx, in, nil)
			if err != nil || ins.State != storage.NotApplied {
				t.Fatalf("stage retained lock or ambiguous effect: %+v %v", ins, err)
			}
			if body.closes.Load() != 1 {
				t.Fatalf("body closed %d times", body.closes.Load())
			}
		})
	}
}

func TestNativeSuccessThenErrorUsesIdentityRecovery(t *testing.T) {
	w, f, cfg := writerFixture(t)
	in := writeIntent("native-lost-reply", "final", "abc")
	s := stageFile(t, w, in, "abc")
	w.link = func(source *os.Root, old string, target *os.Root, name string) error {
		if err := stagedLink(source, old, target, name); err != nil {
			return err
		}
		return syscall.EIO
	}
	if _, err := w.Publish(context.Background(), s, stageOK); !errors.Is(err, syscall.EIO) {
		t.Fatalf("injected native error=%v", err)
	}
	ledgerForTest(t, w, in, func(_ *os.Root, r *stageLedger) {
		if r.Phase != "publishing" {
			t.Fatalf("ambiguous native error sealed as %s", r.Phase)
		}
	})
	ins := inspectState(t, restartWriter(t, f, cfg), in, storage.Applied)
	if ins.Result == nil || ins.Result.Entry.Size != 3 {
		t.Fatal("native error lost actual applied result")
	}
	if raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "final")); err != nil || string(raw) != "abc" {
		t.Fatal("native error rolled back bytes")
	}
}

func TestInspectLockedIntentDoesNotWaitForDriverSlot(t *testing.T) {
	f, cfg := fixture(t, Options{MaxConcurrentOperations: 1})
	w, err := NewStagedWriter(bind(t, f, cfg), WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	in := writeIntent("one-slot", "final", "abc")
	s := stageFile(t, w, in, "abc")
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := w.Publish(context.Background(), s, func(_ context.Context, b []byte) error {
			var r stageReceipt
			_ = json.Unmarshal(b, &r)
			if r.Phase == "publishing" {
				close(entered)
				<-release
			}
			return nil
		})
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	inspected, err := w.Inspect(ctx, in, nil)
	close(release)
	if err != nil || inspected.State != storage.OutcomeUnknown {
		t.Fatalf("inspect waited for full operation pool: %+v %v", inspected, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPublishFenceCallbackLossAndNativeNoReplace(t *testing.T) {
	for _, mode := range []string{"lease-lost", "native-collision", "stage-corrupt", "parent-replaced"} {
		t.Run(mode, func(t *testing.T) {
			w, f, cfg := writerFixture(t)
			if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "parent"), 0700); err != nil {
				t.Fatal(err)
			}
			in := writeIntent("delayed", "parent/final", "abc")
			s := stageFile(t, w, in, "abc")
			lost := errors.New("operation fence lost")
			result, err := w.Publish(context.Background(), s, func(_ context.Context, b []byte) error {
				var receipt stageReceipt
				if err := json.Unmarshal(b, &receipt); err != nil {
					t.Fatal(err)
				}
				if receipt.Phase != "publishing" {
					return nil
				}
				switch mode {
				case "lease-lost":
					return lost
				case "native-collision":
					writeFixture(t, cfg.ContentRoot, "parent/final", "old")
				case "stage-corrupt":
					native := filepath.Join(cfg.SystemRoot, w.namespace, intentKey(in.ID)+"-"+receipt.Attempt+".data")
					if err := os.WriteFile(native, []byte("bad"), 0600); err != nil {
						t.Fatal(err)
					}
				case "parent-replaced":
					if err := os.Rename(filepath.Join(cfg.ContentRoot, "parent"), filepath.Join(cfg.ContentRoot, "old-parent")); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "parent"), 0700); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			if err == nil || result.Entry != nil {
				t.Fatal("unsafe delayed publication succeeded")
			}
			if mode == "lease-lost" && !errors.Is(err, lost) {
				t.Fatal(err)
			}
			if mode == "native-collision" {
				raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "parent/final"))
				if err != nil || string(raw) != "old" {
					t.Fatal("atomic native create replaced collision")
				}
			} else {
				if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "parent/final")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed fence wrote final")
				}
			}
			if mode == "lease-lost" {
				inspectState(t, restartWriter(t, f, cfg), in, storage.NotApplied)
				if _, err := w.Publish(context.Background(), s, stageOK); err == nil {
					t.Fatal("late worker bypassed recovered terminal intent")
				}
			}
			if mode == "parent-replaced" {
				if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "old-parent/final")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("publication wrote detached parent")
				}
				inspectState(t, w, in, storage.OutcomeUnknown)
			}
		})
	}
}

func TestLostNativeLedgerWithReceiptRetainsUnknownGuard(t *testing.T) {
	w, f, cfg := writerFixture(t)
	in := writeIntent("lost-ledger", "final", "abc")
	s := stageFile(t, w, in, "abc")
	if _, err := w.Publish(context.Background(), s, stageOK); err != nil {
		t.Fatal(err)
	}
	ledgerForTest(t, w, in, func(root *os.Root, r *stageLedger) {
		if err := root.Remove(intentKey(in.ID) + ".json"); err != nil {
			t.Fatal(err)
		}
		if err := syncRoot(root); err != nil {
			t.Fatal(err)
		}
	})
	fresh := restartWriter(t, f, cfg)
	ins, err := fresh.Inspect(context.Background(), in, s.Handle)
	if err != nil || ins.State != storage.OutcomeUnknown {
		t.Fatalf("missing ledger incorrectly resolved: %+v %v", ins, err)
	}
	inspectState(t, fresh, in, storage.OutcomeUnknown)
	if err := fresh.Discard(context.Background(), in, s.Handle); !storage.IsCode(err, "outcome_unknown") {
		t.Fatalf("discard sealed unresolved effect: %v", err)
	}
	if _, err := w.Publish(context.Background(), s, stageOK); err == nil {
		t.Fatal("stale publication bypassed missing-ledger guard")
	} else {
		var no *storage.NoFinalEffect
		if errors.As(err, &no) {
			t.Fatal("missing-ledger effect falsely declared absent")
		}
	}
	if raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "final")); err != nil || string(raw) != "abc" {
		t.Fatal("missing-ledger recovery touched final")
	}
}

func TestStagedRootEvidenceAndDuplicatePayloadIsolation(t *testing.T) {
	w, _, cfg := writerFixture(t)
	in := writeIntent("same-id", "final", "abc")
	s := stageFile(t, w, in, "abc")
	foreign, _, foreignCfg := writerFixture(t)
	if _, err := foreign.Inspect(context.Background(), in, s.Handle); !storage.IsCode(err, "invalid_evidence") {
		t.Fatalf("foreign receipt accepted: %v", err)
	}
	if entries, err := os.ReadDir(foreignCfg.ContentRoot); err != nil || len(entries) != 0 {
		t.Fatal("foreign evidence wrote content")
	}
	if _, err := w.Publish(context.Background(), s, stageOK); err != nil {
		t.Fatal(err)
	}
	bad := s
	bad.Digest = cloneDigest(s.Digest)
	bad.Digest.Value = bytes.Repeat([]byte{1}, 32)
	if _, err := w.Publish(context.Background(), bad, stageOK); !storage.IsCode(err, "invalid_evidence") {
		t.Fatalf("duplicate publication accepted different digest: %v", err)
	}
	// Unknown system files and independent ordinary final files survive Discard.
	writeFixture(t, cfg.SystemRoot, w.namespace+"/unrecognized.data", "preserve unknown temporary liability")
	if err := w.Discard(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(cfg.SystemRoot, w.namespace, "unrecognized.data")); err != nil || string(raw) != "preserve unknown temporary liability" {
		t.Fatal("discard removed unknown native material")
	}
	if raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "final")); err != nil || string(raw) != "abc" {
		t.Fatal("discard removed published file")
	}
	// A canonical checksum is required; opaque native validators are not digests.
	unsupported := writeIntent("unsupported-digest", "another", "abc")
	unsupported.Digest.Algorithm = "etag"
	if _, err := w.Stage(context.Background(), unsupported, strings.NewReader("abc"), stageOK); !storage.IsCode(err, "unsupported") {
		t.Fatalf("unsupported checksum admitted: %v", err)
	}
}
