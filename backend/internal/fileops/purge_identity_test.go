package fileops_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// A path can be deleted, re-created and deleted again. Those are two accepted operations,
// so the purge of the second one must get its own native intent: the Local driver journals
// every removal, and a ledger that names a path instead of an operation refuses the second
// delete as intent_conflict after the index has already told the user it is gone. This runs
// the real Local driver through the real purge worker, not a fake native.
func TestPurgeAfterRecreateDeletesTheSecondGeneration(t *testing.T) {
	pool := coordinatorDatabase(t)
	fixture := newCoordinatorFixture(t, pool)
	native, err := localstorage.NewMutationDriver(fixture.driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var mu sync.Mutex
	live := map[storage.Path]bool{}
	commit := func(_ context.Context, _ pgx.Tx, op fileops.Operation, _ *storage.MutationResult) (json.RawMessage, error) {
		var req fileops.ActionRequest
		if err := json.Unmarshal(op.Intent, &req); err != nil {
			return nil, err
		}
		if op.Kind == "mkdir" {
			mu.Lock()
			live[req.To] = true
			mu.Unlock()
		}
		return nil, nil
	}
	prefixCommit := func(_ context.Context, _ pgx.Tx, op fileops.Operation) (int64, error) {
		var req fileops.ActionRequest
		if err := json.Unmarshal(op.Intent, &req); err != nil {
			return 0, err
		}
		mu.Lock()
		for p := range live {
			if p == req.From || strings.HasPrefix(string(p), string(req.From)+"/") {
				delete(live, p)
			}
		}
		mu.Unlock()
		return 0, nil
	}
	livePaths := func(_ context.Context, _ fileops.Root, paths []storage.Path) ([]bool, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]bool, len(paths))
		for i, p := range paths {
			out[i] = live[p]
		}
		return out, nil
	}
	actions, err := fileops.NewActions(fixture.journal, fileops.ActionsOptions{
		Authorize:    func(context.Context, fileops.Operation) error { return nil },
		Validate:     func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil },
		Commit:       commit,
		PrefixCommit: prefixCommit,
		LivePaths:    livePaths,
		LeaseTTL:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := func(req fileops.ActionRequest) fileops.Operation {
		t.Helper()
		req.Root, req.Actor = fixture.root, fixture.actor
		op, replay, err := actions.Prepare(ctx, req, native)
		if err != nil || replay {
			t.Fatalf("prepare %s replay=%v: %v", req.Kind, replay, err)
		}
		out, err := actions.Run(ctx, fixture.root, fixture.actor, op.ID, native)
		if err != nil || out.State != fileops.Succeeded {
			t.Fatalf("run %s state=%s problem=%s: %v", req.Kind, out.State, out.Problem, err)
		}
		return out
	}
	write := func(body string) {
		t.Helper()
		path := filepath.Join(fixture.content, "Folder", "notes.txt")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		// A purge adopts anything written after its acceptance, because that is somebody's
		// newer intent. The fixture's file must be older than the delete that removes it.
		old := time.Now().Add(-time.Minute)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	create := func(key string) {
		run(fileops.ActionRequest{Kind: "mkdir", To: "Folder", Selection: "path:path:Folder", IdempotencyKey: key})
	}
	remove := func(key string) fileops.Operation {
		return run(fileops.ActionRequest{Kind: "delete", From: "Folder", Recursive: true, Selection: "path:path", IdempotencyKey: key})
	}

	create("create-1")
	write("first generation")
	first := remove("delete-1")
	if _, err := os.Stat(filepath.Join(fixture.content, "Folder")); !os.IsNotExist(err) {
		t.Fatalf("first purge left the folder: %v (problem=%s)", err, first.Problem)
	}

	create("create-2")
	write("second generation")
	second := remove("delete-2")
	if data, err := os.ReadFile(filepath.Join(fixture.content, "Folder", "notes.txt")); err == nil {
		t.Fatalf("second purge left %q on disk (problem=%s)", data, second.Problem)
	}
	if _, err := os.Stat(filepath.Join(fixture.content, "Folder")); !os.IsNotExist(err) {
		t.Fatalf("second purge left the re-created folder: %v (problem=%s)", err, second.Problem)
	}
	if first.ID == second.ID {
		t.Fatal("the fixture reused one operation id for two deletions")
	}
}
