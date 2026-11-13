package filechanges

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func bindCommitted(t *testing.T, pool *pgxpool.Pool, owner, namespace string) Head {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	head, err := BindRoot(ctx, tx, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return head
}
func resetCommitted(t *testing.T, pool *pgxpool.Pool, owner, namespace string) Head {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	head, err := ResetRoot(ctx, tx, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return head
}

// beginOpen starts a transaction the test commits or rolls back explicitly, and registers a
// safety rollback. Without it a failing assertion exits the test while the transaction still
// holds its connection, so changeDatabase's pool.Close waits forever at cleanup and the real
// error is never printed. A test that can hang the package on its own failure hides exactly
// the bug it was written to report.
func beginOpen(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func TestPostgresRootBindingAndReset(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	owner, other := newOwner(t, pool), newOwner(t, pool)
	initial := bindCommitted(t, pool, owner, "root:A")
	otherInitial := bindCommitted(t, pool, other, "root:other")
	if initial.RootNamespace != "root:A" || initial.Position != 0 || initial.RootEpoch == "" {
		t.Fatalf("initial binding %+v", initial)
	}
	if again := bindCommitted(t, pool, owner, "root:A"); again.RootEpoch != initial.RootEpoch {
		t.Fatal("ordinary binding rotated epoch")
	}
	tx := beginOpen(t, pool)
	var err error
	if _, err = BindRoot(ctx, tx, owner, "root:B"); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("automatic rebind accepted: %v", err)
	}
	if err = RequireRoot(ctx, tx, owner, "root:B"); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("mismatch read accepted: %v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	first := createChange(owner, "root-A-effect", "/Notes/a.txt")
	position := appendCommitted(t, pool, first)
	tx = beginOpen(t, pool)
	if err = MarkChanged(ctx, tx, owner, []string{testEntry}, position, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `insert into file_change_namespace_markers(owner_id,entry_id,position,change_token,last_changed_at)
 values($1::uuid,$2::uuid,$3,gen_random_uuid(),clock_timestamp())`, owner, testEntry, position); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `insert into file_subscriptions(owner_id,principal_id,root_namespace,epoch,policy_fingerprint,mode,selector_path,anchor_path,kind,start_position,signal_position)
 values($1::uuid,'owner-test','root:A',$2::uuid,'test','path','/','/','folder',$3,$3)`, owner, initial.RootEpoch, position); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `insert into file_change_snapshots(owner_id,principal_id,root_namespace,policy_fingerprint,epoch,head_position,selector_mode,selector_path,target_id,anchor_path,kind,entry_count,changes_cursor)
 values($1::uuid,'owner-test','root:A','test',$2::uuid,$3,'path','/',$4::uuid,'/','folder',1,'opaque-test')`, owner, initial.RootEpoch, position, testEntry); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// A reset is transactional: a dry run must not rotate or clear any state.
	tx = beginOpen(t, pool)
	if _, err = ResetRoot(ctx, tx, owner, "root:B"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	still, err := ReadHead(ctx, pool, owner)
	if err != nil || still.RootEpoch != initial.RootEpoch {
		t.Fatalf("dry reset changed binding %+v %v", still, err)
	}
	resetB := resetCommitted(t, pool, owner, "root:B")
	if resetB.RootEpoch == initial.RootEpoch || resetB.Position != position || resetB.RetainedAfter != position || resetB.RootNamespace != "root:B" {
		t.Fatalf("reset failed to fence old stream %+v", resetB)
	}
	for _, table := range []string{"file_change_markers", "file_change_namespace_markers"} {
		var count int
		if err = pool.QueryRow(ctx, `select count(*) from `+table+` where owner_id=$1::uuid`, owner).Scan(&count); err != nil || count != 0 {
			t.Fatalf("reset retained %s count=%d %v", table, count, err)
		}
	}
	for _, table := range []string{"file_subscriptions", "file_change_snapshots"} {
		var active int
		if err = pool.QueryRow(ctx, `select count(*) from `+table+` where owner_id=$1::uuid and expires_at>clock_timestamp()`, owner).Scan(&active); err != nil || active != 0 {
			t.Fatalf("reset retained active %s %d %v", table, active, err)
		}
	}
	tx = beginOpen(t, pool)
	if _, err = BindRoot(ctx, tx, owner, "root:A"); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("old producer rebound rootA: %v", err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if again := bindCommitted(t, pool, owner, "root:B"); again.RootEpoch != resetB.RootEpoch {
		t.Fatal("valid current producer changed epoch")
	}
	second := createChange(owner, "root-B-effect", "/Notes/b.txt")
	if got := appendCommitted(t, pool, second); got != position+1 {
		t.Fatalf("reset reused sequence %d", got)
	}
	resetA := resetCommitted(t, pool, owner, "root:A")
	if resetA.RootEpoch == initial.RootEpoch || resetA.RootEpoch == resetB.RootEpoch || resetA.RetainedAfter != position+1 {
		t.Fatalf("A B A revived original cursor %+v", resetA)
	}
	if replay := appendCommitted(t, pool, first); replay != position {
		t.Fatalf("reset lost durable receipt %d", replay)
	}
	head, err := ReadHead(ctx, pool, owner)
	if err != nil || head.Position != position+1 {
		t.Fatalf("receipt replay advanced reset head %+v %v", head, err)
	}
	// A restore can retain APP_SECRET and namespace. Explicit same-root reset is
	// still required, and rotates the epoch rather than claiming automatic proof.
	restored := resetCommitted(t, pool, owner, "root:A")
	if restored.RootEpoch == resetA.RootEpoch {
		t.Fatal("same-root restore reset retained old epoch")
	}
	otherHead, err := ReadHead(ctx, pool, other)
	if err != nil || otherHead.RootEpoch != otherInitial.RootEpoch || otherHead.RootNamespace != "root:other" {
		t.Fatalf("reset crossed owners %+v %v", otherHead, err)
	}
}

func TestPostgresResetRejectsOutstandingAuthority(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	initial := bindCommitted(t, pool, owner, "root:A")
	operationID := testUUID(t)
	if _, err := pool.Exec(ctx, `insert into file_operations(id,owner_id,principal_id,action,fingerprint,snapshot)
 values($1::uuid,$2::uuid,'owner-test','move','test','{"State":"outcome_unknown"}'::jsonb)`, operationID, owner); err != nil {
		t.Fatal(err)
	}
	assertBusy := func() {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = ResetRoot(ctx, tx, owner, "root:B"); !errors.Is(err, ErrRootBusy) {
			t.Fatalf("unsafe reset error %v", err)
		}
	}
	assertBusy()
	if _, err := pool.Exec(ctx, `update file_operations set state='succeeded', phase='settled', snapshot='{"State":"succeeded","Phase":"settled"}'::jsonb where id=$1::uuid`, operationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into file_operation_guards(operation_id,owner_id,ordinal,native_path,comparison_path,subtree)
 values($1::uuid,$2::uuid,0,'Notes','Notes',true)`, operationID, owner); err != nil {
		t.Fatal(err)
	}
	assertBusy()
	if _, err := pool.Exec(ctx, `delete from file_operation_guards where operation_id=$1::uuid`, operationID); err != nil {
		t.Fatal(err)
	}
	reset := resetCommitted(t, pool, owner, "root:B")
	if reset.RootEpoch == initial.RootEpoch {
		t.Fatal("safe reset failed to rotate epoch")
	}
}

func TestPostgresRootReadsWaitBeforeRepeatableSnapshot(t *testing.T) {
	pool := changeDatabase(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	initial := bindCommitted(t, pool, owner, "root:A")
	reset, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer reset.Rollback(ctx)
	if _, err = ResetRoot(ctx, reset, owner, "root:B"); err != nil {
		t.Fatal(err)
	}
	var reader pgx.Tx
	var release func()
	acquired := make(chan error, 1)
	go func() {
		var err error
		reader, release, err = BeginRootRead(ctx, pool, owner, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		acquired <- err
	}()
	// Confirm the dedicated read connection actually waits on advisory lock,
	// rather than relying on goroutine scheduling or elapsed-time guesses.
	waitForRootReader(t, pool)
	if err = reset.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("root reader did not resume")
	}
	defer release()
	if err = RequireRoot(ctx, reader, owner, "root:B"); err != nil {
		t.Fatal(err)
	}
	if err = RequireRoot(ctx, reader, owner, "root:A"); !errors.Is(err, ErrRootChanged) {
		t.Fatalf("reader captured pre-reset namespace: %v", err)
	}
	head, err := ReadHead(ctx, reader, owner)
	if err != nil || head.RootEpoch == initial.RootEpoch {
		t.Fatalf("repeatable read captured old epoch %+v %v", head, err)
	}
	if err = reader.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	release()
	release()
	// Cleanup may run with a cancelled original request and still releases its
	// session lock/connection. A subsequent reset proves the lock is gone.
	cancelled, cancel := context.WithCancel(ctx)
	tx, cleanup, err := BeginRootRead(cancelled, pool, owner, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	if err = RequireRoot(cancelled, tx, owner, "root:B"); err != nil {
		t.Fatal(err)
	}
	cancel()
	cleanup()
	resetCommitted(t, pool, owner, "root:C")
}

func waitForRootReader(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity where datname=current_database()
 and pid<>pg_backend_pid() and wait_event_type='Lock' and wait_event='advisory' and query like '%pg_advisory_lock_shared%')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("root reader never waited on reset")
		case <-ticker.C:
		}
	}
}
