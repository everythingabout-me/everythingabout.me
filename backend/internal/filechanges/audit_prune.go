package filechanges

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Audit retention.
//
// file_audit is the owner-facing record behind GET /api/audit: one admission row and
// one completion row per authenticated file request, plus one confirmed row per
// settled effect (and one per entry a sync removed from the index). Nothing prunes it
// on its own, because retention is an operator policy, not a property of the code: the
// rows answer "who touched my files, under which credential, was it allowed, and what
// did it do", and a service upgrade must not silently shorten that memory.
//
// PruneAuditBefore is the explicit command's engine. The rule, and why each clause is
// what it is:
//
//   - observed_at < cutoff, never <=: a row observed exactly at the cutoff is the first
//     row the operator named as retained, so the boundary is unambiguous and a re-run
//     with the same cutoff is idempotent.
//   - one owner per invocation: file_audit carries owner_id and the reader is
//     owner-scoped, so a maintenance command must never cross that boundary.
//   - no other column participates. outcome/action are not part of retention: an owner
//     asking why an upload was denied needs the denied row as much as the completed one.
//
// Ordering and batching use the (owner_id, observed_at desc, id desc) index the reader
// already needs, so no second index is added: the candidate scan is the same range the
// reader pages over, and the delete is by primary key. Each batch is its own
// transaction, so an interrupted run loses at most the batch in flight and a re-run
// continues where the previous one stopped. Deleting audit rows has no dependent row
// anywhere (no foreign key points at file_audit and effect receipts live in file_effects),
// so no owner lock or settlement ordering is required.
const auditPruneBatch = 1000

// AuditPruneDB is the database surface audit retention needs: a pool for the batch
// transactions and a single-row read for the dry run.
type AuditPruneDB interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// PruneAuditBefore deletes this owner's audit rows observed strictly before cutoff and
// reports how many it deleted. With dryRun it reports the same count and writes
// nothing. A malformed owner or a zero cutoff is rejected rather than defaulted: a
// default here would let a typo prune the audit trail.
func PruneAuditBefore(ctx context.Context, db AuditPruneDB, owner string, cutoff time.Time, dryRun bool) (int64, error) {
	if db == nil || !validUUID(owner) || cutoff.IsZero() {
		return 0, ErrInvalidAudit
	}
	at := cutoff.UTC()
	if dryRun {
		var count int64
		if err := db.QueryRow(ctx, `select count(*) from file_audit where owner_id=$1::uuid and observed_at<$2`, owner, at).Scan(&count); err != nil {
			return 0, fmt.Errorf("count expired file audit rows: %w", err)
		}
		return count, nil
	}
	var deleted int64
	for {
		batch, err := pruneAuditBatch(ctx, db, owner, at)
		if err != nil {
			return deleted, err
		}
		deleted += batch
		if batch == 0 {
			return deleted, nil
		}
	}
}

// pruneAuditBatch deletes at most auditPruneBatch rows and returns how many it removed.
// The candidate select and the delete share one transaction: a row selected for
// deletion cannot change its owner or time in between, and SKIP LOCKED lets a second
// concurrent prune (a retry after an interrupted run) take different rows instead of
// blocking or deleting the same batch twice.
func pruneAuditBatch(ctx context.Context, db AuditPruneDB, owner string, cutoff time.Time) (int64, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin audit prune batch: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	rows, err := tx.Query(ctx, `select id::text from file_audit
 where owner_id=$1::uuid and observed_at<$2
 order by observed_at,id limit $3 for update skip locked`, owner, cutoff, auditPruneBatch)
	if err != nil {
		return 0, fmt.Errorf("select expired file audit rows: %w", err)
	}
	ids := make([]string, 0, auditPruneBatch)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read expired file audit row: %w", err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("read expired file audit rows: %w", err)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `delete from file_audit where owner_id=$1::uuid and id=any($2::uuid[]) and observed_at<$3`, owner, ids, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete expired file audit rows: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit audit prune batch: %w", err)
	}
	return tag.RowsAffected(), nil
}
