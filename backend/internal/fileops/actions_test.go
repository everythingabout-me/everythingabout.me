package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
)

// The fake implements the same bound native operation contract as Local. Native
// receipts survive coordinator reconstruction, and unknown is never an existence
// check masquerading as a terminal outcome.
type actionNative struct {
	NativeActions
	root        Root
	entries     map[storage.Path]storage.Entry
	data        map[storage.Path]string
	receipts    map[string]storage.Inspection
	calls       []storage.EffectIntent
	serial      int
	statMu      sync.Mutex
	beforeStat  func(storage.Path)
	afterEffect func(storage.EffectIntent)
	failAt      int
	failWith    error
	unknownAt   int
	lostAt      int
	pageSize    int
	lists       int
	purges      int
	purgeMu     sync.Mutex
}

func newActionNative(root Root) *actionNative {
	n := &actionNative{root: root, entries: map[storage.Path]storage.Entry{}, data: map[storage.Path]string{}, receipts: map[string]storage.Inspection{}, pageSize: 2}
	n.put("", storage.Folder, "")
	// A deletion settles the index before it purges bytes. This package does not model the
	// index, so the newest fake tree answers the settlement's size question: the bytes a
	// delete refunds are the sizes of the objects the purge is about to remove.
	prefixHost = n
	return n
}
func (n *actionNative) put(p storage.Path, kind storage.Kind, data string) {
	n.serial++
	size := int64(len(data))
	if kind == storage.Folder {
		size = 0
	}
	n.entries[p] = storage.Entry{Path: p, Kind: kind, Size: size, Revision: []byte(strconv.Itoa(n.serial)), NativeRef: []byte(fmt.Sprintf("inode-%d", n.serial))}
	n.data[p] = data
}
func (n *actionNative) StorageBinding() storage.Config {
	return storage.Config{OwnerID: n.root.OwnerID, ConnectionID: n.root.ConnectionID, Generation: n.root.Generation, ContentRoot: n.root.ContentRoot, SystemRoot: n.root.SystemRoot}
}
func (n *actionNative) StorageNameRules() storage.NameRules {
	return storage.NameRules{CaseSensitive: n.root.CaseSensitive, MaxDepth: 256, MaxPathBytes: 4096, MaxSegmentBytes: 255}
}
func (n *actionNative) Describe() storage.Descriptor {
	return storage.Descriptor{AtomicCreate: true, NativeConditionalMutation: true, RetainsVersions: n.root.RetainsVersions, Names: n.StorageNameRules()}
}
func (n *actionNative) Close() error { return nil }
func (n *actionNative) Stat(_ context.Context, p storage.Path) (storage.Entry, error) {
	// Observations run concurrently, so the hook must not be entered twice at once:
	// tests use it to count or to mutate the fake from inside a single step.
	if n.beforeStat != nil {
		n.statMu.Lock()
		n.beforeStat(p)
		n.statMu.Unlock()
	}
	e, ok := n.entries[p]
	if !ok {
		return e, storage.NewError("not_found", p, nil)
	}
	return e, nil
}
func (n *actionNative) List(_ context.Context, p storage.Path, req storage.PageRequest) (storage.Page, error) {
	n.lists++
	var entries []storage.Entry
	for path, e := range n.entries {
		if path != "" && parentPath(path) == p {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	offset := 0
	if req.Cursor != "" {
		offset, _ = strconv.Atoi(req.Cursor)
	}
	end := min(offset+n.pageSize, len(entries))
	page := storage.Page{Entries: entries[offset:end]}
	if end < len(entries) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}
func (n *actionNative) Mkdir(ctx context.Context, m storage.Mutation, save storage.Checkpoint) (storage.MutationResult, error) {
	return n.effect(ctx, storage.EffectIntent{ID: m.ID, Action: "mkdir", Target: m.Path, Conditions: m.Conditions}, save)
}

// Purge is the cheap settled deletion: no receipt, no observation, one map operation. It
// exists so the purge's fast path is the one the tests exercise.
func (n *actionNative) Purge(_ context.Context, p storage.Path, kind storage.Kind, revision []byte) error {
	n.purgeMu.Lock()
	defer n.purgeMu.Unlock()
	n.purges++
	entry, ok := n.entries[p]
	if !ok {
		return storage.NewError("not_found", p, nil)
	}
	if len(revision) > 0 && !bytes.Equal(entry.Revision, revision) {
		return storage.NewError("changed", p, nil)
	}
	if kind == storage.Folder {
		for other := range n.entries {
			if other != p && strings.HasPrefix(string(other), string(p)+"/") {
				return storage.NewError("not_empty", p, nil)
			}
		}
	}
	delete(n.entries, p)
	delete(n.data, p)
	return nil
}

func (n *actionNative) Remove(ctx context.Context, m storage.Mutation, save storage.Checkpoint) (storage.MutationResult, error) {
	return n.effect(ctx, storage.EffectIntent{ID: m.ID, Action: "delete", Source: m.Path, SourceRevision: m.Conditions.ExpectedRevision}, save)
}
func (n *actionNative) Copy(ctx context.Context, m storage.MoveRequest, save storage.Checkpoint) (storage.MutationResult, error) {
	return n.effect(ctx, storage.EffectIntent{ID: m.ID, Action: "copy", Source: m.Source, Target: m.Target, SourceRevision: m.SourceRevision, Conditions: m.TargetConditions}, save)
}
func (n *actionNative) Move(context.Context, storage.MoveRequest, storage.Checkpoint) (storage.MutationResult, error) {
	return storage.MutationResult{}, errors.New("service must use captured copy/verify/delete")
}
func (n *actionNative) InspectEffect(_ context.Context, i storage.EffectIntent, _ []byte) (storage.Inspection, error) {
	if receipt, ok := n.receipts[i.ID]; ok {
		return receipt, nil
	}
	return storage.Inspection{State: storage.NotApplied, Evidence: []byte("quiescent unused native intent")}, nil
}
func (n *actionNative) effect(ctx context.Context, i storage.EffectIntent, save storage.Checkpoint) (storage.MutationResult, error) {
	if err := save(ctx, []byte(i.ID)); err != nil {
		return storage.MutationResult{}, err
	}
	n.calls = append(n.calls, i)
	call := len(n.calls)
	fail := func(err error) (storage.MutationResult, error) {
		n.receipts[i.ID] = storage.Inspection{State: storage.NotApplied, Evidence: []byte("terminal failure")}
		return storage.MutationResult{}, err
	}
	if n.unknownAt == call {
		n.receipts[i.ID] = storage.Inspection{State: storage.OutcomeUnknown, Evidence: []byte("request may arrive")}
		return storage.MutationResult{}, errors.New("lost response")
	}
	if n.failAt == call {
		if n.failWith != nil {
			return fail(n.failWith)
		}
		return fail(errors.New("injected native failure"))
	}
	if i.Source != "" {
		e, ok := n.entries[i.Source]
		if !ok || !bytes.Equal(e.Revision, i.SourceRevision) {
			return fail(storage.NewError("changed", i.Source, nil))
		}
	}
	if i.Target != "" {
		if _, ok := n.entries[i.Target]; ok {
			return fail(storage.NewError("name_conflict", i.Target, nil))
		}
		parent, ok := n.entries[parentPath(i.Target)]
		if !ok || parent.Kind != storage.Folder {
			return fail(storage.NewError("parent_missing", i.Target, nil))
		}
	}
	result := storage.MutationResult{Evidence: []byte("terminal native receipt:" + i.ID)}
	switch i.Action {
	case "mkdir":
		if !i.Conditions.Absent {
			return fail(errors.New("mkdir not create-only"))
		}
		n.put(i.Target, storage.Folder, "")
	case "copy":
		if !i.Conditions.Absent {
			return fail(errors.New("copy not create-only"))
		}
		if n.entries[i.Source].Kind != storage.File {
			return fail(errors.New("recursive native copy forbidden"))
		}
		n.put(i.Target, storage.File, n.data[i.Source])
	case "delete":
		for path := range n.entries {
			if path != "" && parentPath(path) == i.Source {
				return fail(storage.NewError("directory_not_empty", i.Source, nil))
			}
		}
		delete(n.entries, i.Source)
		delete(n.data, i.Source)
	}
	if i.Target != "" {
		e := n.entries[i.Target]
		result.Entry = &e
	}
	n.receipts[i.ID] = storage.Inspection{State: storage.Applied, Result: &result, Evidence: result.Evidence}
	if n.afterEffect != nil {
		n.afterEffect(i)
	}
	if n.lostAt == call {
		return storage.MutationResult{}, errors.New("response lost after native success")
	}
	return result, nil
}
func actionCoordinator(t *testing.T, p *Postgres, commit IndexCommit) *Actions {
	t.Helper()
	if commit == nil {
		commit = func(_ context.Context, _ pgx.Tx, op Operation, _ *storage.MutationResult) (json.RawMessage, error) {
			if op.State != Succeeded {
				return nil, nil
			}
			if op.Kind == "delete" {
				return json.RawMessage(`{"deleted":true}`), nil
			}
			var req ActionRequest
			if err := json.Unmarshal(op.Intent, &req); err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"path": "/" + string(req.To)})
		}
	}
	a, err := NewActions(p, ActionsOptions{
		Authorize: func(context.Context, Operation) error { return nil },
		Validate:  func(context.Context, NativeActions, ActionRequest) error { return nil },
		Commit:    commit,
		// The index is out of scope for this package, so the fake settlement reports the
		// bytes the index holds for the subtree (the fake tree's sizes) and leaves the tree
		// alone: the purge is what removes the objects, exactly as in production.
		PrefixCommit: func(_ context.Context, _ pgx.Tx, op Operation) (int64, json.RawMessage, error) {
			var req ActionRequest
			if err := json.Unmarshal(op.Intent, &req); err != nil {
				return 0, nil, err
			}
			if prefixHost != nil {
				var removed int64
				for path, entry := range prefixHost.entries {
					if path == req.From || strings.HasPrefix(string(path), string(req.From)+"/") {
						removed += entry.Size
					}
				}
				return removed, json.RawMessage(`{"deleted":true}`), nil
			}
			return 0, json.RawMessage(`{"deleted":true}`), nil
		},
		LivePaths: func(_ context.Context, _ Root, paths []storage.Path) ([]bool, error) {
			live := make([]bool, len(paths))
			for i, path := range paths {
				live[i] = liveOverride[path]
			}
			return live, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// prefixHost is the fake native tree the accept-time settlement reads sizes from. The
// index is not modelled in this package; "the rows leave the index" therefore only needs
// the bytes, which the purge then deletes from the tree.
var prefixHost *actionNative

// liveOverride names paths an index would hold again after the deletion was accepted: the
// tests use it to model a user re-creating something while the purge runs.
var liveOverride = map[storage.Path]bool{}

func prepareAction(t *testing.T, a *Actions, req ActionRequest, n NativeActions) Operation {
	t.Helper()
	op, replayed, err := a.Prepare(context.Background(), req, n)
	if err != nil || replayed {
		t.Fatalf("prepare %s replay=%v: %v", req.Kind, replayed, err)
	}
	return op
}
func runAction(t *testing.T, a *Actions, op Operation, actor Actor, n NativeActions) Operation {
	t.Helper()
	out, err := a.Run(context.Background(), op.Root, actor, op.ID, n)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func TestActionsPostgres(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("guard before observation and explicit topdown parents", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		n := newActionNative(r)
		a := actionCoordinator(t, p, nil)
		observed := false
		n.beforeStat = func(path storage.Path) {
			if observed {
				return
			}
			observed = true
			_, _, err := p.Begin(ctx, genericRequest(r, actor, "racing", 0))
			requireCode(t, err, "operation_in_progress")
		}
		req := ActionRequest{Root: r, Actor: actor, Kind: "mkdir", To: "one/two/three", Parents: true, IdempotencyKey: "mkdir"}
		op := prepareAction(t, a, req, n)
		if len(op.Entries) != 3 || op.Entries[0].Target != "one" || op.Entries[2].Target != req.To {
			t.Fatal(op.Entries)
		}
		out := runAction(t, a, op, actor, n)
		if out.State != Succeeded || string(out.Result) != `{"path":"/one/two/three"}` {
			t.Fatal(out.State, string(out.Result))
		}
		calls := len(n.calls)
		req.Actor.CanRead = false
		again, replayed, err := a.Prepare(ctx, req, n)
		if err != nil || !replayed || again.ID != op.ID || len(n.calls) != calls {
			t.Fatal("immutable intent replay", replayed, err)
		}
		req.To = "another"
		_, _, err = a.Prepare(ctx, req, n)
		requireCode(t, err, "idempotency_conflict")
	})
	t.Run("guards fence only the affected paths", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(4, 4, 100, 100))
		n := newActionNative(r)
		n.put("trash", storage.Folder, "")
		n.put("trash/old.bin", storage.File, "old")
		n.put("keep", storage.Folder, "")
		n.put("keep/a.txt", storage.File, "a")
		a := actionCoordinator(t, p, nil)
		// An open upload elsewhere holds its target guard for its whole session.
		upload := mustBegin(t, p, genericRequest(r, actor, "keep/a.txt", 0))
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "trash/old.bin"}, n)
		if len(op.Guards) != 1 || op.Guards[0].Path != "trash/old.bin" || op.Guards[0].Subtree {
			t.Fatalf("delete guards = %+v", op.Guards)
		}
		if out := runAction(t, a, op, actor, n); out.State != Succeeded {
			t.Fatalf("unrelated upload blocked the delete: %s", out.State)
		}
		// The same path must still conflict, and must name the blocking operation.
		_, _, err := a.Prepare(ctx, ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "keep/a.txt"}, n)
		requireCode(t, err, "operation_in_progress")
		var conflict *Error
		if !errors.As(err, &conflict) || conflict.OperationID != upload.ID {
			t.Fatalf("conflict lost the blocking operation: %v", err)
		}
	})
	t.Run("a recursive delete settles uploads abandoned inside it", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		n := newActionNative(r)
		n.put("import", storage.Folder, "")
		n.put("import/day.json", storage.File, "{}")
		a := actionCoordinator(t, p, nil)
		// Sessions an import allocated and then abandoned before committing. Each holds its
		// target path until the session expires 24 hours later, which is what made the folder
		// above them undeletable: the browser cancels one blocker per retry.
		abandoned := []Upload{
			mustUpload(t, p, r, actor, "import/a.json", 4, "import-a"),
			mustUpload(t, p, r, actor, "import/b.json", 4, "import-b"),
		}
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "import", Recursive: true, IdempotencyKey: "purge"}, n)
		if out := runAction(t, a, op, actor, n); out.State != Succeeded {
			t.Fatalf("delete of a folder holding abandoned uploads: %s", out.State)
		}
		for _, upload := range abandoned {
			settled, err := p.GetUpload(ctx, r, actor, upload.ID)
			if err != nil || settled.State != UploadAborted || settled.Operation.State != Cancelled {
				t.Fatalf("session %s not settled: %v %+v", upload.ID, err, settled)
			}
			var guards int
			if err = pool.QueryRow(ctx, `select count(*) from file_operation_guards where owner_id=$1::uuid and operation_id=$2::uuid`, r.OwnerID, upload.ID).Scan(&guards); err != nil || guards != 0 {
				t.Fatalf("guards left on %s: %d %v", upload.ID, guards, err)
			}
		}
	})
	t.Run("a committing upload keeps the folder fenced", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		n := newActionNative(r)
		n.put("import", storage.Folder, "")
		n.put("import/day.json", storage.File, "{}")
		a := actionCoordinator(t, p, nil)
		// A session whose parts are all received is committing: its publication may already
		// be native, so the delete must name it instead of cancelling it out from under itself.
		committing := mustUpload(t, p, r, actor, "import/a.json", 4, "import-a")
		mustReceipt(t, p, r, actor, committing, 1, 4)
		if _, err := p.FreezeUpload(ctx, r, actor, committing.ID); err != nil {
			t.Fatal(err)
		}
		_, _, err := a.Prepare(ctx, ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "import", Recursive: true, IdempotencyKey: "purge"}, n)
		requireCode(t, err, "operation_in_progress")
		var conflict *Error
		if !errors.As(err, &conflict) || conflict.OperationID != committing.ID {
			t.Fatalf("conflict lost the committing upload: %v", err)
		}
		still, err := p.GetUpload(ctx, r, actor, committing.ID)
		if err != nil || still.State != UploadCommitting {
			t.Fatalf("committing session was altered: %v %+v", err, still)
		}
	})
	t.Run("recursive copy finite paginated independent and charged", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(7, 7, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/a", storage.File, "abc")
		n.put("src/b", storage.File, "1234")
		n.put("src/empty", storage.Folder, "")
		a := actionCoordinator(t, p, nil)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "copy", From: "src", To: "dst", IfRevision: "revision"}, n)
		if u := mustUsage(t, p, r, actor); u.LogicalReserved != 7 || u.PhysicalReserved != 7 {
			t.Fatal(u)
		}
		out := runAction(t, a, op, actor, n)
		if out.State != Succeeded || n.lists < 4 {
			t.Fatal(out.State, n.lists)
		}
		n.put("src/a", storage.File, "changed")
		if n.data["dst/a"] != "abc" || n.entries["dst/empty"].Kind != storage.Folder {
			t.Fatal("copy aliased source or lost empty folder")
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 14 || u.PhysicalBytes != 14 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("move copies and verifies all destinations before any source deletion", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(5, 5, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/sub", storage.Folder, "")
		n.put("src/sub/a", storage.File, "hello")
		a := actionCoordinator(t, p, nil)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev"}, n)
		out := runAction(t, a, op, actor, n)
		if out.State != Succeeded {
			t.Fatal(out.State, string(out.Problem))
		}
		if _, ok := n.entries["src"]; ok || n.data["dst/sub/a"] != "hello" {
			t.Fatal(n.entries)
		}
		deleting := false
		for _, call := range n.calls {
			if call.Action == "delete" {
				deleting = true
			} else if deleting {
				t.Fatal("copy after source deletion")
			}
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 5 || u.PhysicalBytes != 5 || u.LogicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("changed destination or source folder blocks move deletes", func(t *testing.T) {
		for _, changed := range []string{"destination", "source-folder"} {
			t.Run(changed, func(t *testing.T) {
				p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
				n := newActionNative(r)
				n.put("src", storage.Folder, "")
				n.put("src/a", storage.File, "abc")
				a := actionCoordinator(t, p, nil)
				op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev"}, n)
				n.afterEffect = func(i storage.EffectIntent) {
					if i.Action == "copy" {
						if changed == "destination" {
							n.put("dst/a", storage.File, "bad")
						} else {
							n.put("src", storage.Folder, "")
						}
					}
				}
				out := runAction(t, a, op, actor, n)
				if out.State != Failed || n.data["src/a"] != "abc" || !bytes.Contains(out.Problem, []byte("target_changed")) {
					t.Fatal(out.State, string(out.Problem))
				}
				for _, call := range n.calls {
					if call.Action == "delete" {
						t.Fatal("source deleted after captured binding changed")
					}
				}
			})
		}
	})
	t.Run("move proves its destinations once instead of once per delete", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(4, 4, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/sub", storage.Folder, "")
		names := []string{"a", "b", "c", "d"}
		for _, name := range names {
			n.put(storage.Path("src/sub/"+name), storage.File, name)
		}
		var copied atomic.Bool
		observed := map[storage.Path]int{}
		n.beforeStat = func(path storage.Path) {
			if copied.Load() {
				observed[path]++
			}
		}
		n.afterEffect = func(i storage.EffectIntent) {
			if i.Target == "dst/sub/d" {
				copied.Store(true)
			}
		}
		a := actionCoordinator(t, p, nil)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev"}, n)
		out := runAction(t, a, op, actor, n)
		if out.State != Succeeded {
			t.Fatal(out.State, string(out.Problem))
		}
		// After every copy has applied, each destination is re-read once: one proof of
		// the whole set before the first removal. Re-reading the set before every delete
		// made a move quadratic in the number of files.
		for _, name := range names {
			if got := observed[storage.Path("dst/sub/"+name)]; got != 1 {
				t.Fatalf("%s observed %d times after copying, want 1", name, got)
			}
		}
	})
	t.Run("a write into an accepted deletion loses with the folder", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/a", storage.File, "abc")
		a := actionCoordinator(t, p, nil)
		req := ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "src", Recursive: true, IdempotencyKey: "delete"}
		op := prepareAction(t, a, req, n)
		n.afterEffect = func(i storage.EffectIntent) {
			if i.Action == "delete" && i.Source == "src/a" {
				n.put("src/new", storage.File, "outside")
			}
		}
		out := runAction(t, a, op, actor, n)
		// The folder stopped existing for every read when the deletion was accepted, so a
		// file written into it afterwards was never visible to anybody: it goes with the
		// folder instead of keeping it alive. This is the rule that replaced "never rescan
		// and remove new external additions".
		if out.State != Succeeded || n.data["src/new"] != "" {
			t.Fatal(out.State, string(out.Result), n.data["src/new"])
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 0 || u.PhysicalBytes != 0 || u.LogicalReserved != 0 {
			t.Fatal(u)
		}
		again, replayed, err := a.Prepare(ctx, req, n)
		if err != nil || !replayed || again.ID != op.ID {
			t.Fatal(replayed, err)
		}
	})
	t.Run("cancel stops pending entries settles partial and frees unused credit", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(4, 4, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/a", storage.File, "abcd")
		a := actionCoordinator(t, p, nil)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev"}, n)
		n.afterEffect = func(i storage.EffectIntent) {
			if i.Action == "copy" {
				if _, err := a.Cancel(ctx, r, actor, op.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		out := runAction(t, a, op, actor, n)
		if out.State != Cancelled || !explicitRemaining(out.Result) || n.data["src/a"] != "abcd" || n.data["dst/a"] != "abcd" {
			t.Fatal(out.State, string(out.Result))
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 8 || u.PhysicalBytes != 8 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
		var claims int
		if err := pool.QueryRow(ctx, `select count(*) from file_credit_claims where operation_id=$1`, op.ID).Scan(&claims); err != nil || claims != 0 {
			t.Fatal(claims, err)
		}
	})
	t.Run("unknown cancellation retains guards then restart inspection settles", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.File, "abc")
		n.unknownAt = 1
		a := actionCoordinator(t, p, nil)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "copy", From: "src", To: "dst", IfRevision: "rev"}, n)
		unknown, err := a.Run(ctx, r, actor, op.ID, n)
		requireCode(t, err, "outcome_unknown")
		if unknown.State != Unknown || unknown.Terminal() {
			t.Fatal(unknown.State)
		}
		if _, err = a.Cancel(ctx, r, actor, op.ID); err != nil {
			t.Fatal(err)
		}
		_, _, err = p.Begin(ctx, genericRequest(r, actor, "dst/child", 0))
		requireCode(t, err, "operation_in_progress")
		id := n.calls[0].ID
		n.receipts[id] = storage.Inspection{State: storage.NotApplied, Evidence: []byte("old request fenced and unused")}
		restarted := actionCoordinator(t, NewPostgres(pool), nil)
		out := runAction(t, restarted, op, actor, n)
		if out.State != Cancelled || len(n.calls) != 1 {
			t.Fatal(out.State, len(n.calls))
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 3 || u.LogicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("known copies charge during unknown move then source proofs correct exact total", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(5, 5, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/a", storage.File, "abc")
		n.put("src/b", storage.File, "12")
		n.unknownAt = 4
		a := actionCoordinator(t, p, nil)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev"}, n)
		unknown, err := a.Run(ctx, r, actor, op.ID, n)
		requireCode(t, err, "outcome_unknown")
		if unknown.ChargedLogical != 5 || unknown.ChargedPhysical != 5 {
			t.Fatal(unknown.ChargedLogical, unknown.ChargedPhysical)
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 10 || u.PhysicalBytes != 10 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
		effect := n.calls[3]
		delete(n.entries, effect.Source)
		delete(n.data, effect.Source)
		result := storage.MutationResult{Evidence: []byte("late delete proven terminal")}
		n.receipts[effect.ID] = storage.Inspection{State: storage.Applied, Result: &result, Evidence: result.Evidence}
		out := runAction(t, actionCoordinator(t, NewPostgres(pool), nil), op, actor, n)
		if out.State != Succeeded || out.ChargedLogical != 0 || out.ChargedPhysical != 0 {
			t.Fatal(out.State, out.ChargedLogical, out.ChargedPhysical)
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 5 || u.PhysicalBytes != 5 || u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("lost native response and index failure resume without repeated effects", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.File, "abc")
		n.lostAt = 1
		injected := errors.New("index unavailable")
		failing := true
		commit := func(_ context.Context, _ pgx.Tx, op Operation, _ *storage.MutationResult) (json.RawMessage, error) {
			if op.State != Succeeded {
				t.Fatal("callback did not see terminal state")
			}
			if failing {
				return nil, injected
			}
			return json.RawMessage(`{"path":"/dst"}`), nil
		}
		a := actionCoordinator(t, p, commit)
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "copy", From: "src", To: "dst", IfRevision: "rev"}, n)
		_, err := a.Run(ctx, r, actor, op.ID, n)
		if !errors.Is(err, injected) {
			t.Fatal(err)
		}
		saved, err := p.Get(ctx, r, actor, op.ID)
		if err != nil || saved.Phase != NativeApplied || saved.Terminal() {
			t.Fatal(saved.Phase, err)
		}
		failing = false
		out := runAction(t, actionCoordinator(t, NewPostgres(pool), commit), op, actor, n)
		if out.State != Succeeded || len(n.calls) != 1 || string(out.Result) != `{"path":"/dst"}` {
			t.Fatal(out.State, len(n.calls), string(out.Result))
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 6 || u.LogicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("changed source or parent fails before effect without invented native applied", func(t *testing.T) {
		for _, change := range []string{"source", "parent"} {
			t.Run(change, func(t *testing.T) {
				p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
				n := newActionNative(r)
				n.put("parent", storage.Folder, "")
				n.put("src", storage.File, "abc")
				callback := false
				a := actionCoordinator(t, p, func(_ context.Context, _ pgx.Tx, op Operation, result *storage.MutationResult) (json.RawMessage, error) {
					callback = true
					if result != nil || op.Phase == NativeApplied {
						t.Fatal("invented native effect")
					}
					return nil, nil
				})
				op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "copy", From: "src", To: "parent/dst", IfRevision: "rev"}, n)
				if change == "source" {
					n.put("src", storage.File, "xyz")
				} else {
					n.put("parent", storage.Folder, "")
				}
				out := runAction(t, a, op, actor, n)
				if out.State != Failed || len(n.calls) != 0 || !callback || !bytes.Contains(out.Problem, []byte("target_changed")) {
					t.Fatal(out.State, len(n.calls), callback, string(out.Problem))
				}
				if u := mustUsage(t, p, r, actor); u.LogicalReserved != 0 || u.PhysicalReserved != 0 || u.LogicalBytes != 3 {
					t.Fatal(u)
				}
			})
		}
	})
	t.Run("interrupted preparation releases unused guard after lease fencing", func(t *testing.T) {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
				p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
				n := newActionNative(r)
				req := ActionRequest{Root: r, Actor: actor, Kind: "mkdir", To: "interrupted", IdempotencyKey: "interrupted"}
				intent, _ := json.Marshal(req)
				op := mustBegin(t, p, BeginRequest{Root: r, Actor: actor, Kind: req.Kind, IdempotencyKey: req.IdempotencyKey, Intent: intent, Guards: []Guard{{Path: actor.Scope, Subtree: true}}})
				preparing := mustClaim(t, p, op, actor)
				if cancelled {
					if _, err := p.RequestCancel(ctx, r, actor, op.ID); err != nil {
						t.Fatal(err)
					}
				}
				expireLease(t, pool, op.ID)
				out := runAction(t, actionCoordinator(t, NewPostgres(pool), nil), op, actor, n)
				expected := Failed
				if cancelled {
					expected = Cancelled
				}
				if out.State != expected || out.Phase != Settled || out.NativeResult != nil || len(n.calls) != 0 || n.lists != 0 || !bytes.Contains(out.Problem, []byte("prepare_interrupted")) {
					t.Fatal(out.State, out.Phase, string(out.Problem))
				}
				requireCode(t, p.FreezeAction(ctx, preparing.Lease, actionBinding{}, nil, ReservationRequest{}), "lease_lost")
				mustBegin(t, p, genericRequest(r, actor, "future", 0))
				if u := mustUsage(t, p, r, actor); u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
					t.Fatal(u)
				}
			})
		}
	})
	t.Run("explicit recursive delete and existing mkdir no op", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/a", storage.File, "abc")
		a := actionCoordinator(t, p, nil)
		_, _, err := a.Prepare(ctx, ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "src"}, n)
		requireCode(t, err, "directory_not_empty")
		op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "mkdir", To: "src"}, n)
		out := runAction(t, a, op, actor, n)
		if out.State != Succeeded || len(n.calls) != 0 {
			t.Fatal(out.State, len(n.calls))
		}
	})
}

