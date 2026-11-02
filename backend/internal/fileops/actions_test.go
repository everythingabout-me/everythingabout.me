package fileops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	beforeStat  func(storage.Path)
	afterEffect func(storage.EffectIntent)
	failAt      int
	unknownAt   int
	lostAt      int
	pageSize    int
	lists       int
}

func newActionNative(root Root) *actionNative {
	n := &actionNative{root: root, entries: map[storage.Path]storage.Entry{}, data: map[storage.Path]string{}, receipts: map[string]storage.Inspection{}, pageSize: 2}
	n.put("", storage.Folder, "")
	return n
}
func (n *actionNative) put(p storage.Path, kind storage.Kind, data string) {
	n.serial++
	size := int64(len(data))
	if kind == storage.Folder {
		size = 0
	}
	n.entries[p] = storage.Entry{Path: p, Kind: kind, Size: size, Version: []byte(strconv.Itoa(n.serial)), NativeRef: []byte(fmt.Sprintf("inode-%d", n.serial))}
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
	if n.beforeStat != nil {
		n.beforeStat(p)
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
func (n *actionNative) Remove(ctx context.Context, m storage.Mutation, save storage.Checkpoint) (storage.MutationResult, error) {
	return n.effect(ctx, storage.EffectIntent{ID: m.ID, Action: "delete", Source: m.Path, SourceVersion: m.Conditions.ExpectedVersion}, save)
}
func (n *actionNative) Copy(ctx context.Context, m storage.MoveRequest, save storage.Checkpoint) (storage.MutationResult, error) {
	return n.effect(ctx, storage.EffectIntent{ID: m.ID, Action: "copy", Source: m.Source, Target: m.Target, SourceVersion: m.SourceVersion, Conditions: m.TargetConditions}, save)
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
		return fail(errors.New("injected native failure"))
	}
	if i.Source != "" {
		e, ok := n.entries[i.Source]
		if !ok || !bytes.Equal(e.Version, i.SourceVersion) {
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
	a, err := NewActions(p, ActionsOptions{Authorize: func(context.Context, Operation) error { return nil }, Validate: func(context.Context, NativeActions, ActionRequest) error { return nil }, Commit: commit})
	if err != nil {
		t.Fatal(err)
	}
	return a
}
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
	t.Run("delete never rescans external additions and reports charged partial", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(3, 3, 100, 100))
		n := newActionNative(r)
		n.put("src", storage.Folder, "")
		n.put("src/a", storage.File, "abc")
		a := actionCoordinator(t, p, nil)
		req := ActionRequest{Root: r, Actor: actor, Kind: "delete", From: "src", Recursive: true, IdempotencyKey: "delete"}
		op := prepareAction(t, a, req, n)
		lists := n.lists
		n.afterEffect = func(i storage.EffectIntent) {
			if i.Action == "delete" && i.Source == "src/a" {
				n.put("src/new", storage.File, "outside")
			}
		}
		out := runAction(t, a, op, actor, n)
		if out.State != Failed || !explicitRemaining(out.Result) || n.data["src/new"] != "outside" || n.lists != lists {
			t.Fatal(out.State, string(out.Result), n.lists, lists)
		}
		if u := mustUsage(t, p, r, actor); u.LogicalBytes != 0 || u.PhysicalBytes != 0 || u.LogicalReserved != 0 {
			t.Fatal(u)
		}
		// The outside addition is outside the baseline; it remains unaccounted pending
		// explicit import, while only the captured known bytes receive credit here.
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
		_, _, err = p.Begin(ctx, genericRequest(r, actor, "unrelated", 0))
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
