package filechanges

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Root binding uses a separate advisory lock so producers can acquire shared
// root protection BEFORE index locks without acquiring the short ordering head.
// Reset acquires it exclusively. The salt separates this lock from other users
// of PostgreSQL's advisory-lock namespace; hash collisions only reduce concurrency.
const sharedRootLockSQL = `select pg_advisory_xact_lock_shared(hashtextextended('eam-file-change-root-v1:' || $1::text,0))`
const exclusiveRootLockSQL = `select pg_advisory_xact_lock(hashtextextended('eam-file-change-root-v1:' || $1::text,0))`

func validNamespace(value string) bool {
	return value != "" && len(value) <= 16384 && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

// BindRoot acquires shared root protection in the caller's transaction, ensures
// the head and binds an empty namespace exactly once. Existing roots MUST match;
// it never follows configuration changes automatically. Call before acquiring
// index write locks, and before Append's ordering-head lock. After the initial
// binding, it takes no head write lock. Keep native I/O outside this transaction.
// All producers and root API admission paths must participate in this protocol.
func BindRoot(ctx context.Context, tx pgx.Tx, owner, namespace string) (Head, error) {
	if tx == nil || !validUUID(owner) || !validNamespace(namespace) {
		return Head{}, ErrInvalidChange
	}
	if _, err := tx.Exec(ctx, sharedRootLockSQL, owner); err != nil {
		return Head{}, err
	}
	head, err := EnsureHead(ctx, tx, owner)
	if err != nil {
		return Head{}, err
	}
	if head.RootNamespace == namespace {
		return head, nil
	}
	if head.RootNamespace != "" {
		return Head{}, ErrRootChanged
	}
	// Only first binding serializes on the head; every competing first binder
	// is still before index locks. Never write an already-bound head on this path.
	if _, err = tx.Exec(ctx, `update file_change_heads set root_namespace=$2,updated_at=clock_timestamp()
 where owner_id=$1::uuid and root_namespace=''`, owner, namespace); err != nil {
		return Head{}, err
	}
	head, err = ReadHead(ctx, tx, owner)
	if err != nil {
		return Head{}, err
	}
	if head.RootNamespace != namespace {
		return Head{}, ErrRootChanged
	}
	return head, nil
}

// RequireRoot is a read-path check and does not create or bind anything. Pass the
// same transaction used for the feed window; it holds shared reset protection
// until transaction end. A pgxpool.Pool call is only a point-in-time check.
// Repeatable-read callers must establish their root protection before relying on
// a previously captured snapshot; this cannot refresh an existing MVCC snapshot.
func RequireRoot(ctx context.Context, q Queryer, owner, namespace string) error {
	if !validUUID(owner) || !validNamespace(namespace) {
		return ErrInvalidChange
	}
	var ignored any
	if err := q.QueryRow(ctx, sharedRootLockSQL, owner).Scan(&ignored); err != nil {
		return err
	}
	head, err := ReadHead(ctx, q, owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRootChanged
	}
	if err != nil {
		return err
	}
	if head.RootNamespace != namespace {
		return ErrRootChanged
	}
	return nil
}

// BeginRootRead acquires session-level shared root protection BEFORE BeginTx,
// so a repeatable-read snapshot cannot precede a reset it waited for. Callers
// still RequireRoot inside the returned transaction to verify the namespace.
// release is idempotent, rolls back an unfinished transaction, unlocks the same
// session and releases its pool connection. Failed unlock destroys the connection
// rather than returning a held advisory lock to the pool.
func BeginRootRead(ctx context.Context, pool *pgxpool.Pool, owner string, opts pgx.TxOptions) (pgx.Tx, func(), error) {
	if pool == nil || !validUUID(owner) {
		return nil, nil, ErrInvalidChange
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err = conn.Exec(ctx, `select pg_advisory_lock_shared(hashtextextended('eam-file-change-root-v1:' || $1::text,0))`, owner); err != nil {
		// A lost reply may conceal lock acquisition; close instead of pooling it.
		raw := conn.Hijack()
		cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer done()
		_ = raw.Close(cleanup)
		return nil, nil, err
	}
	var tx pgx.Tx
	var once sync.Once
	release := func() {
		once.Do(func() {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer done()
			if tx != nil {
				_ = tx.Rollback(cleanup)
			}
			var unlocked bool
			if err := conn.QueryRow(cleanup, `select pg_advisory_unlock_shared(hashtextextended('eam-file-change-root-v1:' || $1::text,0))`, owner).Scan(&unlocked); err != nil || !unlocked {
				raw := conn.Hijack()
				_ = raw.Close(cleanup)
				return
			}
			conn.Release()
		})
	}
	tx, err = conn.BeginTx(ctx, opts)
	if err != nil {
		release()
		return nil, nil, err
	}
	return tx, release, nil
}

// ResetRoot is an explicit administrative boundary, never ordinary request
// admission. The caller must quiesce native writers and configured applications,
// then pass only the intended current configuration's namespace. This function
// additionally rejects ANY current guard or nonterminal operation for the owner.
// It does not claim database checks can fence rclone/provider requests.
//
// The exclusive root advisory lock precedes the ordering head. Positions and
// effect receipts survive; a fresh epoch and retained floor make all old cursors
// unusable, including an A -> B -> A configuration cycle. All snapshots and
// subscriptions expire, recursive/namespace markers clear. Even a same-namespace
// reset rotates epoch (required after a restore with restored APP_SECRET/epoch).
// Never lower/reuse positions or delete dedup receipts during reset.
func ResetRoot(ctx context.Context, tx pgx.Tx, owner, namespace string) (Head, error) {
	if tx == nil || !validUUID(owner) || !validNamespace(namespace) {
		return Head{}, ErrInvalidChange
	}
	if _, err := tx.Exec(ctx, exclusiveRootLockSQL, owner); err != nil {
		return Head{}, err
	}
	if _, err := EnsureHead(ctx, tx, owner); err != nil {
		return Head{}, err
	}
	var position int64
	if err := tx.QueryRow(ctx, `select head_position from file_change_heads where owner_id=$1::uuid for update`, owner).Scan(&position); err != nil {
		return Head{}, err
	}
	var busy bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from file_operation_guards where owner_id=$1::uuid)
 or exists(select 1 from file_operations where owner_id=$1::uuid and
 state not in ('succeeded','failed','cancelled'))`, owner).Scan(&busy); err != nil {
		return Head{}, err
	}
	if busy {
		return Head{}, ErrRootBusy
	}
	if _, err := tx.Exec(ctx, `update file_change_heads set root_namespace=$2,root_epoch=gen_random_uuid(),
 retained_after=head_position,retained_since=greatest(retained_since,clock_timestamp()),updated_at=clock_timestamp()
 where owner_id=$1::uuid`, owner, namespace); err != nil {
		return Head{}, err
	}
	for _, statement := range []string{
		`update file_subscriptions set expires_at=least(expires_at,clock_timestamp()) where owner_id=$1::uuid`,
		`update file_change_snapshots set expires_at=least(expires_at,clock_timestamp()) where owner_id=$1::uuid`,
		`delete from file_change_markers where owner_id=$1::uuid`,
		`delete from file_change_namespace_markers where owner_id=$1::uuid`,
	} {
		if _, err := tx.Exec(ctx, statement, owner); err != nil {
			return Head{}, err
		}
	}
	return ReadHead(ctx, tx, owner)
}