func TestActionGuardsFenceAffectedPaths(t *testing.T) {
	actor := Actor{ID: "principal", CanWrite: true}
	cases := []struct {
		name string
		req  ActionRequest
		want []Guard
	}{
		{"file delete", ActionRequest{Kind: "delete", From: "a/b.bin"}, []Guard{{Path: "a/b.bin"}}},
		{"recursive delete", ActionRequest{Kind: "delete", From: "a", Recursive: true}, []Guard{{Path: "a", Subtree: true}}},
		{"move", ActionRequest{Kind: "move", From: "a", To: "b"}, []Guard{{Path: "a", Subtree: true}, {Path: "b", Subtree: true}}},
		{"copy", ActionRequest{Kind: "copy", From: "a", To: "b"}, []Guard{{Path: "a", Subtree: true}, {Path: "b", Subtree: true}}},
		{"mkdir", ActionRequest{Kind: "mkdir", To: "new"}, []Guard{{Path: "new", Subtree: true}}},
		// Missing ancestors are only discovered after admission, so this case keeps
		// the whole-scope fence that the frozen manifest relies on.
		{"mkdir with parents", ActionRequest{Kind: "mkdir", To: "new/deep", Parents: true}, []Guard{{Path: "", Subtree: true}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.Actor = actor
			got := actionGuards(c.req)
			if len(got) != len(c.want) {
				t.Fatalf("guards = %+v, want %+v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("guards = %+v, want %+v", got, c.want)
				}
			}
		})
	}
}

