package fileops_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

type countedNativeTreeMover struct {
	fileops.NativeActions
	lists, moves int
}

func (n *countedNativeTreeMover) List(ctx context.Context, p storage.Path, q storage.PageRequest) (storage.Page, error) {
	n.lists++
	return n.NativeActions.List(ctx, p, q)
}
func (n *countedNativeTreeMover) Move(ctx context.Context, in storage.MoveRequest, save storage.Checkpoint) (storage.MutationResult, error) {
	n.moves++
	return n.NativeActions.Move(ctx, in, save)
}

// This intentionally uses real ordinary files rather than synthetic manifest rows:
// directory rename must preserve every descendant inode without listing its tree.
func TestNativeTreeMoveLocalThirtyThousand(t *testing.T) {
	pool := coordinatorDatabase(t)
	fixture := newCoordinatorFixture(t, pool)
	ctx := context.Background()
	source := filepath.Join(fixture.content, "SourceTree")
	for folder := 0; folder < 30; folder++ {
		dir := filepath.Join(source, fmt.Sprintf("folder-%02d", folder))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for file := 0; file < 1000; file++ {
			name := filepath.Join(dir, fmt.Sprintf("file-%04d.txt", file))
			if err := os.WriteFile(name, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	sample := filepath.Join("folder-17", "file-0432.txt")
	before, err := os.Stat(filepath.Join(source, sample))
	if err != nil {
		t.Fatal(err)
	}
	driver, err := localstorage.NewMutationDriver(fixture.driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	native := &countedNativeTreeMover{NativeActions: driver}
	actions, err := fileops.NewActions(fixture.journal, fileops.ActionsOptions{
		Authorize: func(context.Context, fileops.Operation) error { return nil },
		Validate:  func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil },
		Commit: func(_ context.Context, _ pgx.Tx, _ fileops.Operation, result *storage.MutationResult) (json.RawMessage, error) {
			return json.Marshal(map[string]any{"path": "/" + string(result.Entry.Path)})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := fileops.ActionRequest{Root: fixture.root, Actor: fixture.actor, Kind: "move", From: "SourceTree", To: "MovedTree", IfRevision: "validated", IdempotencyKey: "native-30000"}
	op, replay, err := actions.Prepare(ctx, req, native)
	if err != nil || replay {
		t.Fatalf("prepare real native tree move replay=%v: %v", replay, err)
	}
	entries, err := fixture.journal.Entries(ctx, op)
	if err != nil || len(entries) != 1 || entries[0].Action != "move" || native.lists != 0 {
		t.Fatalf("descendant enumeration at admission: entries%d lists%d %v", len(entries), native.lists, err)
	}
	done, err := actions.Run(ctx, fixture.root, fixture.actor, op.ID, native)
	if err != nil || done.State != fileops.Succeeded {
		t.Fatalf("real native tree move state=%s: %v", done.State, err)
	}
	if native.moves != 1 || native.lists != 0 {
		t.Fatalf("native tree calls: moves%d lists%d", native.moves, native.lists)
	}
	coordinatorAbsent(t, source)
	target := filepath.Join(fixture.content, "MovedTree")
	after, err := os.Stat(filepath.Join(target, sample))
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("descendant inode changed: %v", err)
	}
	count := 0
	if err = filepath.WalkDir(target, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 30000 {
		t.Fatalf("native rename retained %d files, want 30000", count)
	}
	saved, replay, err := actions.Prepare(ctx, req, native)
	if err != nil || !replay || saved.ID != done.ID || native.moves != 1 || native.lists != 0 {
		t.Fatalf("large native replay: %v %v moves%d lists%d", replay, err, native.moves, native.lists)
	}
}
