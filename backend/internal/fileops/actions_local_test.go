package fileops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"everythingabout.me/backend/internal/fileops"
	"everythingabout.me/backend/internal/localstorage"
	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// This exercises actual independent native files, folder identities, durable
// native action receipts, and Postgres index/budget repair together.
func TestActionsLocalPostgres(t *testing.T) {
	pool := coordinatorDatabase(t)
	ctx := context.Background()
	t.Run("native tree actions and no database exit", func(t *testing.T) {
		fixture := newCoordinatorFixture(t, pool)
		native, err := localstorage.NewMutationDriver(fixture.driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
		if err != nil {
			t.Fatal(err)
		}
		var _ fileops.NativeActions = native
		commit := func(_ context.Context, _ pgx.Tx, op fileops.Operation, native *storage.MutationResult) (json.RawMessage, error) {
			if op.State != fileops.Succeeded {
				return nil, nil
			}
			if op.Kind == "delete" {
				return json.RawMessage(`{"deleted":true}`), nil
			}
			if native == nil || native.Entry == nil {
				return nil, errors.New("missing native result")
			}
			return json.Marshal(map[string]any{"path": "/" + string(native.Entry.Path)})
		}
		// This suite exercises native bytes only: the index settlement is modelled as a
		// no-op refund, so the purge is what removes the tree.
		service, err := fileops.NewActions(fixture.journal, fileops.ActionsOptions{
			Authorize: func(context.Context, fileops.Operation) error { return nil },
			Validate:  func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil },
			Commit:    commit,
			// The index is modelled by the native tree here: every object under the prefix is
			// a row holding its size, so the refund is what the purge is about to remove.
			PrefixCommit: func(ctx context.Context, _ pgx.Tx, op fileops.Operation) (int64, error) {
				var req fileops.ActionRequest
				if err := json.Unmarshal(op.Intent, &req); err != nil {
					return 0, err
				}
				var removed int64
				stack := []storage.Path{req.From}
				for len(stack) > 0 {
					dir := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					page, err := native.List(ctx, dir, storage.PageRequest{Limit: 1000})
					if err != nil {
						if storage.IsCode(err, "not_found") {
							continue
						}
						return 0, err
					}
					for _, entry := range page.Entries {
						removed += entry.Size
						if entry.Kind == storage.Folder {
							stack = append(stack, entry.Path)
						}
					}
				}
				return removed, nil
			},
			LivePaths: func(_ context.Context, _ fileops.Root, paths []storage.Path) ([]bool, error) {
				return make([]bool, len(paths)), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		action := func(req fileops.ActionRequest) fileops.Operation {
			t.Helper()
			req.Root = fixture.root
			req.Actor = fixture.actor
			op, replay, err := service.Prepare(ctx, req, native)
			if err != nil || replay {
				t.Fatalf("prepare %s replay %v: %v", req.Kind, replay, err)
			}
			out, err := service.Run(ctx, fixture.root, fixture.actor, op.ID, native)
			if err != nil || out.State != fileops.Succeeded {
				t.Fatalf("run %s state %s problem %s: %v", req.Kind, out.State, out.Problem, err)
			}
			return out
		}
		action(fileops.ActionRequest{Kind: "mkdir", To: "Notes/2026/empty", Parents: true})
		// Seed through the existing durable staged-write protocol so the logical and
		// physical baselines stay exact rather than injecting unaccounted bytes.
		body := []byte("ordinary native bytes\n")
		op := fixture.begin(t, "Notes/2026/file +%.txt", int64(len(body)), nil)
		if _, err = fixture.service(t, nil, commit).Write(ctx, fixture.root, fixture.actor, op.ID, native, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		action(fileops.ActionRequest{Kind: "copy", From: "Notes", To: "Copy", IfRevision: "validated"})
		action(fileops.ActionRequest{Kind: "move", From: "Copy", To: "Moved", IfRevision: "validated"})
		coordinatorBytes(t, filepath.Join(fixture.content, "Notes/2026/file +%.txt"), body)
		coordinatorBytes(t, filepath.Join(fixture.content, "Moved/2026/file +%.txt"), body)
		coordinatorAbsent(t, filepath.Join(fixture.content, "Copy"))
		if info, err := os.Stat(filepath.Join(fixture.content, "Moved/2026/empty")); err != nil || !info.IsDir() {
			t.Fatal("empty folder lost", err)
		}
		original, err := os.Stat(filepath.Join(fixture.content, "Notes/2026/file +%.txt"))
		if err != nil {
			t.Fatal(err)
		}
		copied, err := os.Stat(filepath.Join(fixture.content, "Moved/2026/file +%.txt"))
		if err != nil || os.SameFile(original, copied) {
			t.Fatal("copy is a mutable hardlink", err)
		}
		action(fileops.ActionRequest{Kind: "delete", From: "Notes", Recursive: true, IdempotencyKey: "delete-notes"})
		coordinatorAbsent(t, filepath.Join(fixture.content, "Notes"))
		usage := fixture.usage(t)
		if usage.LogicalBytes != int64(len(body)) || usage.PhysicalBytes != int64(len(body)) || usage.LogicalReserved != 0 || usage.PhysicalReserved != 0 {
			t.Fatal(usage)
		}
		// Reading the native tree above required neither an index lookup nor an export.
	})
	t.Run("native success survives index failure", func(t *testing.T) {
		fixture := newCoordinatorFixture(t, pool)
		native, err := localstorage.NewMutationDriver(fixture.driver, localstorage.WriteOptions{CoordinatedExclusiveWriter: true})
		if err != nil {
			t.Fatal(err)
		}
		failing := true
		injected := errors.New("index transaction rejected")
		makeService := func() *fileops.Actions {
			t.Helper()
			service, err := fileops.NewActions(fileops.NewPostgres(pool), fileops.ActionsOptions{Authorize: func(context.Context, fileops.Operation) error { return nil }, Validate: func(context.Context, fileops.NativeActions, fileops.ActionRequest) error { return nil }, Commit: func(_ context.Context, _ pgx.Tx, _ fileops.Operation, native *storage.MutationResult) (json.RawMessage, error) {
				if failing {
					return nil, injected
				}
				return json.Marshal(map[string]any{"path": "/" + string(native.Entry.Path)})
			}})
			if err != nil {
				t.Fatal(err)
			}
			return service
		}
		service := makeService()
		op, _, err := service.Prepare(ctx, fileops.ActionRequest{Root: fixture.root, Actor: fixture.actor, Kind: "mkdir", To: "real/empty", Parents: true}, native)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = service.Run(ctx, fixture.root, fixture.actor, op.ID, native); !errors.Is(err, injected) {
			t.Fatal(err)
		}
		before, err := native.Stat(ctx, "real/empty")
		if err != nil {
			t.Fatal(err)
		}
		failing = false
		out, err := makeService().Run(ctx, fixture.root, fixture.actor, op.ID, native)
		if err != nil || out.State != fileops.Succeeded {
			t.Fatal(out.State, err)
		}
		after, err := native.Stat(ctx, "real/empty")
		if err != nil || string(before.Revision) != string(after.Revision) {
			t.Fatal("index repair repeated native mkdir", err)
		}
	})
}