// A failed operation must say which entry stopped it and why. The operation row is
// the only place that survives: by the time anyone asks, the log line is gone, and a
// code plus "did not complete" leaves them with a debugging session instead of an
// answer. The cause is rendered by type, so a provider's response body never becomes
// client-visible text.
func TestActionFailureProblemNamesTheStoppingEntryAndItsCause(t *testing.T) {
	pool := isolatedPostgres(t)
	p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
	n := newActionNative(r)
	n.put("victims", storage.Folder, "")
	n.put("victims/a", storage.File, "abc")
	n.failAt = 1
	n.failWith = storage.NewError("not_found", "victims/a", errors.New("provider response body"))
	a := actionCoordinator(t, p, nil)
	op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "copy", From: "victims", To: "copied", IfRevision: "captured"}, n)
	out := runAction(t, a, op, actor, n)
	if out.State != Failed {
		t.Fatalf("state=%s problem=%s", out.State, out.Problem)
	}
	problem := string(out.Problem)
	t.Logf("problem=%s", problem)
	for _, want := range []string{"storage not_found", "had been applied"} {
		if !strings.Contains(problem, want) {
			t.Fatalf("problem %s does not carry %q", problem, want)
		}
	}
	if strings.Contains(problem, "provider response body") {
		t.Fatalf("provider text reached the client-visible detail: %s", problem)
	}
}

