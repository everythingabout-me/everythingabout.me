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

	"everythingabout.me/backend/internal/storage"
)

func mutationFixture(t *testing.T) (*MutationDriver, *Factory, storage.Config) {
	t.Helper()
	f, cfg := fixture(t, Options{})
	w, err := NewMutationDriver(bind(t, f, cfg), WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	return w, f, cfg
}
func mutationStat(t *testing.T, w *MutationDriver, path storage.Path) storage.Entry {
	t.Helper()
	e, err := w.Stat(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func restartMutation(t *testing.T, f *Factory, cfg storage.Config) *MutationDriver {
	t.Helper()
	w, err := NewMutationDriver(bind(t, f, cfg), WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func actionPhase(b []byte) string { var r actionReceipt; _ = json.Unmarshal(b, &r); return r.Phase }
func actionInspection(t *testing.T, w *MutationDriver, in storage.EffectIntent, evidence []byte, state storage.PublishState) storage.Inspection {
	t.Helper()
	got, err := w.InspectEffect(context.Background(), in, evidence)
	if err != nil || got.State != state {
		t.Fatalf("inspection=%+v err=%v want=%s", got, err, state)
	}
	return got
}
func TestMutationNativeCRUDAndContentIdentity(t *testing.T) {
	w, f, cfg := mutationFixture(t)
	ctx := context.Background()
	if !w.Describe().AtomicReplace || !w.Describe().PinnedRead || w.Describe().NativeConditionalMutation || w.driver.Describe().AtomicCreate {
		t.Fatal("incorrect capability isolation")
	}
	if _, err := NewMutationDriver(w.driver, WriteOptions{}); !storage.IsCode(err, "unsupported") {
		t.Fatal(err)
	}
	mk := storage.Mutation{ID: "mkdir", Path: "Native + 名", Conditions: storage.Conditions{Absent: true}}
	made, err := w.Mkdir(ctx, mk, stageOK)
	if err != nil || made.Entry.Kind != storage.Folder {
		t.Fatalf("mkdir=%+v %v", made, err)
	}
	originalFolderVersion := bytes.Clone(made.Entry.Version)
	in := writeIntent("file", "Native + 名/raw", "raw\x00\xffbytes")
	staged := stageFile(t, w.StagedWriter, in, "raw\x00\xffbytes")
	published, err := w.Publish(ctx, staged, stageOK)
	if err != nil {
		t.Fatal(err)
	}
	source := mutationStat(t, w, in.Target)
	if !bytes.Equal(published.Entry.Version, source.Version) || !bytes.Equal(published.Entry.ContentIdentity, source.ContentIdentity) || len(source.Version) != 32 {
		t.Fatal("publication and reads disagree")
	}
	if !bytes.Equal(originalFolderVersion, mutationStat(t, w, mk.Path).Version) {
		t.Fatal("child membership changed ancestor identity version")
	}
	read, err := w.Open(ctx, in.Target, storage.ReadOptions{Version: source.Version})
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(read.Body)
	read.Body.Close()
	if err != nil || string(body) != "raw\x00\xffbytes" {
		t.Fatal("native read differs")
	}
	_, err = w.Open(ctx, in.Target, storage.ReadOptions{Version: []byte("stale")})
	requireCode(t, err, "changed")
	copyReq := storage.MoveRequest{ID: "copy", Source: in.Target, Target: "copy", SourceVersion: source.Version, TargetConditions: storage.Conditions{Absent: true}}
	copied, err := w.Copy(ctx, copyReq, stageOK)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(copied.Entry.Version, source.Version) || bytes.Equal(copied.Entry.ContentIdentity, source.ContentIdentity) || !bytes.Equal(copied.Entry.Digest.Value, source.Digest.Value) {
		t.Fatal("copy is not an independent native generation")
	}
	moveReq := storage.MoveRequest{ID: "move", Source: in.Target, Target: "renamed", SourceVersion: source.Version, TargetConditions: storage.Conditions{Absent: true}}
	moved, err := w.Move(ctx, moveReq, stageOK)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(moved.Entry.Version, source.Version) || !bytes.Equal(moved.Entry.ContentIdentity, source.ContentIdentity) {
		t.Fatal("rename changed content generation")
	}
	if err := os.WriteFile(filepath.Join(cfg.ContentRoot, "renamed"), []byte("changed independent bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.ContentRoot, "copy"))
	if err != nil || string(raw) != "raw\x00\xffbytes" {
		t.Fatal("copy shares source mutable bytes")
	}
	page, err := w.List(ctx, "", storage.PageRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range page.Entries {
		if len(e.Version) == 0 || len(e.ContentIdentity) == 0 {
			t.Fatal("list omitted trusted identity")
		}
	}
	rm := storage.Mutation{ID: "remove", Path: "copy", Conditions: storage.Conditions{ExpectedVersion: copied.Entry.Version}}
	if _, err := w.Remove(ctx, rm, stageOK); err != nil {
		t.Fatal(err)
	}
	actionInspection(t, restartMutation(t, f, cfg), storage.EffectIntent{ID: rm.ID, Action: "remove", Target: rm.Path, Conditions: rm.Conditions}, nil, storage.Applied)
	if _, err := w.Remove(ctx, storage.Mutation{ID: "rmdir", Path: mk.Path, Conditions: storage.Conditions{ExpectedVersion: originalFolderVersion}}, stageOK); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.ContentRoot, string(mk.Path))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty folder remains")
	}
}
func TestMutationReplacementPreservesOldUntilPublish(t *testing.T) {
	w, f, cfg := mutationFixture(t)
	ctx := context.Background()
	writeFixture(t, cfg.ContentRoot, "file", "old complete bytes")
	old := mutationStat(t, w, "file")
	in := writeIntent("replace", "file", "new complete bytes")
	in.Conditions = storage.Conditions{ExpectedVersion: old.Version}
	s := stageFile(t, w.StagedWriter, in, "new complete bytes")
	raw, _ := os.ReadFile(filepath.Join(cfg.ContentRoot, "file"))
	if string(raw) != "old complete bytes" {
		t.Fatal("stage changed visible target")
	}
	w = restartMutation(t, f, cfg)
	result, err := w.Publish(ctx, s, stageOK)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(cfg.ContentRoot, "file"))
	if string(raw) != "new complete bytes" || bytes.Equal(old.Version, result.Entry.Version) {
		t.Fatal("replacement did not switch generation")
	}
	inspectState(t, w.StagedWriter, in, storage.Applied)
	if err := w.Discard(ctx, in, s.Handle); err != nil {
		t.Fatal(err)
	}
}
func TestMutationReplacementConditionAfterDelayedCheckpoint(t *testing.T) {
	for _, mode := range []string{"content", "inode", "parent", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			w, _, cfg := mutationFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, cfg.ContentRoot, "dir/file", "old")
			old := mutationStat(t, w, "dir/file")
			in := writeIntent("replace", "dir/file", "new")
			in.Conditions = storage.Conditions{ExpectedVersion: old.Version}
			s := stageFile(t, w.StagedWriter, in, "new")
			_, err := w.Publish(ctx, s, func(_ context.Context, b []byte) error {
				var r stageReceipt
				_ = json.Unmarshal(b, &r)
				if r.Phase != "publishing" {
					return nil
				}
				switch mode {
				case "content":
					return os.WriteFile(filepath.Join(cfg.ContentRoot, "dir/file"), []byte("external"), 0600)
				case "inode":
					if err := os.Rename(filepath.Join(cfg.ContentRoot, "dir/file"), filepath.Join(cfg.ContentRoot, "oldinode")); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(cfg.ContentRoot, "dir/file"), []byte("old"), 0600)
				case "parent":
					if err := os.Rename(filepath.Join(cfg.ContentRoot, "dir"), filepath.Join(cfg.ContentRoot, "oldparent")); err != nil {
						return err
					}
					return os.Mkdir(filepath.Join(cfg.ContentRoot, "dir"), 0700)
				case "cancel":
					cancel()
				}
				return nil
			})
			if err == nil {
				t.Fatal("lost condition published")
			}
			var proof *storage.NoFinalEffect
			if !errors.As(err, &proof) {
				t.Fatalf("expected no-effect proof: %v", err)
			}
			raw, _ := os.ReadFile(filepath.Join(cfg.ContentRoot, "dir/file"))
			if string(raw) == "new" {
				t.Fatal("lost condition replaced bytes")
			}
		})
	}
}
func TestMutationNoImplicitRecursionAndTypedErrors(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	ctx := context.Background()
	if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, cfg.ContentRoot, "dir/child", "keep")
	folder := mutationStat(t, w, "dir")
	_, err := w.Remove(ctx, storage.Mutation{ID: "not-empty", Path: "dir", Conditions: storage.Conditions{ExpectedVersion: folder.Version}}, stageOK)
	requireCode(t, err, "not_empty")
	raw, _ := os.ReadFile(filepath.Join(cfg.ContentRoot, "dir/child"))
	if string(raw) != "keep" {
		t.Fatal("recursive deletion happened")
	}
	_, err = w.Mkdir(ctx, storage.Mutation{ID: "missing-parent", Path: "missing/folder", Conditions: storage.Conditions{Absent: true}}, stageOK)
	requireCode(t, err, "parent_missing")
	_, err = w.Mkdir(ctx, storage.Mutation{ID: "file-parent", Path: "dir/child/folder", Conditions: storage.Conditions{Absent: true}}, stageOK)
	requireCode(t, err, "not_folder")
	_, err = w.Mkdir(ctx, storage.Mutation{ID: "conflict", Path: "dir", Conditions: storage.Conditions{Absent: true}}, stageOK)
	requireCode(t, err, "name_conflict")
	_, err = w.Copy(ctx, storage.MoveRequest{ID: "copy-folder", Source: "dir", Target: "other", SourceVersion: folder.Version, TargetConditions: storage.Conditions{Absent: true}}, stageOK)
	requireCode(t, err, "unsupported")
	_, err = w.Move(ctx, storage.MoveRequest{ID: "ancestor", Source: "dir", Target: "dir/sub", SourceVersion: folder.Version, TargetConditions: storage.Conditions{Absent: true}}, stageOK)
	requireCode(t, err, "invalid_name")
	if err := os.Symlink(cfg.SystemRoot, filepath.Join(cfg.ContentRoot, "symlink")); err != nil {
		t.Fatal(err)
	}
	_, err = w.Mkdir(ctx, storage.Mutation{ID: "symlink", Path: "symlink/escape", Conditions: storage.Conditions{Absent: true}}, stageOK)
	if err == nil {
		t.Fatal("followed symlink")
	}
}
func TestMutationActionsLostReplyRestartAndTombstones(t *testing.T) {
	for _, action := range []string{"mkdir", "move", "copy", "remove"} {
		t.Run(action, func(t *testing.T) {
			w, f, cfg := mutationFixture(t)
			ctx := context.Background()
			writeFixture(t, cfg.ContentRoot, "source", "bytes")
			e := mutationStat(t, w, "source")
			in := storage.EffectIntent{ID: "lost-" + action, Action: action, Target: "target", Conditions: storage.Conditions{Absent: true}}
			if action == "move" || action == "copy" {
				in.Source = "source"
				in.SourceVersion = e.Version
			}
			if action == "remove" {
				in.Target = "source"
				in.Conditions = storage.Conditions{ExpectedVersion: e.Version}
			}
			var evidence []byte
			_, err := w.runAction(ctx, in, func(_ context.Context, b []byte) error {
				evidence = bytes.Clone(b)
				if actionPhase(b) == "applied" {
					return io.ErrClosedPipe
				}
				return nil
			})
			if err == nil {
				t.Fatal("expected lost reply")
			}
			fresh := restartMutation(t, f, cfg)
			inspection := actionInspection(t, fresh, in, evidence, storage.Applied)
			// Applied terminal ledgers survive subsequent legitimate path reuse.
			if action == "remove" {
				writeFixture(t, cfg.ContentRoot, "source", "later")
			}
			actionInspection(t, fresh, in, inspection.Evidence, storage.Applied)
			if _, err := fresh.runAction(ctx, in, stageOK); err != nil {
				t.Fatal("terminal retry failed", err)
			}
		})
	}
}
func TestMutationNativeErrorAfterRenameRecovery(t *testing.T) {
	for _, action := range []string{"mkdir", "move"} {
		t.Run(action, func(t *testing.T) {
			w, f, cfg := mutationFixture(t)
			ctx := context.Background()
			writeFixture(t, cfg.ContentRoot, "source", "bytes")
			e := mutationStat(t, w, "source")
			in := storage.EffectIntent{ID: action, Action: action, Target: "target", Conditions: storage.Conditions{Absent: true}}
			if action == "move" {
				in.Source = "source"
				in.SourceVersion = e.Version
			}
			w.rename = func(from *os.Root, old string, to *os.Root, name string, replace bool) error {
				if err := actionRename(from, old, to, name, replace); err != nil {
					return err
				}
				return syscall.EIO
			}
			_, err := w.runAction(ctx, in, stageOK)
			if !errors.Is(err, syscall.EIO) {
				t.Fatal(err)
			}
			ins := actionInspection(t, restartMutation(t, f, cfg), in, nil, storage.Applied)
			if ins.Result.Entry == nil || len(ins.Result.Entry.Version) == 0 {
				t.Fatal("recovery lost entry evidence")
			}
		})
	}
}
func TestMutationRecoveryBusyAndStaleWorkers(t *testing.T) {
	w, f, cfg := mutationFixture(t)
	ctx := context.Background()
	in := storage.EffectIntent{ID: "delayed", Action: "mkdir", Target: "target", Conditions: storage.Conditions{Absent: true}}
	entered := make(chan struct{})
	resume := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		_, err := w.runAction(ctx, in, func(_ context.Context, b []byte) error {
			if actionPhase(b) == "executing" {
				close(entered)
				<-resume
				return io.ErrClosedPipe
			}
			return nil
		})
		finished <- err
	}()
	<-entered
	fresh := restartMutation(t, f, cfg)
	actionInspection(t, fresh, in, nil, storage.OutcomeUnknown)
	close(resume)
	if err := <-finished; err == nil {
		t.Fatal("delayed fence accepted")
	}
	actionInspection(t, fresh, in, nil, storage.NotApplied)
	_, err := w.runAction(ctx, in, stageOK)
	if err == nil {
		t.Fatal("stale worker applied after tombstone")
	}
	if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale target created")
	}
	absent := storage.EffectIntent{ID: "never-started", Action: "mkdir", Target: "never", Conditions: storage.Conditions{Absent: true}}
	actionInspection(t, fresh, absent, nil, storage.NotApplied)
	if _, err := w.runAction(ctx, absent, stageOK); err == nil {
		t.Fatal("queued worker bypassed recovery tombstone")
	}
	retained := storage.EffectIntent{ID: "missing-ledger", Action: "mkdir", Target: "retained", Conditions: storage.Conditions{Absent: true}}
	receipt := fresh.actionReceipt(&actionLedger{Intent: retained, Phase: "executing"})
	actionInspection(t, fresh, retained, receipt, storage.OutcomeUnknown)
}
func TestMutationSourceAndTargetChangesDuringCallback(t *testing.T) {
	for _, mode := range []string{"source", "target", "parent"} {
		t.Run(mode, func(t *testing.T) {
			w, _, cfg := mutationFixture(t)
			ctx := context.Background()
			if err := os.Mkdir(filepath.Join(cfg.ContentRoot, "dir"), 0700); err != nil {
				t.Fatal(err)
			}
			writeFixture(t, cfg.ContentRoot, "source", "bytes")
			e := mutationStat(t, w, "source")
			in := storage.EffectIntent{ID: mode, Action: "move", Source: "source", Target: "dir/target", SourceVersion: e.Version, Conditions: storage.Conditions{Absent: true}}
			_, err := w.runAction(ctx, in, func(_ context.Context, b []byte) error {
				if actionPhase(b) != "executing" {
					return nil
				}
				switch mode {
				case "source":
					return os.WriteFile(filepath.Join(cfg.ContentRoot, "source"), []byte("drift"), 0600)
				case "target":
					return os.WriteFile(filepath.Join(cfg.ContentRoot, "dir/target"), []byte("external"), 0600)
				case "parent":
					if err := os.Rename(filepath.Join(cfg.ContentRoot, "dir"), filepath.Join(cfg.ContentRoot, "old")); err != nil {
						return err
					}
					return os.Mkdir(filepath.Join(cfg.ContentRoot, "dir"), 0700)
				}
				return nil
			})
			if err == nil {
				t.Fatal("delayed mutation ignored condition")
			}
			if _, err := os.Stat(filepath.Join(cfg.ContentRoot, "source")); err != nil {
				t.Fatal("source lost")
			}
		})
	}
}
func TestMutationFailedReplacementInputKeepsOldBytes(t *testing.T) {
	w, _, cfg := mutationFixture(t)
	writeFixture(t, cfg.ContentRoot, "file", "old")
	old := mutationStat(t, w, "file")
	for _, body := range []string{"short", "too many bytes"} {
		in := writeIntent("input-"+body, "file", "new bytes")
		in.Conditions = storage.Conditions{ExpectedVersion: old.Version}
		_, err := w.Stage(context.Background(), in, strings.NewReader(body), stageOK)
		if err == nil {
			t.Fatal("bad input accepted")
		}
	}
	raw, _ := os.ReadFile(filepath.Join(cfg.ContentRoot, "file"))
	if string(raw) != "old" {
		t.Fatal("bad input changed target")
	}
}
