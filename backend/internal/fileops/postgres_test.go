package fileops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"everythingabout.me/backend/internal/storage"
	"everythingabout.me/backend/internal/testpg"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EAM_TEST_DATABASE_URL must name a disposable PostgreSQL service with CREATEDB.
// Every test run creates a random isolated database; no .env is read or live schema
// reused. All repository migrations run twice in the new database before tests.
func isolatedPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	scratch := testpg.CreateDatabase(t, "eam_fileops_")
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(scratch.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, err := filepath.Glob("../../../sql/*.sql")
	if err != nil || len(files) < 17 {
		t.Fatalf("migration discovery: %d %v", len(files), err)
	}
	for pass := 0; pass < 2; pass++ {
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = pool.Exec(ctx, string(b)); err != nil {
				t.Fatalf("migration %s pass %d: %v", file, pass+1, err)
			}
		}
	}
	t.Logf("PostgreSQL isolated database %s: all %d migrations applied twice", scratch.Name, len(files))
	return pool
}
func seedOwner(t *testing.T, pool *pgxpool.Pool, base Baseline) (*Postgres, Root, Actor) {
	t.Helper()
	r := testRoot()
	r.OwnerID = newID()
	a := testActor(r)
	_, err := pool.Exec(context.Background(), `insert into users(id,email,password_hash) values($1,$2,'test-only')`, r.OwnerID, r.OwnerID+"@example.test")
	if err != nil {
		t.Fatal(err)
	}
	p := NewPostgres(pool)
	if err = p.Initialize(context.Background(), r, base); err != nil {
		t.Fatal(err)
	}
	return p, r, a
}
func provenBaseline(logical, physical, limit, physicalLimit int64) Baseline {
	return Baseline{LogicalBytes: logical, PhysicalBytes: physical, LogicalLimit: limit, PhysicalLimit: physicalLimit, NativeComplete: true, IndexCompared: true, LegacyWritersDrained: true, Evidence: json.RawMessage(`{"native_scan":"isolated-fixture","drain":"verified"}`)}
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	if !IsCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}
func mustBegin(t *testing.T, p *Postgres, req BeginRequest) Operation {
	t.Helper()
	op, replay, err := p.Begin(context.Background(), req)
	if err != nil || replay {
		t.Fatalf("begin replay=%v: %v", replay, err)
	}
	return op
}
func mustClaim(t *testing.T, p *Postgres, op Operation, a Actor) Operation {
	t.Helper()
	out, err := p.Claim(context.Background(), op.Root, a, op.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustEntries loads an operation's manifest the way a reader that needs the list does.
// After Release B a Get returns no list for a table-backed row, so a test that checks the
// manifest asks the journal for it instead of reading it off the operation.
func mustEntries(t *testing.T, p *Postgres, op Operation) []EntryEffect {
	t.Helper()
	entries, err := p.Entries(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func mustUsage(t *testing.T, p *Postgres, r Root, a Actor) Usage {
	t.Helper()
	u, err := p.Usage(context.Background(), r, a)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
func expireLease(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	// One timestamp expression feeds both the column and the document: two calls would
	// leave the row's column and its document a few microseconds apart, which the journal
	// treats as a disagreement and repairs on the next write.
	_, err := pool.Exec(context.Background(), `update file_operations set lease_expires_at=stamp.at,
 snapshot=jsonb_set(snapshot,'{Lease,ExpiresAt}',to_jsonb(stamp.at))
 from (select clock_timestamp()-interval '1 second' as at) stamp where id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
}
func applied(t *testing.T, p *Postgres, op Operation) {
	t.Helper()
	if err := p.StartEffect(context.Background(), op.Lease); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveApplied(context.Background(), op.Lease, storage.MutationResult{Evidence: []byte("terminal fixture effect")}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresJournal(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	t.Run("explicit baseline and immutable root", func(t *testing.T) {
		r := testRoot()
		r.OwnerID = newID()
		a := testActor(r)
		_, err := pool.Exec(ctx, `insert into users(id,email,password_hash) values($1,$2,'test')`, r.OwnerID, r.OwnerID+"@example.test")
		if err != nil {
			t.Fatal(err)
		}
		p := NewPostgres(pool)
		_, _, err = p.Begin(ctx, genericRequest(r, a, "new", 1))
		requireCode(t, err, "baseline_required")
		b := provenBaseline(0, 0, 10, 20)
		bad := b
		bad.NativeComplete = false
		requireCode(t, p.Initialize(ctx, r, bad), "baseline_required")
		bad = b
		bad.Evidence = json.RawMessage(`{}`)
		requireCode(t, p.Initialize(ctx, r, bad), "baseline_required")
		if err = p.Initialize(ctx, r, b); err != nil {
			t.Fatal(err)
		}
		op := mustBegin(t, p, genericRequest(r, a, "new", 3))
		if err = p.Initialize(ctx, r, b); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalReserved != 3 {
			t.Fatal("initialize reset counters")
		}
		changed := b
		changed.LogicalBytes = 1
		requireCode(t, p.Initialize(ctx, r, changed), "baseline_conflict")
		rotated := r
		rotated.Generation++
		requireCode(t, p.Initialize(ctx, rotated, b), "root_conflict")
		_, _, err = p.Begin(ctx, genericRequest(rotated, a, "new", 0))
		requireCode(t, err, "root_conflict")
		op = mustClaim(t, p, op, a)
		if err = p.FinishWithoutEffect(ctx, op.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.Initialize(ctx, rotated, b), "root_conflict")
	})
	t.Run("concurrent logical and physical admission", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 10, 100))
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make(chan error, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, _, err := p.Begin(ctx, genericRequest(r, a, storage.Path(fmt.Sprintf("item-%d", i)), 1))
				results <- err
			}(i)
		}
		close(start)
		wg.Wait()
		close(results)
		accepted, rejected := 0, 0
		for err := range results {
			if err == nil {
				accepted++
			} else if IsCode(err, "quota_exceeded") {
				rejected++
			} else {
				t.Fatal(err)
			}
		}
		if accepted != 10 || rejected != 10 {
			t.Fatalf("accepted %d rejected %d", accepted, rejected)
		}
		if u := mustUsage(t, p, r, a); u.LogicalReserved != 10 {
			t.Fatal(u)
		}
		p, r, a = seedOwner(t, pool, provenBaseline(0, 0, 0, 5))
		mustBegin(t, p, genericRequest(r, a, "one", 4))
		_, _, err := p.Begin(ctx, genericRequest(r, a, "two", 2))
		requireCode(t, err, "staging_full")
	})
	t.Run("idempotency and result retention after deletion", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 10, 100))
		a.ID = "token:first"
		req := genericRequest(r, a, "deleted/path", 0)
		req.Kind = "delete"
		req.IdempotencyKey = "delete-key"
		req.Intent = json.RawMessage(`{"b":2,"a":1}`)
		op := mustBegin(t, p, req)
		canonical := req
		canonical.Intent = json.RawMessage(`{"a":1, "b":2}`)
		again, replay, err := p.Begin(ctx, canonical)
		if err != nil || !replay || again.ID != op.ID {
			t.Fatalf("replay %v %v", replay, err)
		}
		changed := req
		changed.Reservation.NewBytes = 1
		_, _, err = p.Begin(ctx, changed)
		requireCode(t, err, "idempotency_conflict")
		changed = req
		changed.Root.Generation++
		_, _, err = p.Begin(ctx, changed)
		requireCode(t, err, "idempotency_conflict")
		other := a
		other.ID = "token:other"
		other.ManageOwner = true
		_, err = p.Get(ctx, r, other, op.ID)
		requireCode(t, err, "forbidden")
		other = a
		other.CanRead = false
		other.CanWrite = false
		_, err = p.Get(ctx, r, other, op.ID)
		requireCode(t, err, "forbidden")
		other = a
		other.Scope = "elsewhere"
		_, err = p.Get(ctx, r, other, op.ID)
		requireCode(t, err, "forbidden")
		wrong := r
		wrong.OwnerID = newID()
		_, err = p.Get(ctx, wrong, a, op.ID)
		requireCode(t, err, "not_found")
		owner := testActor(r)
		owner.ManageOwner = true
		if _, err = p.Get(ctx, r, owner, op.ID); err != nil {
			t.Fatal(err)
		}
		op = mustClaim(t, p, op, a)
		applied(t, p, op)
		s := Settlement{State: Succeeded, Result: json.RawMessage(`{"deleted":true}`)}
		settled, err := p.Settle(ctx, op.Lease, s, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := p.Get(ctx, r, a, op.ID)
		if err != nil || !got.Terminal() || !equalJSON(got.Result, json.RawMessage(`{"deleted":true}`)) {
			t.Fatalf("retained delete %s %v", got.Result, err)
		}
		again, replay, err = p.Begin(ctx, canonical)
		if err != nil || !replay || again.ID != op.ID || !again.Terminal() {
			t.Fatal("terminal identity not replayed", err)
		}
		if settled.RetainUntil.Sub(*settled.FinishedAt) < 24*time.Hour {
			t.Fatal("retention shorter than 24 hours")
		}
		if _, err = p.Settle(ctx, op.Lease, s, nil); err != nil {
			t.Fatal("repeat settlement", err)
		}
	})
	t.Run("guard normalization ancestor conflict and permanence", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		r.CaseSensitive = false
		// Establish the desired case rules before this owner's baseline exists.
		_, err := pool.Exec(ctx, `delete from file_budgets where owner_id=$1`, r.OwnerID)
		if err != nil {
			t.Fatal(err)
		}
		if err = p.Initialize(ctx, r, provenBaseline(0, 0, 100, 100)); err != nil {
			t.Fatal(err)
		}
		req := genericRequest(r, a, "Cafe\u0301 /File", 2)
		op := mustBegin(t, p, req)
		_, _, err = p.Begin(ctx, genericRequest(r, a, "CAFÉ /file", 0))
		requireCode(t, err, "operation_in_progress")
		var conflict *Error
		if !errors.As(err, &conflict) || conflict.OperationID != op.ID {
			t.Fatalf("guard conflict lost exact operation: %v", err)
		}
		ancestor := genericRequest(r, a, "Café ", 0)
		ancestor.Guards[0].Subtree = true
		_, _, err = p.Begin(ctx, ancestor)
		requireCode(t, err, "operation_in_progress")
		op = mustClaim(t, p, op, a)
		if err = p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if err = p.MarkUnknown(ctx, op.Lease, []byte("native request may arrive")); err != nil {
			t.Fatal(err)
		}
		expireLease(t, pool, op.ID)
		restarted := NewPostgres(pool)
		claimed, err := restarted.Claim(ctx, r, a, op.ID, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if claimed.State != Unknown || claimed.Lease.Epoch != op.Lease.Epoch+1 {
			t.Fatal(claimed)
		}
		requireCode(t, restarted.StartEffect(ctx, claimed.Lease), "recovery_required")
		requireCode(t, p.SaveCheckpoint(ctx, op.Lease, []byte("late")), "lease_lost")
		requireCode(t, p.SaveApplied(ctx, op.Lease, storage.MutationResult{}), "lease_lost")
		_, _, err = p.Begin(ctx, genericRequest(r, a, "CAFÉ /file", 0))
		requireCode(t, err, "operation_in_progress")
		requireCode(t, p.FinishWithoutEffect(ctx, claimed.Lease, Cancelled, nil), "recovery_required")
		_, err = p.Settle(ctx, claimed.Lease, Settlement{State: Cancelled}, nil)
		requireCode(t, err, "recovery_required")
		cancel, err := p.RequestCancel(ctx, r, a, op.ID)
		if err != nil || !cancel.CancelRequested || cancel.State != Unknown {
			t.Fatalf("cancel changed unknown: %v", err)
		}
		if err = p.SaveApplied(ctx, claimed.Lease, storage.MutationResult{Evidence: []byte("terminal verified")}); err != nil {
			t.Fatal(err)
		}
		if _, err = p.Settle(ctx, claimed.Lease, Settlement{State: Succeeded, LogicalDelta: 2, PhysicalDelta: 2}, nil); err != nil {
			t.Fatal(err)
		}
		mustBegin(t, p, genericRequest(r, a, "CAFÉ /file", 0))
	})
	t.Run("lease clock fences and checkpoint history", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		op := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "file", 1)), a)
		if err := p.SaveCheckpoint(ctx, op.Lease, []byte("allocation-one")); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveCheckpoint(ctx, op.Lease, []byte("allocation-two")); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := pool.QueryRow(ctx, `select count(*) from file_operation_checkpoints where operation_id=$1`, op.ID).Scan(&count); err != nil || count != 2 {
			t.Fatalf("checkpoint history %d %v", count, err)
		}
		saved, err := p.Get(ctx, r, a, op.ID)
		if err != nil || saved.Phase != Receiving {
			t.Fatal(saved.Phase, err)
		}
		forged := op.Lease
		forged.Token = "forged"
		_, err = p.Renew(ctx, forged, time.Minute)
		requireCode(t, err, "lease_lost")
		clientExpiry := op.Lease
		clientExpiry.ExpiresAt = time.Now().Add(-time.Hour)
		renewed, err := p.Renew(ctx, clientExpiry, time.Minute)
		if err != nil || renewed.ExpiresAt.Before(time.Now()) {
			t.Fatalf("trusted client clock: %v", err)
		}
		expireLease(t, pool, op.ID)
		_, err = p.Renew(ctx, renewed, time.Minute)
		requireCode(t, err, "lease_lost")
		requireCode(t, p.SaveCheckpoint(ctx, renewed, nil), "lease_lost")
		op = mustClaim(t, p, op, a)
		if err = p.FinishWithoutEffect(ctx, op.Lease, Cancelled, nil); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("immutable stage and transactional index failure", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		req := genericRequest(r, a, "uploaded", 4)
		req.Kind = "upload_commit"
		w := storage.WriteIntent{Target: "uploaded", Size: 4, Conditions: storage.Conditions{Absent: true}}
		req.Intent, _ = json.Marshal(w)
		op := mustClaim(t, p, mustBegin(t, p, req), a)
		w.ID = op.ID
		s := storage.Staged{Intent: w, Handle: []byte("private-stage"), Size: 4}
		if err := p.SaveStage(ctx, op.Lease, s); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveStage(ctx, op.Lease, s); err != nil {
			t.Fatal(err)
		}
		changed := s
		changed.Handle = []byte("other")
		requireCode(t, p.SaveStage(ctx, op.Lease, changed), "intent_conflict")
		if err := p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.StartEffect(ctx, op.Lease), "recovery_required")
		if err := p.SaveApplied(ctx, op.Lease, storage.MutationResult{Entry: &storage.Entry{Path: "uploaded", Kind: storage.File, Size: 4}, Evidence: []byte("native fsync")}); err != nil {
			t.Fatal(err)
		}
		_, err := pool.Exec(ctx, `create table if not exists fileops_test_index(id uuid primary key)`)
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("injected index failure")
		bad := func(ctx context.Context, tx pgx.Tx, o Operation, _ *storage.MutationResult) (json.RawMessage, error) {
			_, err := tx.Exec(ctx, `insert into fileops_test_index(id) values($1)`, o.ID)
			if err != nil {
				return nil, err
			}
			return nil, failure
		}
		settlement := Settlement{State: Succeeded, LogicalDelta: 4, PhysicalDelta: 4, Result: json.RawMessage(`{"uploaded":true}`)}
		_, err = p.Settle(ctx, op.Lease, settlement, bad)
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
		var count int
		if err = pool.QueryRow(ctx, `select count(*) from fileops_test_index where id=$1`, op.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("callback transaction did not roll back", err)
		}
		current, err := p.Get(ctx, r, a, op.ID)
		if err != nil || current.Phase != NativeApplied || current.Terminal() {
			t.Fatal("applied outcome lost", err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 0 || u.LogicalReserved != 4 {
			t.Fatal(u)
		}
		good := func(ctx context.Context, tx pgx.Tx, o Operation, _ *storage.MutationResult) (json.RawMessage, error) {
			_, err := tx.Exec(ctx, `insert into fileops_test_index(id) values($1)`, o.ID)
			return json.RawMessage(`{"path":"/uploaded"}`), err
		}
		result, err := p.Settle(ctx, op.Lease, settlement, good)
		if err != nil || string(result.Result) != `{"path":"/uploaded"}` {
			t.Fatal(result, err)
		}
		if _, err = p.Settle(ctx, op.Lease, settlement, good); err != nil {
			t.Fatal("repeat callback was invoked", err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 4 || u.LogicalReserved != 0 || u.PhysicalBytes != 4 {
			t.Fatal(u)
		}
	})
	t.Run("partial manifest evidence and unknown entry", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		req := genericRequest(r, a, "tree", 10)
		req.Guards[0].Subtree = true
		req.Manifest = []ManifestEntry{{Number: 0, Action: "copy", Source: "tree/a", Target: "tree/b", SourceRevision: []byte("v1"), TargetConditions: storage.Conditions{Absent: true}, Size: 5}, {Number: 1, Action: "copy", Source: "tree/c", Target: "tree/d", SourceRevision: []byte("v2"), TargetConditions: storage.Conditions{Absent: true}, Size: 5}}
		op := mustClaim(t, p, mustBegin(t, p, req), a)
		if err := p.BeginEntry(ctx, op.Lease, 0); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.BeginEntry(ctx, op.Lease, 0), "recovery_required")
		if err := p.SaveEntry(ctx, op.Lease, 0, "outcome_unknown", []byte("cp"), []byte("uncertain"), nil); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.SaveApplied(ctx, op.Lease, storage.MutationResult{}), "invalid_transition")
		requireCode(t, p.BeginEntry(ctx, op.Lease, 1), "recovery_required")
		if err := p.SaveEntry(ctx, op.Lease, 0, "applied", []byte("terminal"), []byte("verified"), json.RawMessage(`{"bytes":5}`)); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveApplied(ctx, op.Lease, storage.MutationResult{Evidence: []byte("partial native applied")}); err != nil {
			t.Fatal(err)
		}
		_, err := p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: 5, PhysicalDelta: 5}, nil)
		requireCode(t, err, "invalid_transition")
		result, err := p.Settle(ctx, op.Lease, Settlement{State: Failed, LogicalDelta: 5, PhysicalDelta: 5, Result: json.RawMessage(`{"applied":[0],"remaining":[1]}`)}, nil)
		if err != nil || len(result.Entries) != 2 || result.Entries[0].State != "applied" || result.Entries[1].State != "pending" {
			t.Fatal(result, err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalBytes != 5 || u.LogicalReserved != 0 {
			t.Fatal(u)
		}
	})
	t.Run("replacement credit is exclusive in flight and released at settlement", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(8, 8, 10, 100))
		req := replacementRequest(r, a, "file", 10, 8, []byte("native-v1"))
		op := mustClaim(t, p, mustBegin(t, p, req), a)
		if u := mustUsage(t, p, r, a); u.LogicalReserved != 2 {
			t.Fatal("replacement reserved full size", u)
		}
		_, _, err := p.Begin(ctx, req)
		requireCode(t, err, "operation_in_progress")
		var wi storage.WriteIntent
		if err = json.Unmarshal(req.Intent, &wi); err != nil {
			t.Fatal(err)
		}
		wi.ID = op.ID
		if err = p.SaveStage(ctx, op.Lease, storage.Staged{Intent: wi, Size: wi.Size, Handle: []byte("replacement-stage")}); err != nil {
			t.Fatal(err)
		}
		if err = p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if err = p.SaveApplied(ctx, op.Lease, storage.MutationResult{Entry: &storage.Entry{Path: wi.Target, Size: wi.Size, Kind: storage.File}, Evidence: []byte("terminal replacement")}); err != nil {
			t.Fatal(err)
		}
		_, err = p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: 3, PhysicalDelta: 2}, nil)
		requireCode(t, err, "reservation_exceeded")
		if _, err = p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: 2, PhysicalDelta: 2}, nil); err != nil {
			t.Fatal(err)
		}
		req = replacementRequest(r, a, "file", 8, 8, []byte("native-v1"))
		// The first replacement spent its credit and settled, so the fence is gone
		// (credit.go): identical bytes carry the identical native revision, and a
		// re-import of the same file must be admitted against that revision again.
		if _, replay, err := p.Begin(ctx, req); err != nil || replay {
			t.Fatalf("a settled replacement still fences its revision: replay=%v %v", replay, err)
		}
		fresh := replacementRequest(r, a, "unused", 3, 3, []byte("unused-v1"))
		unused := mustClaim(t, p, mustBegin(t, p, fresh), a)
		if err = p.FinishWithoutEffect(ctx, unused.Lease, Failed, nil); err != nil {
			t.Fatal(err)
		}
		mustBegin(t, p, fresh)
	})
	t.Run("retained liabilities and recorded cleanup", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		op := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "file", 10)), a)
		applied(t, p, op)
		if _, err := p.Settle(ctx, op.Lease, Settlement{State: Failed, LogicalDelta: 3, PhysicalDelta: 3, KeepLogical: 2, KeepPhysical: 4, Result: json.RawMessage(`{"remaining":["cleanup"]}`)}, nil); err != nil {
			t.Fatal(err)
		}
		_, _, err := p.Begin(ctx, genericRequest(r, a, "file", 1))
		requireCode(t, err, "operation_in_progress")
		if err = p.MarkCleaned(ctx, r, a, op.ID); err != nil {
			t.Fatal(err)
		}
		if err = p.MarkCleaned(ctx, r, a, op.ID); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.PhysicalReserved != 0 || u.LogicalReserved != 2 || u.PhysicalBytes != 3 || u.LogicalBytes != 3 {
			t.Fatal(u)
		}
		_, _, err = p.Begin(ctx, genericRequest(r, a, "file", 1))
		requireCode(t, err, "operation_in_progress")
		candidates, err := p.CleanupCandidates(ctx, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, candidate := range candidates {
			if candidate.ID == op.ID {
				t.Fatal("cleaned operation listed")
			}
		}
	})
	t.Run("explicit terminal nonapplication proof", func(t *testing.T) {
		p, r, a := seedOwner(t, pool, provenBaseline(0, 0, 100, 100))
		op := mustClaim(t, p, mustBegin(t, p, genericRequest(r, a, "file", 4)), a)
		if err := p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if err := p.MarkUnknown(ctx, op.Lease, []byte("unknown")); err != nil {
			t.Fatal(err)
		}
		requireCode(t, p.FinishWithoutEffect(ctx, op.Lease, Cancelled, nil), "recovery_required")
		requireCode(t, p.ResolveNotApplied(ctx, op.Lease, Cancelled, nil, nil), "invalid_request")
		if err := p.ResolveNotApplied(ctx, op.Lease, Failed, []byte("all requests terminal; no effect; discarded staging"), json.RawMessage(`{"code":"native_failed"}`)); err != nil {
			t.Fatal(err)
		}
		if u := mustUsage(t, p, r, a); u.LogicalReserved != 0 || u.PhysicalReserved != 0 {
			t.Fatal(u)
		}
		mustBegin(t, p, genericRequest(r, a, "file", 4))
	})
}

// requireUsage checks the two budgets independently: logical bytes follow file sizes,
// physical bytes follow native generations, and a reserved byte is liability still held.
func requireUsage(t *testing.T, p *Postgres, r Root, a Actor, logical, logicalReserved, physical, physicalReserved int64) {
	t.Helper()
	u := mustUsage(t, p, r, a)
	if u.LogicalBytes != logical || u.LogicalReserved != logicalReserved || u.PhysicalBytes != physical || u.PhysicalReserved != physicalReserved {
		t.Fatalf("usage logical=%d+%d physical=%d+%d, want logical=%d+%d physical=%d+%d",
			u.LogicalBytes, u.LogicalReserved, u.PhysicalBytes, u.PhysicalReserved,
			logical, logicalReserved, physical, physicalReserved)
	}
}

// requireClaims counts the credit rows an owner holds, which is what decides whether the
// next operation can claim a revision.
func requireClaims(t *testing.T, pool *pgxpool.Pool, owner string, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(context.Background(), `select count(*) from file_credit_claims where owner_id=$1`, owner).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("credit claims = %d, want %d", got, want)
	}
}

// applyReplacement drives one replacement through the journal the way the upload commit
// does: admit (taking the credit), stage the published bytes, record the native result,
// then settle with the delta the accounting contract derives from the two sizes.
func applyReplacement(t *testing.T, p *Postgres, r Root, a Actor, path storage.Path, newBytes, oldBytes int64, version []byte) Operation {
	t.Helper()
	op := mustClaim(t, p, mustBegin(t, p, replacementRequest(r, a, path, newBytes, oldBytes, version)), a)
	return finishReplacement(t, p, op, newBytes, oldBytes)
}

// finishReplacement completes a claimed replacement: the staged bytes, the native result,
// then the settlement that spends the credit.
func finishReplacement(t *testing.T, p *Postgres, op Operation, newBytes, oldBytes int64) Operation {
	t.Helper()
	ctx := context.Background()
	var intent storage.WriteIntent
	if err := json.Unmarshal(op.Intent, &intent); err != nil {
		t.Fatal(err)
	}
	intent.ID = op.ID
	if err := p.SaveStage(ctx, op.Lease, storage.Staged{Intent: intent, Size: intent.Size, Handle: []byte("stage-" + op.ID)}); err != nil {
		t.Fatal(err)
	}
	if err := p.StartEffect(ctx, op.Lease); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveApplied(ctx, op.Lease, storage.MutationResult{
		Entry:    &storage.Entry{Path: intent.Target, Kind: storage.File, Size: intent.Size},
		Evidence: []byte("native replace " + op.ID),
	}); err != nil {
		t.Fatal(err)
	}
	delta := newBytes - oldBytes
	settled, err := p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: delta, PhysicalDelta: delta}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return settled
}

// deleteRequest admits a deletion action, the shape an accepted delete is stored in.
func deleteRequest(t *testing.T, r Root, a Actor, path storage.Path) BeginRequest {
	t.Helper()
	intent, err := json.Marshal(ActionRequest{Root: r, Actor: a, Kind: "delete", From: path})
	if err != nil {
		t.Fatal(err)
	}
	return BeginRequest{Root: r, Actor: a, Kind: "delete", Intent: intent, Guards: []Guard{{Path: path}}}
}

// freezeDelete takes the deletion credit for one captured source revision. It returns the
// error unmodified so a caller can require the credit_conflict of a second attempt.
func freezeDelete(t *testing.T, p *Postgres, r Root, a Actor, path storage.Path, size int64, version []byte) (Operation, error) {
	t.Helper()
	ctx := context.Background()
	op := mustClaim(t, p, mustBegin(t, p, deleteRequest(t, r, a, path)), a)
	source := storage.Entry{Path: path, Kind: storage.File, Size: size, Revision: version, NativeRef: []byte("inode-" + string(version))}
	target := source
	manifest := []ManifestEntry{{Number: 0, Action: "delete", Source: path, SourceRevision: version, SourceEntry: &target, Size: size}}
	return op, p.FreezeAction(ctx, op.Lease, actionBinding{Sources: []storage.Entry{source}}, manifest, ReservationRequest{OldBytes: size})
}

// appliedDelete proves the entry's native removal and settles the refund it earned.
func appliedDelete(t *testing.T, p *Postgres, op Operation, size int64) {
	t.Helper()
	ctx := context.Background()
	evidence := []byte("native removal verified")
	result, err := json.Marshal(storage.MutationResult{Evidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.BeginEntry(ctx, op.Lease, 0); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveEntry(ctx, op.Lease, 0, "started", []byte("checkpoint"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveEntry(ctx, op.Lease, 0, "applied", []byte("checkpoint"), evidence, result); err != nil {
		t.Fatal(err)
	}
	if err := p.SaveApplied(ctx, op.Lease, storage.MutationResult{Evidence: []byte("subtree removed")}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: -size, PhysicalDelta: -size}, nil); err != nil {
		t.Fatal(err)
	}
}

// TestPostgresCreditClaims pins the lifetime rule in credit.go: a replacement's claim is
// a fence that ends at settlement, a deletion's claim is a receipt that never ends, and
// the quota after each case is exactly the arithmetic the rule promises.
func TestPostgresCreditClaims(t *testing.T) {
	pool := isolatedPostgres(t)
	ctx := context.Background()
	version := []byte("native-etag:unchanged")

	t.Run("identical bytes replace twice in a row", func(t *testing.T) {
		// Arithmetic: 100 B replaced by 100 B reserves max(100-100,0)=0 and settles
		// 100-100=0; doing it twice still leaves exactly the 100 B that is on disk.
		p, r, a := seedOwner(t, pool, provenBaseline(100, 100, 1000, 1000))
		first := applyReplacement(t, p, r, a, "same/file", 100, 100, version)
		if first.State != Succeeded {
			t.Fatalf("first replacement: %+v", first.State)
		}
		requireUsage(t, p, r, a, 100, 0, 100, 0)
		requireClaims(t, pool, r.OwnerID, 0)
		second := applyReplacement(t, p, r, a, "same/file", 100, 100, version)
		if second.State != Succeeded {
			t.Fatalf("second replacement: %+v", second.State)
		}
		requireUsage(t, p, r, a, 100, 0, 100, 0)
		requireClaims(t, pool, r.OwnerID, 0)
	})

	t.Run("different bytes replace twice in a row", func(t *testing.T) {
		// Arithmetic: 100 -> 200 reserves 100 and settles +100; 200 -> 60 reserves 0 and
		// settles -140. The budget ends at 60, exactly the file on disk.
		p, r, a := seedOwner(t, pool, provenBaseline(100, 100, 1000, 1000))
		applyReplacement(t, p, r, a, "diff/file", 200, 100, version)
		requireUsage(t, p, r, a, 200, 0, 200, 0)
		requireClaims(t, pool, r.OwnerID, 0)
		applyReplacement(t, p, r, a, "diff/file", 60, 200, []byte("native-etag:grown"))
		requireUsage(t, p, r, a, 60, 0, 60, 0)
		requireClaims(t, pool, r.OwnerID, 0)
	})

	t.Run("a concurrent replacement of one revision still conflicts", func(t *testing.T) {
		// An open session holds the credit but no path guard (sessions fence at commit).
		// That is exactly the window the claim exists for: two publications of the same
		// old revision could both credit its 100 B.
		p, r, a := seedOwner(t, pool, provenBaseline(100, 100, 1000, 1000))
		client := storage.WriteIntent{Target: "busy/file", Size: 100, ClientCondition: "replace:cr_observed", MediaType: "text/plain"}
		captured := client
		captured.Conditions = storage.Conditions{ExpectedRevision: version}
		intent, err := json.Marshal(captured)
		if err != nil {
			t.Fatal(err)
		}
		session, replay, err := p.CreateUpload(ctx, BeginRequest{Root: r, Actor: a, Kind: "upload_commit", IdempotencyKey: "concurrent-replacement", Intent: intent,
			Reservation: ReservationRequest{NewBytes: 100, OldBytes: 100, CreditPath: "busy/file", CreditVersion: version, PhysicalBytes: 100}},
			UploadOptions{PartSize: 100, ClientIntent: client})
		if err != nil || replay {
			t.Fatalf("replacement session: replay=%v %v", replay, err)
		}
		_, _, err = p.Begin(ctx, replacementRequest(r, a, "busy/file", 100, 100, version))
		requireCode(t, err, "credit_conflict")
		var holder string
		if err := pool.QueryRow(ctx, `select operation_id::text from file_credit_claims where owner_id=$1`, r.OwnerID).Scan(&holder); err != nil || holder != session.ID {
			t.Fatalf("credit holder %q, want the open session %s: %v", holder, session.ID, err)
		}
		requireUsage(t, p, r, a, 100, 0, 100, 100)
	})

	t.Run("a native write that has not settled keeps its fence", func(t *testing.T) {
		// The window between the publication and the settlement is the one a crash lands
		// in. The claim must still be there (and the delta must not be applied) so that
		// recovery settles the same operation, and no second replacement can spend the
		// same revision in the meantime.
		p, r, a := seedOwner(t, pool, provenBaseline(100, 100, 1000, 1000))
		req := replacementRequest(r, a, "crash/file", 100, 100, version)
		op := mustClaim(t, p, mustBegin(t, p, req), a)
		var intent storage.WriteIntent
		if err := json.Unmarshal(req.Intent, &intent); err != nil {
			t.Fatal(err)
		}
		intent.ID = op.ID
		if err := p.SaveStage(ctx, op.Lease, storage.Staged{Intent: intent, Size: intent.Size, Handle: []byte("stage")}); err != nil {
			t.Fatal(err)
		}
		if err := p.StartEffect(ctx, op.Lease); err != nil {
			t.Fatal(err)
		}
		if err := p.SaveApplied(ctx, op.Lease, storage.MutationResult{Entry: &storage.Entry{Path: intent.Target, Kind: storage.File, Size: intent.Size}, Evidence: []byte("published")}); err != nil {
			t.Fatal(err)
		}
		requireClaims(t, pool, r.OwnerID, 1)
		requireUsage(t, p, r, a, 100, 0, 100, 100)
		_, _, err := p.Begin(ctx, replacementRequest(r, a, "crash/file", 100, 100, version))
		requireCode(t, err, "operation_in_progress")
		requireClaims(t, pool, r.OwnerID, 1)
		if _, err = p.Settle(ctx, op.Lease, Settlement{State: Succeeded, LogicalDelta: 0, PhysicalDelta: 0}, nil); err != nil {
			t.Fatal(err)
		}
		requireClaims(t, pool, r.OwnerID, 0)
		requireUsage(t, p, r, a, 100, 0, 100, 0)
		applyReplacement(t, p, r, a, "crash/file", 100, 100, version)
		requireUsage(t, p, r, a, 100, 0, 100, 0)
	})

	t.Run("a settled replacement claim is adopted, not a conflict", func(t *testing.T) {
		// A row written by a binary from before the release rule points at a replacement
		// that already settled. The revision is still current (identical bytes), so the
		// next replacement adopts the row and the last one releases it.
		p, r, a := seedOwner(t, pool, provenBaseline(100, 100, 1000, 1000))
		legacy := applyReplacement(t, p, r, a, "legacy/file", 100, 100, version)
		if _, err := pool.Exec(ctx, `insert into file_credit_claims(owner_id,comparison_path,version,operation_id) values($1,$2,$3,$4)`, r.OwnerID, "legacy/file", version, legacy.ID); err != nil {
			t.Fatal(err)
		}
		requireClaims(t, pool, r.OwnerID, 1)
		req := replacementRequest(r, a, "legacy/file", 100, 100, version)
		next := mustClaim(t, p, mustBegin(t, p, req), a)
		var holder string
		if err := pool.QueryRow(ctx, `select operation_id::text from file_credit_claims where owner_id=$1`, r.OwnerID).Scan(&holder); err != nil || holder != next.ID {
			t.Fatalf("adopted holder %q, want %s: %v", holder, next.ID, err)
		}
		finishReplacement(t, p, next, 100, 100)
		requireClaims(t, pool, r.OwnerID, 0)
	})

	t.Run("a delete credit is a receipt and is never adopted", func(t *testing.T) {
		// Arithmetic: the delete refunds its 100 B once. A second delete of the same
		// revision would subtract bytes that no longer exist, and a replacement must not
		// inherit that receipt either.
		p, r, a := seedOwner(t, pool, provenBaseline(100, 100, 1000, 1000))
		op, err := freezeDelete(t, p, r, a, "gone/file", 100, version)
		if err != nil {
			t.Fatal(err)
		}
		appliedDelete(t, p, op, 100)
		requireUsage(t, p, r, a, 0, 0, 0, 0)
		requireClaims(t, pool, r.OwnerID, 1)
		second, err := freezeDelete(t, p, r, a, "gone/file", 100, version)
		requireCode(t, err, "credit_conflict")
		if err := p.FinishWithoutEffect(ctx, second.Lease, Failed, nil); err != nil {
			t.Fatal(err)
		}
		_, _, err = p.Begin(ctx, replacementRequest(r, a, "gone/file", 100, 100, version))
		requireCode(t, err, "credit_conflict")
		requireUsage(t, p, r, a, 0, 0, 0, 0)
		requireClaims(t, pool, r.OwnerID, 1)
	})
}