// Accepting a recursive delete must not walk the subtree: the index settlement makes it
// final, and the manifest of objects is nobody's business but the purge's. Walking here
// made the confirmation proportional to the folder — 20s for 48 subfolders — for a set
// the client never needed to see.
func TestDeleteAcceptsWithoutWalkingThenPurges(t *testing.T) {
	pool := isolatedPostgres(t)
	p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
	n := newActionNative(r)
	n.put("victims", storage.Folder, "")
	n.put("victims/a", storage.File, "abc")
	prefixHost = n
	defer func() { prefixHost = nil }()
	a := actionCoordinator(t, p, nil)
	req := ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "victims", Recursive: true, IdempotencyKey: "delete-prefix"}
	op, replayed, err := a.Prepare(context.Background(), req, n)
	if err != nil || replayed {
		t.Fatalf("prepare replay=%v: %v", replayed, err)
	}
	if n.lists != 0 {
		t.Fatalf("acceptance walked the subtree: %d listings", n.lists)
	}
	if op.Terminal() || op.Purge == nil || op.Purge.Path != "victims" {
		t.Fatalf("acceptance did not settle a prefix purge: %+v", op)
	}
	if _, err = a.journal.ActionBinding(context.Background(), op); !IsCode(err, "manifest_not_frozen") {
		t.Fatalf("acceptance froze a manifest: %v", err)
	}
	out := runAction(t, a, op, actor, n)
	if out.State != Succeeded {
		t.Fatalf("purge state=%s problem=%s", out.State, out.Problem)
	}
	if n.purges == 0 {
		t.Fatal("purge used the journalled removal instead of the settled one")
	}
	if _, ok := n.data["victims/a"]; ok {
		t.Fatal("purge left the object behind")
	}
	if out.Purge.Deleted == 0 {
		t.Fatal("purge did not report what it deleted")
	}
	// A second pass is recovery: it re-walks and finds nothing, which is success rather
	// than a failure to rebind a manifest that no longer exists.
	again := runAction(t, a, out, actor, n)
	if again.State != Succeeded {
		t.Fatalf("recovery state=%s problem=%s", again.State, again.Problem)
	}
	if _, ok := n.data["victims/a"]; ok {
		t.Fatal("recovery resurrected data")
	}
}

