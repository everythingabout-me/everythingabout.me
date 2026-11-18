package fileops

import (
	"fmt"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

// A move applies one dependency level at a time, not one entry at a time: the
// provider round trips inside a level overlap, while a folder still exists before
// anything is copied into it. The fake refuses a copy whose parent is missing, so a
// successful move is also the order proof.
func TestMoveAppliesOneLevelConcurrently(t *testing.T) {
	pool := isolatedPostgres(t)
	p, r, actor := seedOwner(t, pool, provenBaseline(64, 64, 1000, 1000))
	n := newActionNative(r)
	n.put("src", storage.Folder, "")
	n.put("src/sub", storage.Folder, "")
	for i := 0; i < 24; i++ {
		n.put(storage.Path(fmt.Sprintf("src/sub/f%02d", i)), storage.File, "x")
	}
	a := actionCoordinator(t, p, nil)
	op := prepareAction(t, a, ActionRequest{Root: r, Actor: actor, Kind: "move", From: "src", To: "dst", IfRevision: "rev", IdempotencyKey: "parallel"}, n)
	out := runAction(t, a, op, actor, n)
	if out.State != Succeeded {
		t.Fatalf("move failed: %s %s", out.State, out.Problem)
	}
	if n.maxInflight.Load() < 2 {
		t.Fatalf("entries never overlapped: max in flight %d", n.maxInflight.Load())
	}
	if n.data["dst/sub/f00"] != "x" {
		t.Fatalf("copy did not land: %v", n.data)
	}
	if _, ok := n.entries["src"]; ok {
		t.Fatal("source folder survived the move")
	}
}
