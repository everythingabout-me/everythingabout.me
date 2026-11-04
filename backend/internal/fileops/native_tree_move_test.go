package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

type nativeTreeMoveTestDriver struct {
	*actionNative
	moves, inspections    int
	loseReply, unresolved bool
}

func (n *nativeTreeMoveTestDriver) Describe() storage.Descriptor {
	d := n.actionNative.Describe()
	d.NativeTreeMove = true
	return d
}
func (n *nativeTreeMoveTestDriver) Move(ctx context.Context, in storage.MoveRequest, save storage.Checkpoint) (storage.MutationResult, error) {
	n.moves++
	effect := storage.EffectIntent{ID: in.ID, Action: "move", Source: in.Source, Target: in.Target, SourceVersion: in.SourceVersion, Conditions: in.TargetConditions}
	n.calls = append(n.calls, effect)
	if err := save(ctx, []byte("durable-native-move")); err != nil {
		return storage.MutationResult{}, err
	}
	source, ok := n.entries[in.Source]
	if !ok {
		return storage.MutationResult{}, storage.NewError("not_found", in.Source, nil)
	}
	if _, exists := n.entries[in.Target]; exists {
		return storage.MutationResult{}, storage.NewError("name_conflict", in.Target, nil)
	}
	for p, e := range n.entries {
		if p != in.Source && !strings.HasPrefix(string(p), string(in.Source)+"/") {
			continue
		}
		target := in.Target + storage.Path(strings.TrimPrefix(string(p), string(in.Source)))
		e.Path = target
		n.entries[target] = e
		n.data[target] = n.data[p]
		delete(n.entries, p)
		delete(n.data, p)
	}
	source.Path = in.Target
	result := storage.MutationResult{Entry: &source, Evidence: []byte("native move complete and quiescent")}
	n.receipts[in.ID] = storage.Inspection{State: storage.Applied, Result: &result, Evidence: result.Evidence}
	if n.loseReply {
		return storage.MutationResult{}, errors.New("native reply lost")
	}
	return result, nil
}
func (n *nativeTreeMoveTestDriver) InspectEffect(_ context.Context, in storage.EffectIntent, _ []byte) (storage.Inspection, error) {
	n.inspections++
	if n.unresolved {
		return storage.Inspection{State: storage.OutcomeUnknown, Evidence: []byte("native move still unresolved")}, nil
	}
	if receipt, found := n.receipts[in.ID]; found {
		return receipt, nil
	}
	return storage.Inspection{State: storage.NotApplied, Evidence: []byte("native move never executed")}, nil
}
func nativeTreeFixture(t *testing.T, p *Postgres, r Root, actor Actor) (*Actions, *nativeTreeMoveTestDriver, ActionRequest) {
	t.Helper()
	native := &nativeTreeMoveTestDriver{actionNative: newActionNative(r)}
	native.put("source", storage.Folder, "")
	native.put("source/child", storage.Folder, "")
	native.put("source/child/file", storage.File, "")
	req := ActionRequest{Root: r, Actor: actor, Kind: "move", From: "source", To: "target", IfRevision: "validated", IdempotencyKey: "native-tree-move"}
	return actionCoordinator(t, p, nil), native, req
}

func TestNativeTreeMoveCapture(t *testing.T) {
	root := testRoot()
	native := &nativeTreeMoveTestDriver{actionNative: newActionNative(root)}
	native.put("source", storage.Folder, "")
	native.put("source/child", storage.Folder, "")
	native.put("source/child/file", storage.File, "payload")
	req := ActionRequest{Root: root, Actor: testActor(root), Kind: "move", From: "source", To: "target", IfRevision: "validated"}
	binding, manifest, reservation, err := captureAction(context.Background(), req, native)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Strategy != nativeTreeMoveStrategy || len(binding.Sources) != 1 || len(manifest) != 1 || manifest[0].Action != "move" || manifest[0].SourceEntry == nil || manifest[0].SourceEntry.Kind != storage.Folder {
		t.Fatalf("root-only capture: %+v %+v", binding, manifest)
	}
	if !equalJSON(reservation, ReservationRequest{}) {
		t.Fatalf("native rename reserved duplicate bytes: %+v", reservation)
	}
	if err = validateCapture(context.Background(), req, native, binding, manifest); err != nil {
		t.Fatal(err)
	}
	if native.lists != 0 {
		t.Fatalf("native rename listed descendants %d times", native.lists)
	}
	_, manifest, _, err = captureAction(context.Background(), req, native.actionNative)
	if err != nil || len(manifest) <= 1 || native.lists == 0 {
		t.Fatalf("non-native fallback lost finite manifest: %d %v", len(manifest), err)
	}
	for _, target := range []storage.Path{"longer-target", "p/a"} {
		native.put("p", storage.Folder, "")
		native.lists = 0
		req.To = target
		binding, manifest, _, err = captureAction(context.Background(), req, native)
		if err != nil || binding.Strategy != "" || len(manifest) <= 1 || native.lists == 0 {
			t.Fatalf("growing destination bypassed descendant validation: %s strategy=%q entries=%d lists=%d %v", target, binding.Strategy, len(manifest), native.lists, err)
		}
	}
	req.From, req.To = "source/child/file", "file"
	binding, manifest, _, err = captureAction(context.Background(), req, native)
	if err != nil || binding.Strategy != "" || len(manifest) != 2 || manifest[0].Action != "copy" || manifest[1].Action != "delete" {
		t.Fatalf("file move unexpectedly selected tree strategy: %+v %v", manifest, err)
	}
}

