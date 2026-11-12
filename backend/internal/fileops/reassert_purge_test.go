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

// A folder re-created while its delete is still purging must survive the purge. The purge's
// live-row check can run before the mkdir's row commits, and a folder adoption writes
// nothing natively, so an ordinary conditional delete would still match the marker. The
// mkdir's reassert moves the native directory identity; the purge's conditional remove then
// refuses it and adopts the newer intent. The test gates the purge at its live-row check so
// the ordering is driven, not raced.
func TestMkdirReassertSurvivesConcurrentFolderPurge(t *testing.T) {
	pool := coordinatorDatabase(t)
	fixture := newCoordinatorFixture(t, pool)
	native, err := localstorage.NewMutationDriver(fixture.driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var mu sync.Mutex
	live := map[storage.Path]bool{}
	checkReached := make(chan struct{})
	releaseCheck := make(chan struct{})
	var gateOnce sync.Once
	livePaths := func(_ context.Context, _ fileops.Root, paths []storage.Path) ([]bool, error) {
		mu.Lock()
		observed := make([]bool, len(paths))
		for i, p := range paths {
			observed[i] = live[p]
		}
		mu.Unlock()
		gated := false
		gateOnce.Do(func() {
			for _, p := range paths {
				if p == "Notes" {
					gated = true
				}
			}
		})
		if gated {
			// This is what the purge observed before the mkdir settled. Hold its delete
			// until the re-creation has run, then answer from the observation.
			close(checkReached)
			<-releaseCheck
			for i := range observed {
				observed[i] = false
			}
		}
		return observed, nil
	}
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
	action := func(req fileops.ActionRequest) fileops.Operation {
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
	action(fileops.ActionRequest{Kind: "mkdir", To: "Notes", Selection: "path:path:Notes"})
	before, err := os.Stat(filepath.Join(fixture.content, "Notes"))
	if err != nil {
		t.Fatal(err)
	}

	deleteRequest := fileops.ActionRequest{Kind: "delete", From: "Notes", Recursive: true, Selection: "path:path", IdempotencyKey: "reassert-delete"}
	deleteRequest.Root, deleteRequest.Actor = fixture.root, fixture.actor
	deleteOp, replay, err := actions.Prepare(ctx, deleteRequest, native)
	if err != nil || replay || deleteOp.Purge == nil {
		t.Fatalf("accept delete replay=%v purge=%v: %v", replay, deleteOp.Purge, err)
	}
	purgeDone := make(chan fileops.Operation, 1)
	purgeErr := make(chan error, 1)
	go func() {
		out, err := actions.Run(ctx, fixture.root, fixture.actor, deleteOp.ID, native)
		purgeDone <- out
		purgeErr <- err
	}()
	<-checkReached

	// The name is free: this mkdir adopts the still-existing marker and must reassert it
	// because the accepted delete covers the path.
	action(fileops.ActionRequest{Kind: "mkdir", To: "Notes", Selection: "path:path:Notes", Reassert: true, IdempotencyKey: "reassert-create"})
	mu.Lock()
	recreatedLive := live["Notes"]
	mu.Unlock()
	if !recreatedLive {
		t.Fatal("re-created mkdir did not settle a live row")
	}
	after, err := os.Stat(filepath.Join(fixture.content, "Notes"))
	if err != nil {
		t.Fatalf("re-created folder is not native: %v", err)
	}
	if os.SameFile(before, after) {
		t.Fatal("mkd ir did not reassert the folder marker")
	}

	close(releaseCheck)
	out := <-purgeDone
	if err := <-purgeErr; err != nil {
		t.Fatalf("purge failed: %v", err)
	}
	if out.State != fileops.Succeeded || out.Purge == nil || out.Purge.Adopted == 0 {
		t.Fatalf("purge did not adopt the re-created marker: state=%s purge=%+v", out.State, out.Purge)
	}
	if _, err = os.Stat(filepath.Join(fixture.content, "Notes")); err != nil {
		t.Fatalf("purge removed the re-created folder: %v", err)
	}
	mu.Lock()
	stillLive := live["Notes"]
	mu.Unlock()
	if !stillLive {
		t.Fatal("re-created folder lost its live row")
	}
	t.Logf("fence: marker survived=%v sameFile=%v live=%v purgeAdopted=%d purgeDeleted=%d",
		true, os.SameFile(before, after), stillLive, out.Purge.Adopted, out.Purge.Deleted)
}
