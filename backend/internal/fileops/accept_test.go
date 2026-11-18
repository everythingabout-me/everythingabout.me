package fileops

import (
	"context"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// Accept must answer before the manifest exists, and Run must freeze the same
// manifest Prepare would have frozen. This is the shape a large async move depends
// on: the walk is the operation's work, not the request's.
func TestAcceptDefersTheManifest(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, provenBaseline(4, 4, 100, 100))
	n := newActionNative(r)
	n.put("src", storage.Folder, "")
	n.put("src/a", storage.File, "abc")
	n.put("src/b", storage.File, "12")
	a := actionCoordinator(t, p, nil)

	op, replayed, err := a.Accept(ctx, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev", IdempotencyKey: "accept-deferred"}, n)
	if err != nil || replayed {
		t.Fatalf("accept replay=%v: %v", replayed, err)
	}
	if op.Terminal() || op.EntryTotal() != 0 {
		t.Fatalf("accepted operation is already planned: state=%s entries=%d", op.State, op.EntryTotal())
	}
	if _, err = p.ActionBinding(ctx, op); !IsCode(err, "manifest_not_frozen") {
		t.Fatalf("accepted operation already has a frozen manifest: %v", err)
	}
	if len(n.calls) != 0 {
		t.Fatalf("accept issued %d native effect(s)", len(n.calls))
	}

	out := runAction(t, a, op, actor, n)
	if out.State != Succeeded || n.data["dst/a"] != "abc" || n.data["dst/b"] != "12" {
		t.Fatalf("run after accept: %s %v", out.State, n.data)
	}
	if _, err = p.ActionBinding(ctx, out); err != nil {
		t.Fatalf("frozen manifest missing after run: %v", err)
	}
	if out.EntryTotal() == 0 {
		t.Fatal("settled move reports no entries")
	}
}

// An early 202 must not turn a request that cannot succeed into an accepted
// operation that fails later: what the caller can still fix is answered here.
func TestAcceptRefusesWhatTheCallerCanFix(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	p, r, actor := seedOwner(t, pool, provenBaseline(4, 4, 100, 100))
	n := newActionNative(r)
	n.put("src", storage.Folder, "")
	n.put("src/a", storage.File, "abc")
	n.put("taken", storage.File, "x")
	a := actionCoordinator(t, p, nil)

	for _, test := range []struct {
		name string
		from storage.Path
		to   storage.Path
		code string
	}{
		{"occupied destination", "src", "taken", "name_conflict"},
		{"missing source", "nope", "dst", "not_found"},
		{"missing destination parent", "src", "nope/dst", "parent_missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			op, _, err := a.Accept(ctx, ActionRequest{Root: r, Actor: actor, Kind: "move", From: test.from, To: test.to, IfRevision: "rev", IdempotencyKey: "accept-" + test.name}, n)
			requireCode(t, err, test.code)
			if op.State != Failed {
				t.Fatalf("state after refusal: %s", op.State)
			}
		})
	}
	if len(n.calls) != 0 {
		t.Fatalf("a refused accept issued %d native effect(s)", len(n.calls))
	}
}