// A path that does not exist still answers synchronously: acceptance observes the
// target, so a typo is a 404 rather than an accepted operation that fails later.
func TestDeleteAcceptanceStillResolvesTheTarget(t *testing.T) {
	pool := isolatedPostgres(t)
	p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
	n := newActionNative(r)
	prefixHost = n
	defer func() { prefixHost = nil }()
	a := actionCoordinator(t, p, nil)
	_, _, err := a.Prepare(context.Background(), ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "absent", Recursive: true}, n)
	if !storage.IsCode(err, "not_found") {
		t.Fatalf("missing target accepted: %v", err)
	}
}

// A path the index holds again was re-created after the deletion was accepted. The purge
// must leave it, and it must leave the folder above it too: deleting the marker would
// orphan the user's new content under a path that no longer exists.
func TestPurgeKeepsAPathRecreatedDuringThePurge(t *testing.T) {
	pool := isolatedPostgres(t)
	p, r, actor := seedOwner(t, pool, provenBaseline(4, 4, 100, 100))
	n := newActionNative(r)
	n.put("victims", storage.Folder, "")
	n.put("victims/a", storage.File, "abc")
	n.put("victims/recreated", storage.File, "xyz")
	a := actionCoordinator(t, p, nil)
	op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "victims", Recursive: true, IdempotencyKey: "delete-recreated"}, n)
	liveOverride[storage.Path("victims/recreated")] = true
	defer delete(liveOverride, storage.Path("victims/recreated"))
	out := runAction(t, a, op, actor, n)
	if out.State != Succeeded || out.Purge.Adopted == 0 {
		t.Fatalf("state=%s adopted=%d", out.State, out.Purge.Adopted)
	}
	if n.data["victims/recreated"] != "xyz" {
		t.Fatal("purge deleted a re-created path")
	}
	if _, ok := n.data["victims/a"]; ok {
		t.Fatal("purge left a stale sibling")
	}
	if _, ok := n.entries["victims"]; !ok {
		t.Fatal("purge removed the folder above a surviving path")
	}
}
