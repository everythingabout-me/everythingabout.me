package fileops

import (
	"context"
	"encoding/json"
	"testing"

	"everythingabout.me/backend/internal/storage"
)

func TestIdentityReplayClientIntent(t *testing.T) {
	legacy := storage.WriteIntent{Target: "folder/file", Size: 7, ClientCondition: "create:default"}
	original, err := clientUploadFingerprint(legacy)
	if err != nil {
		t.Fatal(err)
	}
	observed := legacy
	observed.Selection, observed.EntryID, observed.ParentID = "path", "later-entry", "later-parent"
	got, err := clientUploadFingerprint(observed)
	if err != nil || got != original {
		t.Fatalf("path alias changed legacy fingerprint: %v", err)
	}
	for _, tc := range []struct {
		mode          string
		first, next   storage.Path
		entry, parent string
	}{
		{"id", "before/file", "after/file", "selected-file", "captured-parent"},
		{"parentId", "before/file", "after/file", "captured-file", "selected-parent"},
	} {
		a, b := legacy, legacy
		a.Selection, a.Target, a.EntryID, a.ParentID = tc.mode, tc.first, tc.entry, tc.parent
		b.Selection, b.Target, b.EntryID, b.ParentID = tc.mode, tc.next, tc.entry, tc.parent
		x, _ := clientUploadFingerprint(a)
		y, _ := clientUploadFingerprint(b)
		if x != y {
			t.Fatalf("%s retry depended on current path", tc.mode)
		}
		if tc.mode == "id" {
			b.EntryID = "other-id"
		} else {
			b.ParentID = "other-parent"
		}
		y, _ = clientUploadFingerprint(b)
		if x == y {
			t.Fatalf("%s client identity omitted selected ID", tc.mode)
		}
	}
	// A create that asked for missing ancestors stores the connection's ImplicitParents
	// answer as Parents, but its replay runs before resolution and computes the flag as
	// false. Admission evidence must not split a fingerprint: the stored intent that an
	// admitted session carries and the client request that replays it are the same intent.
	admitted := legacy
	admitted.Parents = true
	replayed := legacy
	storedPrint, err := clientUploadFingerprint(admitted)
	if err != nil {
		t.Fatal(err)
	}
	replayedPrint, err := clientUploadFingerprint(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if storedPrint != replayedPrint {
		t.Fatal("implicit-parents admission changed the client fingerprint")
	}
	base := ActionRequest{Kind: "move", From: "before/file", To: "dest/file", IfRevision: "nr_original"}
	captured := base
	captured.Selection, captured.EntryID, captured.ParentID = "path:path", "captured-source", "captured-parent"
	if !actionClientEqual(base, captured) {
		t.Fatal("captured path bindings changed legacy action identity")
	}
	captured.Selection = "id:path"
	same := captured
	same.From = same.To
	if !actionClientEqual(captured, same) {
		t.Fatal("ID move retry depended on its moved source path")
	}
	same.EntryID = "different-entry"
	if actionClientEqual(captured, same) {
		t.Fatal("different source ID reused action intent")
	}
	captured.Selection = "id:parentId"
	same = captured
	same.To = "moved-parent/file"
	if !actionClientEqual(captured, same) {
		t.Fatal("parent ID retry depended on its moved path")
	}
	same.To = "moved-parent/other-name"
	if actionClientEqual(captured, same) {
		t.Fatal("different destination name reused action intent")
	}
}

func TestIdentityReplayPostgres(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("ID action retries precede new native path constraints", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		native := newActionNative(r)
		native.put("Scope", storage.Folder, "")
		native.put("Scope/old", storage.File, "")
		actions := actionCoordinator(t, p, nil)
		req := ActionRequest{Root: r, Actor: actor, Kind: "move", From: "Scope/old", To: "Scope/new", IfRevision: "nr_saved", EntryID: "selected-entry", ParentID: "captured-parent", Selection: "id:path", IdempotencyKey: "stable-move"}
		op := prepareAction(t, actions, req, native)
		done := runAction(t, actions, op, actor, native)
		calls := len(native.calls)
		req.From = req.To
		replay, found, err := actions.Prepare(ctx, req, native)
		if err != nil || !found || replay.ID != done.ID {
			t.Fatalf("moved ID replay: %v %v", found, err)
		}
		req.From = ""
		replay, found, err = actions.Prepare(ctx, req, native)
		if err != nil || !found || replay.ID != done.ID || len(native.calls) != calls {
			t.Fatalf("retired ID replay: %v %v", found, err)
		}
		req.EntryID = "other-entry"
		_, _, err = actions.Prepare(ctx, req, native)
		requireCode(t, err, "idempotency_conflict")
		req.EntryID = "selected-entry"
		req.Actor.Scope = "Elsewhere"
		_, _, err = actions.Prepare(ctx, req, native)
		requireCode(t, err, "forbidden")
	})
	t.Run("pending replay checks original binding before native effects", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		native := newActionNative(r)
		native.put("old", storage.File, "")
		actions := actionCoordinator(t, p, nil)
		req := ActionRequest{Root: r, Actor: actor, Kind: "move", From: "old", To: "new", IfRevision: "nr_saved", EntryID: "selected-entry", Selection: "id:path", IdempotencyKey: "pending-move"}
		prepareAction(t, actions, req, native)
		actions.options.Authorize = func(context.Context, Operation) error { return E("target_changed", nil) }
		req.From = "later-location"
		_, _, err := actions.Prepare(ctx, req, native)
		requireCode(t, err, "target_changed")
		if len(native.calls) != 0 {
			t.Fatal("changed identity replay emitted a native effect")
		}
	})
	t.Run("write and upload replay bind selected IDs rather than location", func(t *testing.T) {
		p, r, actor := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		req, opt := uploadRequest(r, actor, "Scope/original", 0, "upload-id")
		opt.ClientIntent.Selection, opt.ClientIntent.EntryID = "id", "selected-entry"
		var captured storage.WriteIntent
		if err := json.Unmarshal(req.Intent, &captured); err != nil {
			t.Fatal(err)
		}
		captured.Selection, captured.EntryID, captured.ParentID = "id", "selected-entry", "captured-parent"
		req.Intent, _ = json.Marshal(captured)
		u, _, err := p.CreateUpload(ctx, req, opt)
		if err != nil {
			t.Fatal(err)
		}
		retry := opt.ClientIntent
		retry.Target = ""
		saved, found, err := p.ReplayUpload(ctx, r, actor, "upload-id", retry)
		if err != nil || !found || saved.ID != u.ID {
			t.Fatalf("raw ID upload replay: %v %v", found, err)
		}
		retry.EntryID = "other-entry"
		_, _, err = p.ReplayUpload(ctx, r, actor, "upload-id", retry)
		requireCode(t, err, "idempotency_conflict")
		req.IdempotencyKey = "put-id"
		captured.Target = "Scope/put"
		req.Intent, _ = json.Marshal(captured)
		req.Guards = []Guard{{Path: captured.Target}}
		op, _, err := p.Begin(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		retry = opt.ClientIntent
		retry.Target = "Scope/moved"
		old, found, err := p.ReplayWrite(ctx, r, actor, "put-id", retry)
		if err != nil || !found || old.ID != op.ID {
			t.Fatalf("moved ID PUT replay: %v %v", found, err)
		}
		retry.Selection = "path"
		retry.Target = captured.Target
		_, _, err = p.ReplayWrite(ctx, r, actor, "put-id", retry)
		requireCode(t, err, "idempotency_conflict")
	})
}
