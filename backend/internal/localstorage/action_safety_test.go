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
	"syscall"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
)

func TestMutationPrimitiveAmbiguityRecovery(t *testing.T) {
	for _, action := range []string{"replace", "copy", "remove"} {
		t.Run(action, func(t *testing.T) {
			w, f, cfg := mutationFixture(t)
			ctx := context.Background()
			writeFixture(t, cfg.ContentRoot, "file", "old")
			old := mutationStat(t, w, "file")
			var err error
			if action == "replace" {
				in := writeIntent("replace", "file", "new")
				in.Conditions = storage.Conditions{ExpectedVersion: old.Version}
				s := stageFile(t, w.StagedWriter, in, "new")
				w.replace = func(a *os.Root, an string, b *os.Root, bn string, replace bool) error {
					if err := actionRename(a, an, b, bn, replace); err != nil {
						return err
					}
					return syscall.EIO
				}
				_, err = w.Publish(ctx, s, stageOK)
				if !errors.Is(err, syscall.EIO) {
					t.Fatal(err)
				}
				ins := inspectState(t, restartMutation(t, f, cfg).StagedWriter, in, storage.Applied)
				if len(ins.Result.Entry.Version) == 0 {
					t.Fatal("replacement recovery lost version")
				}
			} else if action == "copy" {
				in := storage.EffectIntent{ID: "copy", Action: "copy", Source: "file", Target: "copied", SourceVersion: old.Version, Conditions: storage.Conditions{Absent: true}}
				w.link = func(a *os.Root, an string, b *os.Root, bn string) error {
					if err := stagedLink(a, an, b, bn); err != nil {
						return err
					}
					return syscall.EIO
				}
				_, err = w.runAction(ctx, in, stageOK)
				if !errors.Is(err, syscall.EIO) {
					t.Fatal(err)
				}
				actionInspection(t, restartMutation(t, f, cfg), in, nil, storage.Applied)
			} else {
				in := storage.EffectIntent{ID: "remove", Action: "remove", Target: "file", Conditions: storage.Conditions{ExpectedVersion: old.Version}}
				w.remove = func(root *os.Root, name string) error {
					if err := root.Remove(name); err != nil {
						return err
					}
					return syscall.EIO
				}
				_, err = w.runAction(ctx, in, stageOK)
				if !errors.Is(err, syscall.EIO) {
					t.Fatal(err)
				}
				fresh := restartMutation(t, f, cfg)
				ins := actionInspection(t, fresh, in, nil, storage.Applied)
				actionInspection(t, fresh, storage.EffectIntent{ID: in.ID, Action: "delete", Source: in.Target, SourceVersion: old.Version}, ins.Evidence, storage.Applied)
			}
		})
	}
}
func TestMutationReplacementRetainsNotAppliedAndUnknownEvidence(t *testing.T) {
	for _, mode := range []string{"unchanged", "drift", "missing-ledger"} {
		t.Run(mode, func(t *testing.T) {
			w, f, cfg := mutationFixture(t)
			ctx := context.Background()
			writeFixture(t, cfg.ContentRoot, "file", "old")
			old := mutationStat(t, w, "file")
			in := writeIntent("replace", "file", "new")
			in.Conditions = storage.Conditions{ExpectedVersion: old.Version}
			s := stageFile(t, w.StagedWriter, in, "new")
			_, err := w.Publish(ctx, s, func(_ context.Context, b []byte) error {
				var receipt stageReceipt
				_ = json.Unmarshal(b, &receipt)
				if receipt.Phase == "publishing" {
					return io.ErrClosedPipe
				}
				return nil
			})
			if err == nil {
				t.Fatal("expected interrupted checkpoint")
			}
			if mode == "drift" {
				if err := os.WriteFile(filepath.Join(cfg.ContentRoot, "file"), []byte("external"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-ledger" {
				root, err := w.root(ctx)
				if err != nil {
					t.Fatal(err)
				}
				err = root.Remove(intentKey(in.ID) + ".json")
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			expected := storage.OutcomeUnknown
			if mode == "unchanged" {
				expected = storage.NotApplied
			}
			fresh := restartMutation(t, f, cfg)
			ins, err := fresh.Inspect(ctx, in, s.Handle)
			if err != nil || ins.State != expected {
				t.Fatalf("inspection=%+v err=%v", ins, err)
			}
			if mode == "unchanged" {
				if _, err := w.Publish(ctx, s, stageOK); err == nil {
					t.Fatal("stale replacement resumed after recovery")
				}
			}
		})
	}
}
func TestMutationCopyRechecksSourceAfterDelayedCallback(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	writeFixture(t, cfg.ContentRoot, "file", "old")
	old := mutationStat(t, w, "file")
	req := storage.MoveRequest{ID: "copy", Source: "file", Target: "copied", SourceVersion: old.Version, TargetConditions: storage.Conditions{Absent: true}}
	_, err := w.Copy(context.Background(), req, func(_ context.Context, b []byte) error {
		if actionPhase(b) == "executing" {
			return os.WriteFile(filepath.Join(cfg.ContentRoot, "file"), []byte("new"), 0600)
		}
		return nil
	})
	requireCode(t, err, "changed")
	if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "copied")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("changed source was copied")
	}
}
func TestMutationSourceTimestampAndFrozenRead(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	ctx := context.Background()
	modified := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	in := writeIntent("timed", "file", "old bytes")
	in.SourceModifiedAt = &modified
	s := stageFile(t, w.StagedWriter, in, "old bytes")
	result, err := w.Publish(ctx, s, stageOK)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Entry.ModifiedAt.Equal(modified) {
		t.Fatal("accepted native timestamp was not preserved")
	}
	read, err := w.Open(ctx, "file", storage.ReadOptions{Version: result.Entry.Version})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Body.Close()
	replacement := writeIntent("next", "file", "new bytes")
	replacement.Conditions = storage.Conditions{ExpectedVersion: result.Entry.Version}
	next, err := w.Stage(ctx, replacement, strings.NewReader("new bytes"), stageOK)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Publish(ctx, next, stageOK); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(read.Body)
	if err != nil || string(raw) != "old bytes" {
		t.Fatal("selected generation changed under atomic replacement")
	}
	newEntry := mutationStat(t, w, "file")
	if bytes.Equal(newEntry.Version, result.Entry.Version) {
		t.Fatal("replacement reused version")
	}
	_ = cfg
}
func TestMutationRemoveRechecksEmptyFolderAfterCallback(t *testing.T) {
	w, f, cfg := mutationFixture(t)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "folder"), 0700); err != nil {
		t.Fatal(err)
	}
	e := mutationStat(t, w, "folder")
	in := storage.EffectIntent{ID: "remove-folder", Action: "remove", Target: "folder", Conditions: storage.Conditions{ExpectedVersion: e.Version}}
	_, err := w.runAction(ctx, in, func(_ context.Context, b []byte) error {
		if actionPhase(b) == "executing" {
			return os.WriteFile(filepath.Join(cfg.ContentRoot, "folder/new"), []byte("keep"), 0600)
		}
		return nil
	})
	requireCode(t, err, "not_empty")
	actionInspection(t, restartMutation(t, f, cfg), in, nil, storage.NotApplied)
	raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "folder/new"))
	if err != nil || string(raw) != "keep" {
		t.Fatal("added child removed")
	}
}