func TestNativeTreeMovePostgres(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("single durable root effect and zero quota delta", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		actions, native, req := nativeTreeFixture(t, p, r, actor)
		op := prepareAction(t, actions, req, native)
		if len(op.Entries) != 1 || op.Entries[0].Action != "move" || len(op.Guards) != 2 || !op.Guards[0].Subtree || !op.Guards[1].Subtree {
			t.Fatalf("native move journal: %+v", op)
		}
		before := mustUsage(t, p, r, actor)
		done := runAction(t, actions, op, actor, native)
		if done.State != Succeeded || native.moves != 1 || native.lists != 0 {
			t.Fatalf("native move state=%s moves=%d lists=%d", done.State, native.moves, native.lists)
		}
		if _, ok := native.entries["target/child/file"]; !ok {
			t.Fatal("native subtree not moved")
		}
		if after := mustUsage(t, p, r, actor); before != after {
			t.Fatalf("rename changed usage: %+v -> %+v", before, after)
		}
		replay, found, err := actions.Prepare(ctx, req, native)
		if err != nil || !found || replay.ID != done.ID || native.moves != 1 {
			t.Fatalf("terminal replay repeated rename: %v %v", found, err)
		}
	})
	t.Run("cancellation and changed folder identity stop before rename", func(t *testing.T) {
		for _, cancel := range []bool{true, false} {
			p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
			actions, native, req := nativeTreeFixture(t, p, r, actor)
			op := prepareAction(t, actions, req, native)
			if cancel {
				if _, err := actions.Cancel(ctx, r, actor, op.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				native.put("source", storage.Folder, "")
			}
			done, err := actions.Run(ctx, r, actor, op.ID, native)
			if err != nil || !done.Terminal() || done.State == Succeeded || native.moves != 0 || native.lists != 0 {
				t.Fatalf("unsafe native rename after cancel=%t: state=%s moves%d lists%d %v", cancel, done.State, native.moves, native.lists, err)
			}
			if _, exists := native.entries["source/child/file"]; !exists {
				t.Fatal("stopped rename changed descendants")
			}
		}
	})
	t.Run("unknown native move retains guards and only inspects", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		actions, native, req := nativeTreeFixture(t, p, r, actor)
		native.loseReply, native.unresolved = true, true
		op := prepareAction(t, actions, req, native)
		unknown, err := actions.Run(ctx, r, actor, op.ID, native)
		if !IsCode(err, "outcome_unknown") || unknown.State != Unknown || native.moves != 1 {
			t.Fatalf("lost rename: %s %v", unknown.State, err)
		}
		blocked := ActionRequest{Root: r, Actor: actor, Kind: "mkdir", To: "target/conflict", IdempotencyKey: "conflicting-create"}
		_, _, err = actions.Prepare(ctx, blocked, native)
		requireCode(t, err, "operation_in_progress")
		_, err = actions.Run(ctx, r, actor, op.ID, native)
		if !IsCode(err, "outcome_unknown") || native.moves != 1 || native.lists != 0 {
			t.Fatalf("unknown recovery replayed effect: moves=%d lists=%d %v", native.moves, native.lists, err)
		}
		native.unresolved = false
		done := runAction(t, actions, op, actor, native)
		if done.State != Succeeded || native.moves != 1 || native.inspections < 3 || native.lists != 0 {
			t.Fatalf("native recovery: %s moves%d inspections%d lists%d", done.State, native.moves, native.inspections, native.lists)
		}
	})
	t.Run("index retry observes root without repeating native move", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		_, native, req := nativeTreeFixture(t, p, r, actor)
		fail := true
		commit := func(_ context.Context, _ pgx.Tx, _ Operation, result *storage.MutationResult) (json.RawMessage, error) {
			if fail {
				return nil, errors.New("index temporarily unavailable")
			}
			return json.Marshal(map[string]any{"path": string(result.Entry.Path)})
		}
		actions := actionCoordinator(t, p, commit)
		op := prepareAction(t, actions, req, native)
		_, err := actions.Run(ctx, r, actor, op.ID, native)
		applied, readErr := p.Get(ctx, r, actor, op.ID)
		if err == nil || readErr != nil || applied.Phase != NativeApplied || native.moves != 1 {
			t.Fatalf("index failure: phase=%s moves=%d err=%v readErr=%v", applied.Phase, native.moves, err, readErr)
		}
		fail = false
		fresh := actionCoordinator(t, NewPostgres(pool), commit)
		done := runAction(t, fresh, op, actor, native)
		if done.State != Succeeded || native.moves != 1 || native.lists != 0 {
			t.Fatalf("index recovery repeated move: %s moves%d lists%d", done.State, native.moves, native.lists)
		}
	})
}
