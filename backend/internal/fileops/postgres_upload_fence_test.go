package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"everythingabout.me/backend/internal/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func guardRows(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `select count(*) from file_operation_guards where operation_id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// subtreeRequest is an action that fences everything under path, the way a move, a copy
// or a recursive delete does.
func subtreeRequest(r Root, a Actor, path storage.Path) BeginRequest {
	req := genericRequest(r, a, path, 0)
	req.Guards = []Guard{{Path: path, Subtree: true}}
	return req
}

func releaseWithoutEffect(t *testing.T, p *Postgres, op Operation, a Actor) {
	t.Helper()
	claimed := mustClaim(t, p, op, a)
	if err := p.FinishWithoutEffect(context.Background(), claimed.Lease, Cancelled, nil); err != nil {
		t.Fatal(err)
	}
}

func savedProblem(t *testing.T, op Operation) (code, detail string) {
	t.Helper()
	var saved struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(op.Problem, &saved); err != nil {
		t.Fatalf("operation problem %q: %v", op.Problem, err)
	}
	return saved.Code, saved.Detail
}

func TestPostgresUploadFence(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("an open session fences nothing and takes its guard when it freezes", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/file", 4, "")
		if n := guardRows(t, pool, u.ID); n != 0 {
			t.Fatalf("an open session holds %d guard rows", n)
		}
		// Anything may happen above the destination while the session is only staging bytes.
		above := mustBegin(t, p, subtreeRequest(r, a, "folder"))
		mustReceipt(t, p, r, a, u, 1, 4)
		// The commit is the first moment the session can change the tree, so it is the
		// moment it must not overlap that operation — and the refusal names the holder.
		_, err := p.FreezeUpload(ctx, r, a, u.ID, nil)
		requireCode(t, err, "operation_in_progress")
		var held *Error
		if !errors.As(err, &held) || held.OperationID != above.ID || held.OperationKind != "copy" {
			t.Fatalf("freeze refusal lost its holder: %+v", held)
		}
		// Refused, not ended: the holder may fail and leave everything where it was.
		if still, err := p.GetUpload(ctx, r, a, u.ID); err != nil || still.State != UploadOpen || guardRows(t, pool, u.ID) != 0 {
			t.Fatalf("a refused freeze changed the session: %+v %v", still, err)
		}
		releaseWithoutEffect(t, p, above, a)
		frozen, err := p.FreezeUpload(ctx, r, a, u.ID, nil)
		if err != nil || frozen.State != UploadCommitting {
			t.Fatalf("freeze after the holder ended: %+v %v", frozen, err)
		}
		if n := guardRows(t, pool, u.ID); n != 1 {
			t.Fatalf("a committing session holds %d guard rows, want 1", n)
		}
		// From here on the publication is fenced like any other native effect.
		_, _, err = p.Begin(ctx, subtreeRequest(r, a, "folder"))
		requireCode(t, err, "operation_in_progress")
		if !errors.As(err, &held) || held.OperationID != u.ID || held.OperationKind != "upload_commit" {
			t.Fatalf("action refusal lost the committing upload: %+v", held)
		}
		// A repeated commit is the same commit, not a second guard.
		if again, err := p.FreezeUpload(ctx, r, a, u.ID, nil); err != nil || again.State != UploadCommitting || guardRows(t, pool, u.ID) != 1 {
			t.Fatalf("freeze replay: %+v %v", again, err)
		}
	})
	t.Run("a destination that changed ends the session at freeze", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/gone", 4, "")
		mustReceipt(t, p, r, a, u, 1, 4)
		// A check that could not answer is not a verdict about the destination.
		unavailable := errors.New("index unavailable")
		_, err := p.FreezeUpload(ctx, r, a, u.ID, func(context.Context, pgx.Tx, Operation) error { return unavailable })
		if !errors.Is(err, unavailable) {
			t.Fatalf("binding failure was swallowed: %v", err)
		}
		if still, err := p.GetUpload(ctx, r, a, u.ID); err != nil || still.State != UploadOpen {
			t.Fatalf("an unanswered binding check ended the session: %+v %v", still, err)
		}
		var asked string
		failed, err := p.FreezeUpload(ctx, r, a, u.ID, func(_ context.Context, _ pgx.Tx, op Operation) error {
			asked = op.ID
			return E("parent_missing", errors.New(`the destination folder "/folder" no longer exists`))
		})
		if err != nil || asked != u.ID {
			t.Fatalf("freeze with a broken binding: asked=%s %v", asked, err)
		}
		if failed.State != UploadFailed || failed.Operation.State != Failed || failed.Operation.FinishedAt == nil {
			t.Fatalf("a session that can never commit stayed alive: %+v", failed)
		}
		code, detail := savedProblem(t, failed.Operation)
		if code != "parent_missing" || !strings.Contains(detail, `"/folder" no longer exists`) || !strings.Contains(detail, "Start a new upload") {
			t.Fatalf("stored reason: %s %q", code, detail)
		}
		// Nothing was published, so nothing stays fenced and the quota is free at once;
		// the staged bytes keep their physical reservation until cleanup discards them.
		if n := guardRows(t, pool, u.ID); n != 0 {
			t.Fatalf("a failed freeze left %d guard rows", n)
		}
		if usage := mustUsage(t, p, r, a); usage.LogicalReserved != 0 || usage.PhysicalReserved != 24 {
			t.Fatalf("reservation after a failed freeze: %+v", usage)
		}
		status, err := p.GetUpload(ctx, r, a, u.ID)
		if err != nil || status.State != UploadFailed {
			t.Fatalf("status after a failed freeze: %+v %v", status, err)
		}
		_, err = p.FreezeUpload(ctx, r, a, u.ID, nil)
		requireCode(t, err, "upload_gone")
		candidates, err := p.CleanupCandidates(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, op := range candidates {
			found = found || op.ID == u.ID
		}
		if !found {
			t.Fatal("the failed session's staged bytes are not scheduled for cleanup")
		}
	})
	t.Run("a new session for the same path supersedes the open one", func(t *testing.T) {
		// The limit admits one 8-byte session, not two: the retry fits only because the
		// session it replaces gives its reservation back in the same transaction.
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 10, 1000))
		older := mustUpload(t, p, r, a, "folder/one", 8, "first")
		mustReceipt(t, p, r, a, older, 1, 4)
		// Another principal with write access to the path may displace it too.
		token := Actor{ID: "token:one", CanRead: true, CanWrite: true}
		newer := mustUpload(t, p, r, token, "folder/one", 8, "second")
		ended, err := p.GetUpload(ctx, r, a, older.ID)
		if err != nil || ended.State != UploadAborted || ended.Operation.State != Cancelled {
			t.Fatalf("superseded session: %+v %v", ended, err)
		}
		code, detail := savedProblem(t, ended.Operation)
		if code != "upload_superseded" || !strings.Contains(detail, newer.ID) {
			t.Fatalf("the superseded session does not name its successor: %s %q", code, detail)
		}
		if usage := mustUsage(t, p, r, a); usage.LogicalReserved != 8 {
			t.Fatalf("both sessions hold quota: %+v", usage)
		}
		_, _, err = p.AcceptUploadPart(ctx, r, a, older.ID, receipt(2, 4))
		requireCode(t, err, "upload_gone")
		// The older client's own key still answers with its own, now terminal, session.
		req, opt := uploadRequest(r, a, "folder/one", 8, "first")
		replayed, replay, err := p.CreateUpload(ctx, req, opt)
		if err != nil || !replay || replayed.ID != older.ID || replayed.State != UploadAborted {
			t.Fatalf("superseded replay: %+v %v %v", replayed, replay, err)
		}
		// A creation that is not admitted ends nobody.
		req, opt = uploadRequest(r, a, "folder/one", 20, "third")
		_, _, err = p.CreateUpload(ctx, req, opt)
		requireCode(t, err, "quota_exceeded")
		if alive, err := p.GetUpload(ctx, r, token, newer.ID); err != nil || alive.State != UploadOpen {
			t.Fatalf("a refused creation ended the open session: %+v %v", alive, err)
		}
	})
	t.Run("a replacement supersedes the session holding the same credit", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(8, 8, 100, 1000))
		version := []byte("observed-revision")
		create := func(key string) Upload {
			t.Helper()
			client := storage.WriteIntent{Target: "folder/doc", Size: 6, ClientCondition: "replace:cr_observed", MediaType: "text/plain"}
			captured := client
			captured.Conditions = storage.Conditions{ExpectedRevision: version}
			captured.ParentPath = "folder"
			intent, _ := json.Marshal(captured)
			u, replay, err := p.CreateUpload(ctx, BeginRequest{Root: r, Actor: a, Kind: "upload_commit", IdempotencyKey: key, Intent: intent,
				Reservation: ReservationRequest{NewBytes: 6, OldBytes: 8, CreditPath: "folder/doc", CreditVersion: version, PhysicalBytes: 28}}, UploadOptions{PartSize: 4, ClientIntent: client})
			if err != nil || replay {
				t.Fatalf("replacement session %s: replay=%v %v", key, replay, err)
			}
			return u
		}
		first := create("first")
		// The credit for the replaced revision is exclusive. It used to refuse this second
		// session with credit_conflict, which names no holder at all.
		second := create("second")
		var holder string
		if err := pool.QueryRow(ctx, `select operation_id::text from file_credit_claims where owner_id=$1`, r.OwnerID).Scan(&holder); err != nil || holder != second.ID {
			t.Fatalf("credit claim holder %s, want %s: %v", holder, second.ID, err)
		}
		if ended, err := p.GetUpload(ctx, r, a, first.ID); err != nil || ended.State != UploadAborted {
			t.Fatalf("first replacement session: %+v %v", ended, err)
		}
	})
	t.Run("a committing session is not superseded", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/busy", 4, "")
		mustReceipt(t, p, r, a, u, 1, 4)
		if _, err := p.FreezeUpload(ctx, r, a, u.ID, nil); err != nil {
			t.Fatal(err)
		}
		// Its publication may already be native, so the newer attempt waits for it or adopts
		// it; the refusal has to say which operation that is.
		req, opt := uploadRequest(r, a, "folder/busy", 4, "again")
		_, _, err := p.CreateUpload(ctx, req, opt)
		requireCode(t, err, "operation_in_progress")
		var held *Error
		if !errors.As(err, &held) || held.OperationID != u.ID || held.OperationKind != "upload_commit" {
			t.Fatalf("creation refusal lost the committing upload: %+v", held)
		}
		if still, err := p.GetUpload(ctx, r, a, u.ID); err != nil || still.State != UploadCommitting {
			t.Fatalf("committing session was altered: %+v %v", still, err)
		}
	})
	t.Run("concurrent creations leave one open session", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 10000))
		const clients = 8
		start := make(chan struct{})
		errs := make(chan error, clients)
		var wg sync.WaitGroup
		for i := 0; i < clients; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				req, opt := uploadRequest(r, a, "folder/contended", 4, fmt.Sprintf("client-%d", i))
				_, _, err := p.CreateUpload(ctx, req, opt)
				errs <- err
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("a creation lost to a session that had published nothing: %v", err)
			}
		}
		var open, total int
		if err := pool.QueryRow(ctx, `select count(*) filter(where state='open'),count(*) from file_uploads where owner_id=$1 and comparison_path='folder/contended'`, r.OwnerID).Scan(&open, &total); err != nil || open != 1 || total != clients {
			t.Fatalf("open=%d total=%d: %v", open, total, err)
		}
		if usage := mustUsage(t, p, r, a); usage.LogicalReserved != 4 {
			t.Fatalf("superseded sessions kept quota: %+v", usage)
		}
	})
	t.Run("a guard left by a session opened under create-time fencing fences nothing", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 1000))
		u := mustUpload(t, p, r, a, "folder/legacy", 4, "")
		// What such a session wrote when it was created, before guards moved to the commit.
		if _, err := pool.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree) values($1,$2,0,'folder/legacy','folder/legacy',false)`, u.ID, r.OwnerID); err != nil {
			t.Fatal(err)
		}
		above := mustBegin(t, p, subtreeRequest(r, a, "folder"))
		releaseWithoutEffect(t, p, above, a)
		mustReceipt(t, p, r, a, u, 1, 4)
		frozen, err := p.FreezeUpload(ctx, r, a, u.ID, nil)
		if err != nil || frozen.State != UploadCommitting || guardRows(t, pool, u.ID) != 1 {
			t.Fatalf("freeze over a create-time guard row: %+v rows=%d %v", frozen, guardRows(t, pool, u.ID), err)
		}
		// Once it commits, the same row is a real fence.
		_, _, err = p.Begin(ctx, subtreeRequest(r, a, "folder"))
		requireCode(t, err, "operation_in_progress")
	})
}
